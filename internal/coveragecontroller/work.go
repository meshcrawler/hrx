package coveragecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"

	"github.com/meshcrawler/hrx/internal/codexrpc"
)

var errWorkNotReady = errors.New("coverage work frontier is not ready")

// renewWork belongs to the same owner as accept/pump. It separates live
// consumption from immutable stage receipts: an ordinary RPC response may move
// the former, but may never inflate the journal prefix attached to an old digest.
// The owner must still route every work call and consume its response ordinal.
func (c *coordinator) renewWork(g *codexGate) (codexrpc.Consumed, error) {
	if c.gate != g {
		return codexrpc.Consumed{}, errStageBinding
	}
	if err := c.check(); err != nil {
		return codexrpc.Consumed{}, err
	}
	if c.flight != nil || len(c.stream.candidates) != 0 || len(c.stream.prefix.pending) != 0 {
		return codexrpc.Consumed{}, errWorkNotReady
	}
	for _, item := range c.stream.lifecycles.items {
		if !item.completed {
			return codexrpc.Consumed{}, errWorkNotReady
		}
	}
	p, ok := g.client.Current()
	if !ok || p.Binding != g.binding {
		return codexrpc.Consumed{}, errWorkNotReady
	}
	through, err := c.stream.prefix.frontier()
	if err != nil {
		return codexrpc.Consumed{}, err
	}
	return g.client.RenewConsumed(p, through)
}

// Requests are immutable after construction; replies are buffered so an abandoned
// caller cannot block the receive owner. The owner accepts at most one operation.
type workRequest struct {
	ctx    context.Context
	method string
	params json.RawMessage
	reply  chan workResult
}
type workResult struct {
	response codexrpc.Response
	err      error
}

func newWorkRequest(ctx context.Context, method string, params json.RawMessage, maxBytes int) (*workRequest, error) {
	if ctx == nil || method == "" || len(method) > 512 || maxBytes <= 0 || len(params) > maxBytes || (len(params) > 0 && !json.Valid(params)) {
		return nil, codexrpc.ErrRequest
	}
	return &workRequest{ctx: ctx, method: method, params: append(json.RawMessage(nil), params...), reply: make(chan workResult, 1)}, nil
}

// Every work source uses this same check before acquiring a live publication
// fence. A valid fence for one thread must never authorize a different thread,
// configuration changes, or an unclassified native method.
func (c *coordinator) validateWork(req *workRequest) error {
	// The terminal may refresh the same read-only metadata it queried during
	// startup. These calls still use the owner's live fence and response prefix.
	// Auxiliary sources are restricted by the router before reaching this owner.
	if startupRead(req.method) {
		if checkJSON(req.params) != nil {
			return codexrpc.ErrRequest
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(req.params, &fields) != nil {
			return codexrpc.ErrRequest
		}
		return nil
	}
	var allowed []string
	switch req.method {
	case "turn/start", "thread/queue/add":
		allowed = []string{"threadId", "input", "clientUserMessageId"}
	case "turn/steer":
		allowed = []string{"threadId", "input", "expectedTurnId", "clientUserMessageId"}
	case "turn/interrupt":
		allowed = []string{"threadId", "turnId"}
	case "thread/queue/list":
		allowed = []string{"threadId", "cursor", "limit"}
	case "thread/read":
		allowed = []string{"threadId", "includeTurns"}
	case "thread/turns/list":
		allowed = []string{"threadId", "cursor", "limit", "sortDirection", "itemsView"}
	case "thread/items/list":
		allowed = []string{"threadId", "turnId", "cursor", "limit", "sortDirection"}
	default:
		return codexrpc.ErrRequest
	}
	if err := checkJSON(req.params); err != nil {
		return codexrpc.ErrRequest
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(req.params, &fields) != nil {
		return codexrpc.ErrRequest
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !slices.Contains(allowed, name) {
			if req.method != "turn/start" || c.settings == nil || !c.settings.SameTurnSetting(name, fields[name]) {
				return scopeSettingError(name)
			}
		}
	}
	var params struct {
		Thread string `json:"threadId"`
	}
	if json.Unmarshal(req.params, &params) != nil || params.Thread != c.stream.lifecycles.thread {
		return codexrpc.ErrRequest
	}
	if req.method == "thread/turns/list" || req.method == "thread/items/list" {
		if !validHistoryPage(fields) {
			return codexrpc.ErrRequest
		}
	}
	return nil
}

// Pinned native TUI history pages are read-only and remain scoped to the
// admitted thread. Accept its opaque string cursors, never arbitrary objects.
func validHistoryPage(fields map[string]json.RawMessage) bool {
	for _, key := range []string{"cursor", "turnId", "sortDirection", "itemsView"} {
		raw, ok := fields[key]
		if !ok || strings.TrimSpace(string(raw)) == "null" {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) != nil || len(value) > 8192 {
			return false
		}
		switch key {
		case "turnId":
			if value == "" {
				return false
			}
		case "sortDirection":
			if value != "asc" && value != "desc" {
				return false
			}
		case "itemsView":
			if value != "notLoaded" && value != "summary" && value != "full" {
				return false
			}
		}
	}
	if raw, ok := fields["limit"]; ok && strings.TrimSpace(string(raw)) != "null" {
		var limit int
		if json.Unmarshal(raw, &limit) != nil || limit < 1 || limit > 1000 {
			return false
		}
	}
	return true
}

// Only fixed schema names cross the diagnostic boundary, never values or
// arbitrary client field names. Retain ErrRequest classification for callers.
type workScopeError struct{ field string }

func (e *workScopeError) Error() string { return "request outside admitted scope: " + e.field }
func (e *workScopeError) Unwrap() error { return codexrpc.ErrRequest }
func scopeSettingError(field string) error {
	switch field {
	case "collaborationMode", "model", "cwd", "approvalsReviewer", "approvalPolicy", "sandboxPolicy", "runtimeWorkspaceRoots", "effort", "serviceTier", "serviceTierForTurn", "turnTrigger", "summary", "personality":
		return &workScopeError{field: "baseline setting mismatch (" + field + ")"}
	default:
		return &workScopeError{field: "unsupported parameter"}
	}
}
