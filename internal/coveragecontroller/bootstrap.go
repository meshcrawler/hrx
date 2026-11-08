package coveragecontroller

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/meshcrawler/hrx/internal/codexlaunch"
)

var errBootstrap = errors.New("invalid fresh coverage bootstrap")

// bootstrap records the exclusive owner's Call method alongside each response.
// Methods must come from that Call, never from peer-supplied response metadata.
// It consumes the same bounded prefix as notifications, including before start.
// This is evidence assembly only: no gate, journal or UI admission is performed.
type bootstrap struct {
	events              *eventQueue
	approvals           *approvalBook
	prefix              *prefix
	methods             map[uint64]string
	name                string
	revision            uint64
	step                int
	resumeLookup        bool
	resumeThread        string
	native              codexlaunch.Native
	startHash, readHash [32]byte
	requestHash         [32]byte
	sealed              bool
	drain               *resumeDrain
}

// bootstrapReceipt is an immutable binding of parsed native evidence to the
// actual consumed prefix. It is historical evidence, never reusable authority.
type bootstrapReceipt struct {
	connection          connectionID
	through             uint64
	revision            uint64
	identity            codexlaunch.Identity
	startHash, readHash [32]byte
}

func newBootstrap(id connectionID, revision uint64, name string, messages, totalBytes, frameBytes int) (*bootstrap, error) {
	if revision == 0 || name == "" || len(name) > 512 {
		return nil, errBootstrap
	}
	p, err := newPrefix(id, messages, totalBytes, frameBytes)
	if err != nil {
		return nil, err
	}
	return &bootstrap{prefix: p, revision: revision, name: name, methods: make(map[uint64]string)}, nil
}

func (b *bootstrap) fail(err error) error {
	if b.events != nil {
		b.events.stop(err)
	}
	b.methods = nil
	b.drain = nil
	b.native = codexlaunch.Native{}
	b.startHash, b.readHash, b.requestHash = [32]byte{}, [32]byte{}, [32]byte{}
	return b.prefix.invalidate(err)
}

func (b *bootstrap) accept(f frame, callMethod string) error {
	if b.sealed {
		return b.fail(errBootstrap)
	}
	if (f.kind == responseFrame) != (callMethod != "") || len(callMethod) > 64 {
		return b.fail(errBootstrap)
	}
	if err := b.prefix.add(f); err != nil {
		return b.fail(err)
	}
	if callMethod != "" {
		b.methods[f.ordinal] = callMethod
	}
	for {
		next, ok, err := b.prefix.next()
		if err != nil {
			return b.fail(err)
		}
		if !ok {
			return nil
		}
		if err := b.reduce(next, b.methods[next.ordinal]); err != nil {
			return b.fail(err)
		}
		delete(b.methods, next.ordinal)
		if err := b.prefix.commit(next.ordinal); err != nil {
			return b.fail(err)
		}
		if (next.kind == notificationFrame || next.kind == serverRequestFrame) && b.events != nil {
			if err := b.events.push(next); err != nil {
				return b.fail(err)
			}
		}
	}
}

