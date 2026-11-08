package coveragecontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
	"unicode/utf8"

	"github.com/meshcrawler/hrx/internal/codexlaunch"
)

// collectBootstrap exclusively owns Call/Next on a newly constructed client.
// The client's bounded reader already runs before initialize; while Call waits,
// it retains notifications or fails closed on overflow. Every response is merged
// with preceding notifications before the next startup RPC is issued.
// This creates/names a native thread but performs no inference or work release.
// The caller still owns native teardown on failure and intent comparison before
// staging. The low-level reserved binding is not semantic baseline admission.
func collectBootstrap(ctx context.Context, g *codexGate, start codexlaunch.ThreadStart, name string, messages, totalBytes, frameBytes int) (*bootstrap, error) {
	raw, err := json.Marshal(start)
	if err != nil {
		return nil, errBootstrap
	}
	return collectBootstrapRequest(ctx, g, raw, name, messages, totalBytes, frameBytes)
}

// collectBootstrapRequest retains the complete start parameters supplied by the
// exclusive startup owner, including fields the HRX planner cannot reconstruct.
// It is not a public proxy: caller authentication and actual TUI routing remain
// the launch owner's responsibility. Request contents are never journaled.
func collectBootstrapRequest(ctx context.Context, g *codexGate, start json.RawMessage, name string, messages, totalBytes, frameBytes int) (*bootstrap, error) {
	if g == nil || g.client == nil {
		return nil, errBootstrap
	}
	id, revision := g.identity()
	b, err := newBootstrap(id, revision, name, messages, totalBytes, frameBytes)
	fail := func(err error) (*bootstrap, error) {
		g.revoke()
		if b != nil {
			b.fail(err)
		}
		return nil, err
	}
	if err != nil {
		return fail(err)
	}
	raw, err := preservedStart(start, frameBytes)
	if _, bounded := ctx.Deadline(); !bounded || err != nil {
		return fail(errBootstrap)
	}
	b.requestHash = sha256.Sum256(raw)
	call := func(method string, params any) error {
		f, err := g.call(ctx, method, params)
		if err != nil {
			return err
		}
		if err := b.accept(f, method); err != nil {
			return err
		}
		for b.prefix.consumed < f.ordinal {
			next, err := g.receive(ctx)
			if err != nil {
				return err
			}
			if err := b.accept(next, ""); err != nil {
				return err
			}
		}
		return nil
	}
	if err := call("initialize", map[string]any{"clientInfo": map[string]string{"name": "hrx-coverage", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}}); err != nil {
		return fail(err)
	}
	if err := g.client.Notify(ctx, "initialized", nil); err != nil {
		return fail(err)
	}
	if err := call("thread/start", raw); err != nil {
		return fail(err)
	}
	thread := b.native.Identity.ThreadID
	if err := call("thread/name/set", map[string]string{"threadId": thread, "name": name}); err != nil {
		return fail(err)
	}
	if err := call("thread/read", map[string]any{"threadId": thread, "includeTurns": true}); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	select {
	case <-g.done():
		return fail(errCoordinatorLost)
	default:
	}
	return b, nil
}

func preservedStart(raw json.RawMessage, limit int) (json.RawMessage, error) {
	if limit <= 0 || len(raw) > limit || checkJSON(raw) != nil {
		return nil, errBootstrap
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, errBootstrap
	}
	// Native remote TUI omits cwd unless --cd was supplied. Preserve that
	// inheritance; ParseThreadStart requires an absolute effective cwd before
	// admission, and ValidateStartResponse checks any explicit selection.
	if value, ok := fields["cwd"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		var cwd string
		if json.Unmarshal(value, &cwd) != nil || !filepath.IsAbs(cwd) {
			return nil, errBootstrap
		}
	}
	var history string
	if json.Unmarshal(fields["historyMode"], &history) != nil || history != "paginated" {
		return nil, errBootstrap
	}
	if value, ok := fields["ephemeral"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("false")) {
		return nil, errBootstrap
	}
	// Marshal produces the exact JSON encoding used by codexrpc's RawMessage
	// parameter path, including HTML escaping. No typed roundtrip drops fields.
	result, err := json.Marshal(raw)
	if err != nil || len(result) > limit {
		return nil, errBootstrap
	}
	return result, nil
}

// Ordinary durable resume preserves native parameters and binds the requested
// thread. History/path replacement needs its own identity proof before use.
func preservedResume(raw json.RawMessage, limit int) (json.RawMessage, string, error) {
	if limit <= 0 || len(raw) > limit || checkJSON(raw) != nil {
		return nil, "", errBootstrap
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, "", errBootstrap
	}
	var thread string
	if json.Unmarshal(fields["threadId"], &thread) != nil || thread == "" {
		return nil, "", errBootstrap
	}
	for _, key := range []string{"history", "path"} {
		if value, ok := fields[key]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, "", errBootstrap
		}
	}
	result, err := json.Marshal(raw)
	if err != nil || len(result) > limit {
		return nil, "", errBootstrap
	}
	return result, thread, nil
}

// initializedStart adds host initialization to the native developer-instruction
// field. No turn is created here. Existing terminal-generated guidance is kept,
// and the complete amended request is the one hashed and admitted by bootstrap.
func initializedStart(raw json.RawMessage, instructions string, limit int) (json.RawMessage, error) {
	return initializedStartValue(raw, instructions, instructions != "", limit)
}

func initializedStartValue(raw json.RawMessage, instructions string, present bool, limit int) (json.RawMessage, error) {
	preserved, err := preservedStart(raw, limit)
	if err != nil || !present {
		return preserved, err
	}
	if len(instructions) > limit || !utf8.ValidString(instructions) {
		return nil, errBootstrap
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(preserved, &fields) != nil {
		return nil, errBootstrap
	}
	var existing string
	if value, ok := fields["developerInstructions"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		if json.Unmarshal(value, &existing) != nil {
			return nil, errBootstrap
		}
	}
	combined := instructions
	if existing != "" && existing != instructions {
		if combined != "" {
			combined += "\n\n"
		}
		combined += existing
	}
	fields["developerInstructions"], err = json.Marshal(combined)
	if err != nil {
		return nil, errBootstrap
	}
	amended, err := json.Marshal(fields)
	if err != nil || len(amended) > limit {
		return nil, errBootstrap
	}
	return amended, nil
}
