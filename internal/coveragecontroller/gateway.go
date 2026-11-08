package coveragecontroller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/meshcrawler/hrx/internal/codexlaunch"
	"github.com/meshcrawler/hrx/internal/codexrpc"
)

// gateway owns the native connection from the TUI handshake through shutdown.
// All ingress shares one router. Socket handlers must assign sources from their
// private endpoints and must never obtain this gateway's native client.
// The launcher routes terminal and auxiliary traffic through this owner.
type gateway struct {
	input                                   inputFence
	verifyPending                           func(context.Context, OwnedPendingRequest) error
	attentionMu                             sync.Mutex
	attentionTestHook                       func(context.Context, AttentionTestPoint) error
	expectedModel, expectedWorkingDirectory string
	expectedThreadID                        string
	combinedResumeDrain                     bool
	initialDeveloperInstructions            string
	profileDeveloperInstructions            codexlaunch.Optional[string]
	g                                       *codexGate
	b                                       *bootstrap
	store                                   stageStore
	router                                  *workRouter
	events                                  *eventQueue
	ctx                                     context.Context
	cancel                                  context.CancelFunc
	cancelCause                             context.CancelCauseFunc
	startup                                 chan struct{}
	done                                    chan struct{}
	ready                                   chan struct{}
	peers                                   chan struct{}
	terminal                                atomic.Bool
	state                                   atomic.Int32 // 0 bootstrap, 1 controller owns transport, 2 stopped
	initialized                             bool
	initResult                              json.RawMessage
	stageBudget, shutdownBudget             time.Duration
	report                                  serveReport // read only after done closes
}

