package coveragecontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/meshcrawler/hrx/internal/codexrpc"
)

type workSource uint8

const (
	terminalSource workSource = iota + 1
	boardSource
	directMessageSource
)

var errRouterBusy = errors.New("coverage router request bound reached")

// workRouter is shared by all ingress handlers. Source is assigned by the host's
// endpoint, never by an RPC field. Downstream IDs are kept per invocation; only
// codexrpc's private IDs reach the one native connection. No handler can bypass
// the controller's admission fence or consume native responses itself.
type workRouter struct {
	requests chan *workRequest
	slots    chan struct{}
	done     <-chan struct{}
	maxFrame int
}

func newWorkRouter(g *codexGate, concurrent, maxFrame int) (*workRouter, error) {
	if g == nil || concurrent <= 0 || concurrent > 128 || maxFrame <= 0 {
		return nil, errStageBinding
	}
	return &workRouter{requests: make(chan *workRequest), slots: make(chan struct{}, concurrent), done: g.done(), maxFrame: maxFrame}, nil
}
func requestID(raw json.RawMessage) bool {
	var text string
	if json.Unmarshal(raw, &text) == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return len(text) <= 256
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return false
	}
	_, err := strconv.ParseInt(string(n), 10, 64)
	return err == nil
}
func (r *workRouter) dispatch(ctx context.Context, source workSource, raw []byte) ([]byte, error) {
	if source != terminalSource && source != boardSource && source != directMessageSource {
		return nil, codexrpc.ErrRequest
	}
	// Do not create an unbounded parked goroutine/parameter queue under load.
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	default:
		return nil, errRouterBusy
	}
	if len(raw) > r.maxFrame || checkJSON(raw) != nil {
		return nil, codexrpc.ErrRequest
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || envelope == nil || !requestID(envelope["id"]) {
		return nil, codexrpc.ErrRequest
	}
	for key := range envelope {
		if key != "id" && key != "method" && key != "params" && key != "jsonrpc" {
			return nil, codexrpc.ErrRequest
		}
	}
	if version, ok := envelope["jsonrpc"]; ok && string(version) != `"2.0"` {
		return nil, codexrpc.ErrRequest
	}
	var method string
	if json.Unmarshal(envelope["method"], &method) != nil {
		return nil, codexrpc.ErrRequest
	}
	if source != terminalSource && method != "thread/queue/add" && method != "thread/read" {
		return nil, codexrpc.ErrRequest
	}
	req, err := newWorkRequest(ctx, method, envelope["params"], r.maxFrame)
	if err != nil {
		return nil, err
	}
	result := r.execute(ctx, req)

	reply := map[string]json.RawMessage{"id": bytes.Clone(envelope["id"])}
	if version, ok := envelope["jsonrpc"]; ok {
		reply["jsonrpc"] = version
	}
	switch {
	case len(result.response.RemoteError) > 0:
		reply["error"] = result.response.RemoteError
	case result.err != nil:
		// Fixed local classifications only; no filesystem paths or journal internals.
		code, message := -32001, "coverage admission unavailable"
		if errors.Is(result.err, codexrpc.ErrRequest) {
			code, message = -32602, "request outside admitted scope"
			var scope *workScopeError
			if errors.As(result.err, &scope) {
				message = scope.Error()
			}
		}
		reply["error"], _ = json.Marshal(map[string]any{"code": code, "message": message})
	default:
		reply["result"] = result.response.Result
		if len(reply["result"]) == 0 {
			reply["result"] = json.RawMessage("null")
		}
	}
	return json.Marshal(reply)
}

func (r *workRouter) execute(ctx context.Context, req *workRequest) workResult {
	select {
	case <-ctx.Done():
		return workResult{err: ctx.Err()}
	case <-r.done:
		return workResult{err: errCoordinatorLost}
	case r.requests <- req:
	}
	select {
	case result := <-req.reply:
		return result
	case <-ctx.Done():
		return workResult{err: ctx.Err()}
	case <-r.done:
		return workResult{err: errCoordinatorLost}
	}
}
