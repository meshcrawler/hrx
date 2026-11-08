package coveragecontroller

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/meshcrawler/hrx/internal/codexrpc"
)

// approvalBook retains only bounded, generation-owned native requests. The pinned
// outgoing_message.rs allocates monotonically increasing integer request IDs.
// Enforcing that contract prevents a delayed duplicate terminal response from
// answering a later request with a recycled ID, without an unbounded seen set.
// Capture does not authorize forwarding: reduction marks the request ready only
// after the owner accounts for its ordinal in the contiguous receive prefix.
type approvalBook struct {
	mu         sync.Mutex
	generation string
	last       int64
	limit      int
	pending    map[int64]*approvalEntry
	replies    chan *approvalReply
}
type approvalEntry struct {
	message codexrpc.Message
	ready   bool
	claimed bool
}

func newApprovalBook(generation string, limit int) *approvalBook {
	return &approvalBook{generation: generation, last: -1, limit: limit, pending: make(map[int64]*approvalEntry), replies: make(chan *approvalReply, limit)}
}
func approvalEnvelope(raw []byte) (int64, map[string]json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	var id int64
	if checkJSON(raw) != nil || json.Unmarshal(raw, &envelope) != nil || envelope == nil || len(envelope["id"]) == 0 || string(envelope["id"]) == "null" || json.Unmarshal(envelope["id"], &id) != nil || id < 0 {
		return 0, nil, codexrpc.ErrRequest
	}
	return id, envelope, nil
}
func (b *approvalBook) capture(message codexrpc.Message) error {
	id, _, err := approvalEnvelope(message.Raw)
	if err != nil || message.Kind != codexrpc.ServerRequest || message.Generation != b.generation || message.Ordinal == 0 {
		return errServerRequest
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if id <= b.last || len(b.pending) >= b.limit {
		return errServerRequest
	}
	b.last = id
	b.pending[id] = &approvalEntry{message: message}
	return nil
}
func (b *approvalBook) reduce(f frame, thread string) error {
	id, envelope, err := approvalEnvelope(f.raw)
	if err != nil {
		return err
	}
	var method string
	var params struct {
		Thread string `json:"threadId"`
	}
	if json.Unmarshal(envelope["method"], &method) != nil || json.Unmarshal(envelope["params"], &params) != nil {
		return errServerRequest
	}
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/tool/requestUserInput", "mcpServer/elicitation/request", "item/permissions/requestApproval", "item/tool/call":
		if params.Thread != thread || thread == "" {
			return errServerRequest
		}
	case "account/chatgptAuthTokens/refresh", "attestation/generate", "currentTime/read":
		// These pinned connection-scoped requests go only to the owning terminal.
		// They still use one-shot correlation and the live writer fence after startup.
		var object map[string]json.RawMessage
		if json.Unmarshal(envelope["params"], &object) != nil || object == nil {
			return errServerRequest
		}
	default:
		return errServerRequest
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.pending[id]
	if entry == nil || entry.message.Ordinal != f.ordinal || entry.message.Generation != f.connection.generation || entry.ready {
		return errServerRequest
	}
	entry.ready = true
	return nil
}

// take consumes correlation before writing. Any uncertain write is terminal;
// retrying an approval against a different frontier must not duplicate effects.
func (b *approvalBook) take(raw []byte) (codexrpc.Message, json.RawMessage, json.RawMessage, error) {
	id, envelope, err := approvalEnvelope(raw)
	if err != nil {
		return codexrpc.Message{}, nil, nil, err
	}
	for key := range envelope {
		if key != "id" && key != "result" && key != "error" && key != "jsonrpc" {
			return codexrpc.Message{}, nil, nil, codexrpc.ErrRequest
		}
	}
	if version, ok := envelope["jsonrpc"]; ok && string(version) != `"2.0"` {
		return codexrpc.Message{}, nil, nil, codexrpc.ErrRequest
	}
	result, hasResult := envelope["result"]
	remoteError, hasError := envelope["error"]
	if hasResult == hasError {
		return codexrpc.Message{}, nil, nil, codexrpc.ErrRequest
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.pending[id]
	if entry == nil || !entry.ready || entry.claimed {
		return codexrpc.Message{}, nil, nil, errServerRequest
	}
	entry.claimed = true
	return entry.message, result, remoteError, nil
}

type approvalReply struct {
	ctx                 context.Context
	message             codexrpc.Message
	result, remoteError json.RawMessage
	done                chan error
}

func (h *gateway) answer(ctx context.Context, source workSource, raw []byte) error {
	if source != terminalSource || h.state.Load() == 2 || h.g.approvals == nil || len(raw) > h.router.maxFrame {
		return errServerRequest
	}
	message, result, remote, err := h.g.approvals.take(raw)
	if err != nil {
		return err
	}
	reply := &approvalReply{ctx: ctx, message: message, result: result, remoteError: remote, done: make(chan error, 1)}
	select {
	case h.g.approvals.replies <- reply:
	case <-ctx.Done():
		return ctx.Err()
	case <-h.done:
		return errCoordinatorLost
	}
	select {
	case err := <-reply.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-h.done:
		return errCoordinatorLost
	}
}

func (b *approvalBook) live(message codexrpc.Message) bool {
	id, _, err := approvalEnvelope(message.Raw)
	if err != nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := b.pending[id]
	return entry != nil && entry.ready && entry.claimed && entry.message.Ordinal == message.Ordinal
}
func (b *approvalBook) finish(message codexrpc.Message) {
	id, _, err := approvalEnvelope(message.Raw)
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if entry := b.pending[id]; entry != nil && entry.message.Ordinal == message.Ordinal {
		delete(b.pending, id)
	}
}
func (b *approvalBook) resolve(f frame, thread string) error {
	var event struct {
		Method string
		Params struct {
			Thread string          `json:"threadId"`
			ID     json.RawMessage `json:"requestId"`
		}
	}
	if json.Unmarshal(f.raw, &event) != nil {
		return errEventFormat
	}
	if event.Method != "serverRequest/resolved" {
		return nil
	}
	var id int64
	if event.Params.Thread != thread || len(event.Params.ID) == 0 || string(event.Params.ID) == "null" || json.Unmarshal(event.Params.ID, &id) != nil || id < 0 {
		return errServerRequest
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pending, id)
	return nil
}

func (b *approvalBook) outstanding() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
}
func connectionRequest(method string) bool {
	switch method {
	case "account/chatgptAuthTokens/refresh", "attestation/generate", "currentTime/read":
		return true
	default:
		return false
	}
}