func newGateway(parent context.Context, g *codexGate, store stageStore, name string, stageBudget, shutdownBudget time.Duration) (*gateway, error) {
	if parent == nil || g == nil || store == nil || stageBudget <= 0 || shutdownBudget <= 0 {
		return nil, errStageBinding
	}
	id, revision := g.identity()
	b, err := newBootstrap(id, revision, name, 128, 8<<20, 4<<20)
	if err != nil {
		return nil, err
	}
	events, err := newEventQueue(128, 8<<20)
	if err != nil {
		return nil, err
	}
	b.events = events
	router, err := newWorkRouter(g, 16, 4<<20)
	if err != nil {
		return nil, err
	}
	g.approvals = newApprovalBook(g.binding.Generation, 16)
	b.approvals = g.approvals
	ctx, cancelCause := context.WithCancelCause(parent)
	cancel := func() { cancelCause(context.Canceled) }
	h := &gateway{g: g, b: b, store: store, router: router, events: events, ctx: ctx, cancel: cancel, cancelCause: cancelCause, startup: make(chan struct{}, 1), peers: make(chan struct{}, 16), done: make(chan struct{}), ready: make(chan struct{}), stageBudget: stageBudget, shutdownBudget: shutdownBudget}
	go func() {
		<-ctx.Done()
		if h.state.CompareAndSwap(0, 2) {
			g.revoke()
			events.stop(context.Cause(ctx))
			shutdown, stop := context.WithTimeout(context.Background(), shutdownBudget)
			defer stop()
			transport, err := g.client.Stop(shutdown)
			h.report = serveReport{Cause: context.Cause(ctx), Transport: transport, CleanupError: err, ReceiverJoined: true, WriterJoined: true, WorkerJoined: true}
			close(h.done)
		}
	}()
	return h, nil
}
func gatewayReply(id json.RawMessage, r codexrpc.Response) ([]byte, error) {
	reply := map[string]json.RawMessage{"id": id}
	if len(r.RemoteError) > 0 {
		reply["error"] = r.RemoteError
	} else {
		reply["result"] = r.Result
		if len(r.Result) == 0 {
			reply["result"] = json.RawMessage("null")
		}
	}
	return json.Marshal(reply)
}
func (h *gateway) handle(ctx context.Context, source workSource, raw []byte) ([]byte, error) {
	if ctx == nil || len(raw) > h.router.maxFrame || checkJSON(raw) != nil {
		return nil, codexrpc.ErrRequest
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(raw, &request) != nil || request == nil {
		return nil, codexrpc.ErrRequest
	}
	for key := range request {
		if key != "id" && key != "method" && key != "params" && key != "jsonrpc" {
			return nil, codexrpc.ErrRequest
		}
	}
	if version, ok := request["jsonrpc"]; ok && string(version) != `"2.0"` {
		return nil, codexrpc.ErrRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var method string
	if json.Unmarshal(request["method"], &method) != nil {
		return nil, codexrpc.ErrRequest
	}
	notification := method == "initialized" && request["id"] == nil
	if !notification && !requestID(request["id"]) {
		return nil, codexrpc.ErrRequest
	}
	if source != terminalSource && source != boardSource && source != directMessageSource {
		return nil, codexrpc.ErrRequest
	}
	for {
		switch h.state.Load() {
		case 2:
			return nil, errCoordinatorLost
		case 1:
			// Auxiliary queue clients handshake locally; they cannot reinitialize the
			// one native connection or select another native thread.
			if notification {
				return nil, nil
			}
			if method == "initialize" {
				return gatewayReply(request["id"], codexrpc.Response{Result: h.initResult})
			}
			if method == "pitput/attention/continue" {
				return h.continuationRoute(ctx, source, request["id"], request["params"])
			}
			if method == "pitput/attention/observe" {
				return h.attentionOperation(ctx, source, request["id"], request["params"], true)
			}
			if method == "pitput/attention/admit" {
				return h.attention(ctx, source, request["id"], request["params"])
			}
			bounded, cancel := context.WithTimeout(ctx, h.stageBudget)
			release, err := h.input.acquire(bounded, false)
			if err != nil {
				cancel()
				return nil, err
			}
			result, err := h.router.dispatch(bounded, source, raw)
			release()
			cancel()
			return result, err
		}
		if source != terminalSource {
			return nil, errWorkNotReady
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-h.done:
			return nil, errCoordinatorLost
		case h.startup <- struct{}{}:
		}
		if h.state.Load() != 0 {
			<-h.startup
			continue
		}
		result, err := h.startupRequest(ctx, request, method, notification)
		<-h.startup
		if err != nil {
			h.cancelCause(err)
		}
		return result, err
	}
}
func (h *gateway) startupRequest(caller context.Context, request map[string]json.RawMessage, method string, notification bool) ([]byte, error) {
	// Each accepted startup operation has a real wall-clock budget. Canceling a
	// dispatched native call has uncertain effects and tears down the owner.
	ctx, cancel := context.WithTimeout(h.ctx, h.stageBudget)
	defer cancel()
	stop := context.AfterFunc(caller, cancel)
	defer stop()
	if err := caller.Err(); err != nil {
		return nil, err
	}
	if notification {
		if h.b.step != 1 || h.initialized {
			return nil, errBootstrap
		}
		if err := h.g.client.Notify(ctx, "initialized", request["params"]); err != nil {
			return nil, err
		}
		h.initialized = true
		return nil, nil
	}
	params := request["params"]
	if method == "initialize" {
		if h.b.step != 0 {
			return nil, errBootstrap
		}
		result, err := h.bootstrapCall(ctx, method, params)
		if err != nil {
			return nil, err
		}
		h.initResult = append(json.RawMessage(nil), result.Result...)
		return gatewayReply(request["id"], result)
	}
	if !h.initialized || h.b.step != 1 {
		return nil, errBootstrap
	}
	if method == "thread/read" {
		if !validResumeLookup(params, h.expectedThreadID) {
			return nil, errBootstrap
		}
		h.b.resumeLookup = true
		result, err := h.bootstrapCall(ctx, method, params)
		h.b.resumeLookup = false
		if err != nil {
			return nil, err
		}
		var read struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if len(result.RemoteError) > 0 || json.Unmarshal(result.Result, &read) != nil || read.Thread.ID != h.expectedThreadID {
			return nil, errBootstrap
		}
		return gatewayReply(request["id"], result)
	}
	if startupRead(method) {
		result, err := h.bootstrapCall(ctx, method, params)
		if err != nil {
			return nil, err
		}
		return gatewayReply(request["id"], result)
	}
	if method != "thread/start" && method != "thread/resume" {
		return nil, fmt.Errorf("%w: unsupported startup method %.80q", errBootstrap, method)
	}
	var start json.RawMessage
	var err error
	if method == "thread/resume" {
		start, h.b.resumeThread, err = preservedResume(params, h.router.maxFrame)
		if err == nil && h.expectedThreadID != "" && h.b.resumeThread != h.expectedThreadID {
			err = errBootstrap
		}
	} else {
		if h.expectedThreadID != "" {
			return nil, fmt.Errorf("%w: saved launch requires resume", errBootstrap)
		}
		start, err = h.initializedProfileStart(ctx, params)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: invalid %s parameters", err, method)
	}
	h.b.requestHash = sha256.Sum256(start)
	if method == "thread/resume" && h.combinedResumeDrain {
		h.b.drain, err = newResumeDrain(h.store, h.b.resumeThread)
		if err != nil {
			return nil, err
		}
		if _, err = h.resumeQueue(ctx, true); err != nil {
			return nil, err
		}
		read, _ := json.Marshal(map[string]any{"threadId": h.b.resumeThread, "includeTurns": true})
		stopped, readErr := h.bootstrapCall(ctx, "thread/read", read)
		if readErr != nil {
			return nil, readErr
		}
		if err = h.b.drain.captureStoppedHistory(stopped.Result); err != nil {
			return nil, err
		}
		h.b.drain.allowInterruptedHistory = h.verifyPending != nil
		h.b.drain.armed = true
	}
	result, err := h.bootstrapCall(ctx, method, start)
	if err != nil {
		return nil, err
	}
	if err := h.b.native.ValidateStartResponse(start); err != nil {
		return nil, err
	}
	if err := h.validateLaunchBinding(); err != nil {
		return nil, err
	}
	thread := h.b.native.ThreadID
	name, _ := json.Marshal(map[string]string{"threadId": thread, "name": h.b.name})
	if _, err = h.bootstrapCall(ctx, "thread/name/set", name); err != nil {
		return nil, err
	}
	if h.b.drain != nil {
		if err = h.finishResumeDrain(ctx); err != nil {
			return nil, err
		}
	} else {
		read, _ := json.Marshal(map[string]any{"threadId": thread, "includeTurns": true})
		if _, err = h.bootstrapCall(ctx, "thread/read", read); err != nil {
			return nil, err
		}
	}
	// The stage must outlive this request function's local timeout context.
	if !h.state.CompareAndSwap(0, 1) {
		return nil, errCoordinatorLost
	}
	stageCtx, stageCancel := context.WithTimeout(h.ctx, h.stageBudget)
	c, err := newBaselineCoordinator(stageCtx, h.g, h.b, h.store, 128, 16)
	if err != nil {
		stageCancel()
		h.g.revoke()
		h.events.stop(err)
		shutdown, stop := context.WithTimeout(context.Background(), h.shutdownBudget)
		var writerErr error
		if c != nil {
			writerErr = c.stop(shutdown)
		}
		transport, transportErr := h.g.client.Stop(shutdown)
		stop()
		h.report = serveReport{Cause: err, Transport: transport, ReceiverJoined: true, WriterJoined: writerErr == nil, WorkerJoined: true, CleanupError: errors.Join(writerErr, transportErr)}
		h.state.Store(2)
		h.cancel()
		close(h.done)
		return nil, err
	}
	c.stream.approvals = h.g.approvals
	go func() {
		defer stageCancel()
		h.report = c.serveWork(h.ctx, h.g, h.stageBudget, h.shutdownBudget, h.router.requests)
		h.state.Store(2)
		h.cancel()
		close(h.done)
	}()
	// This response is not an authority token: later input always uses the live
	// owner fence. Do not release the initial TUI prompt before durable staging.
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if p, ok := h.g.client.Current(); ok && p.Binding == h.g.binding {
			close(h.ready)
			return gatewayReply(request["id"], result)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-h.done:
			return nil, errCoordinatorLost
		case <-tick.C:
		}
	}
}
func (h *gateway) close(ctx context.Context) (serveReport, error) {
	h.cancel()
	select {
	case <-h.done:
		return h.report, h.report.CleanupError
	case <-ctx.Done():
		return serveReport{}, errors.Join(errReceiveShutdown, ctx.Err())
	}
}

func (h *gateway) validateLaunchBinding() error {
	if h.expectedModel != "" && h.b.native.Model != h.expectedModel {
		return fmt.Errorf("%w: native model differs from launch record", errBootstrap)
	}
	if h.expectedWorkingDirectory != "" {
		cwd, err := filepath.EvalSymlinks(h.b.native.Cwd)
		if err != nil || cwd != h.expectedWorkingDirectory {
			return fmt.Errorf("%w: native working directory differs from launch record", errBootstrap)
		}
	}
	return nil
}

// Native resume TUI resolves its exact saved thread before resuming it. This
// lookup never creates an admission baseline or permits a turn.
func validResumeLookup(params json.RawMessage, expected string) bool {
	if expected == "" || checkJSON(params) != nil {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(params, &fields) != nil {
		return false
	}
	for key := range fields {
		if key != "threadId" && key != "includeTurns" {
			return false
		}
	}
	var id string
	if json.Unmarshal(fields["threadId"], &id) != nil || id != expected {
		return false
	}
	if raw, ok := fields["includeTurns"]; ok {
		var include bool
		if string(raw) == "null" || json.Unmarshal(raw, &include) != nil {
			return false
		}
	}
	return true
}