func (b *bootstrap) reduce(f frame, method string) error {
	if err := checkJSON(f.raw); err != nil {
		return err
	}
	if f.kind == serverRequestFrame {
		if b.approvals == nil {
			return errServerRequest
		}
		return b.approvals.reduce(f, "")
	}
	if f.kind == notificationFrame {
		var n struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(f.raw, &n) != nil || n.Method == "" {
			return fmt.Errorf("%w: malformed notification", errBootstrap)
		}
		if strings.HasPrefix(n.Method, "turn/") || strings.HasPrefix(n.Method, "item/") {
			if b.drain != nil {
				return b.drain.notification(n.Method, n.Params)
			}
			var activity struct {
				ThreadID string `json:"threadId"`
				TurnID   string `json:"turnId"`
				Turn     struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"turn"`
			}
			_ = json.Unmarshal(n.Params, &activity)
			return fmt.Errorf("%w: activity notification before baseline (method=%.80q resume=%t thread_match=%t turn_present=%t interrupted=%t step=%d)",
				errBootstrap, n.Method, b.resumeThread != "", b.resumeThread != "" && activity.ThreadID == b.resumeThread,
				activity.TurnID != "" || activity.Turn.ID != "", activity.Turn.Status == "interrupted", b.step)
		}
		return checkThreadBoundary(n.Method, n.Params, b.native.Identity.ThreadID)
	}
	if b.step == 1 && (startupRead(method) || b.resumeLookup && method == "thread/read") {
		return nil
	}
	order := [...]string{"initialize", "thread/start", "thread/name/set", "thread/read"}
	if b.resumeThread != "" {
		order[1] = "thread/resume"
	}
	recoveryRead := b.drain != nil && (method == "thread/queue/list" || method == "thread/read")
	if !recoveryRead && (b.step >= len(order) || method != order[b.step]) {
		return fmt.Errorf("%w: RPC order at step %d", errBootstrap, b.step)
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(f.raw, &body) != nil || body == nil {
		return errBootstrap
	}
	result, ok := body["result"]
	if remoteError, rejected := body["error"]; rejected {
		var remote struct{ Code int }
		_ = json.Unmarshal(remoteError, &remote)
		return fmt.Errorf("%w: native %s refused (code %d)", errBootstrap, method, remote.Code)
	}
	if !ok {
		return fmt.Errorf("%w: native result absent", errBootstrap)
	}
	if recoveryRead {
		return nil // Internal bounded recovery reads do not advance baseline RPC order.
	}
	switch method {
	case "thread/start", "thread/resume":
		native, err := codexlaunch.ParseThreadStart(result)
		if err != nil {
			return err
		}
		if b.resumeThread != "" && native.ThreadID != b.resumeThread {
			return fmt.Errorf("%w: resume thread identity mismatch", errBootstrap)
		}
		if method == "thread/resume" {
			var existing struct {
				Thread struct {
					Name *string `json:"name"`
				} `json:"thread"`
			}
			if json.Unmarshal(result, &existing) != nil {
				return errBootstrap
			}
			// Keep the user's existing title. Only an unnamed resumed thread
			// needs the host's stable baseline name materialized on disk.
			if existing.Thread.Name != nil && *existing.Thread.Name != "" {
				b.name = *existing.Thread.Name
			}
		}
		if method == "thread/start" {
			// Pinned core session/mod.rs initializes NEW sessions in Default
			// mode with the effective model/effort and no mode-specific prompt.
			// Resume must use its reported saved mode instead of this default.
			var effort any
			if native.ReasoningEffort.Present {
				effort = native.ReasoningEffort.Value
			}
			mode, _ := json.Marshal(map[string]any{"mode": "default", "settings": map[string]any{
				"model": native.Model, "reasoning_effort": effort, "developer_instructions": nil}})
			native.CollaborationMode = codexlaunch.Optional[json.RawMessage]{Present: true, Value: mode}
		}
		b.native, b.startHash = native, sha256.Sum256(result)
	case "thread/read":
		var err error
		if b.resumeThread != "" {
			err = b.native.ValidateResumeBaseline(result, b.name)
		} else {
			err = b.native.ValidateFreshBaseline(result, b.name)
		}
		if err != nil {
			return err
		}
		b.readHash = sha256.Sum256(result)
	}
	b.step++
	return nil
}

// seal requires all four successful replies and no unconsumed frames. The
// owner must still compare effective launch intent, stage durably, keep receiving
// and pass the live publication gate; this receipt cannot replace those checks.
func (b *bootstrap) seal() (bootstrapReceipt, error) {
	if b.sealed || b.drain != nil || b.prefix.cause != nil || b.step != 4 || len(b.prefix.pending) != 0 {
		return bootstrapReceipt{}, b.fail(errBootstrap)
	}
	b.sealed = true
	return bootstrapReceipt{b.prefix.connection, b.prefix.consumed, b.revision, b.native.Identity, b.startHash, b.readHash}, nil
}

// Native startup metadata calls do not change thread configuration or release
// model input. Their replies still occupy and must fill the shared prefix.
func startupRead(method string) bool {
	switch method {
	case "config/read", "configRequirements/read", "account/read", "model/list", "skills/list", "hooks/list", "app/list", "mcpServerStatus/list", "experimentalFeature/list", "thread/list", "thread/loaded/list",
		"collaborationMode/list", "account/bedrock/checkGovCloudRequirements":
		return true
	default:
		return false
	}
}
