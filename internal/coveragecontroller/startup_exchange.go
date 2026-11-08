package coveragecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/meshcrawler/hrx/internal/codexrpc"
)

// Bootstrap services connection-scoped native requests while an RPC is pending.
// Thread work/permission requests remain forbidden until baseline admission.
// Every read is joined before handing receive ownership to the next operation;
// a concurrently delivered frame is reduced, never discarded during handoff.
func (h *gateway) bootstrapCall(ctx context.Context, method string, params json.RawMessage) (response codexrpc.Response, failure error) {
	// Report only schema-owned operation names and the numeric admission step.
	// Never include native parameters, transcript content, or remote error data.
	defer func() {
		if failure != nil {
			operation := "startup-query"
			switch method {
			case "initialize", "thread/start", "thread/resume", "thread/read", "thread/name/set":
				operation = method
			}
			failure = fmt.Errorf("native bootstrap %s at step %d: %w", operation, h.b.step, failure)
		}
	}()

	run, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		response codexrpc.Response
		err      error
	}
	replies := make(chan outcome, 1)
	callerDone := make(chan struct{})
	go func() {
		defer close(callerDone)
		r, err := h.g.client.Call(run, method, params)
		replies <- outcome{r, err}
	}()
	type received struct {
		frame frame
		err   error
	}
	var next <-chan received
	var stopRead context.CancelFunc
	accept := func(result received) error {
		if result.err != nil {
			return result.err
		}
		return h.b.accept(result.frame, "")
	}
	defer func() {
		cancel()
		if stopRead != nil {
			stopRead()
		}
		deadline := time.NewTimer(h.shutdownBudget)
		defer deadline.Stop()
		if next != nil {
			select {
			case result := <-next:
				if failure == nil && result.err == nil {
					failure = accept(result)
				}
			case <-deadline.C:
				failure = errors.Join(failure, errReceiveShutdown)
				return
			}
		}
		select {
		case <-callerDone:
		case <-deadline.C:
			failure = errors.Join(failure, errReceiveShutdown)
		}
	}()
	var gotReply bool
	var callErr error
	for {
		if gotReply && h.b.prefix.consumed >= response.Ordinal && h.g.approvals.outstanding() == 0 {
			if next != nil {
				stopRead()
				select {
				case result := <-next:
					next = nil
					if result.err == nil {
						if err := accept(result); err != nil {
							return response, err
						}
					} else if !errors.Is(result.err, context.Canceled) {
						return response, result.err
					}
				case <-run.Done():
					return response, run.Err()
				}
				continue
			}
			if callErr != nil && !startupRead(method) {
				return response, callErr
			}
			return response, nil
		}
		if next == nil {
			readCtx, stop := context.WithCancel(run)
			stopRead = stop
			delivered := make(chan received, 1)
			next = delivered
			go func() { f, err := h.g.receive(readCtx); delivered <- received{f, err} }()
		}
		select {
		case result := <-replies:
			gotReply = true
			response, callErr = result.response, result.err
			if response.Ordinal == 0 {
				return response, callErr
			}
			f, err := h.g.responseFrame(response, nil)
			if err != nil {
				return response, err
			}
			if err := h.b.accept(f, method); err != nil {
				return response, err
			}
		case result := <-next:
			stopRead()
			next = nil
			if err := accept(result); err != nil {
				return response, err
			}
		case answer := <-h.g.approvals.replies:
			if !connectionRequest(answer.message.Method) || !h.g.approvals.live(answer.message) {
				answer.done <- errServerRequest
				return response, errServerRequest
			}
			answerCtx, stop := context.WithCancel(run)
			stopCaller := context.AfterFunc(answer.ctx, stop)
			err := answer.ctx.Err()
			if err == nil {
				err = h.g.client.Reply(answerCtx, answer.message, answer.result, answer.remoteError)
			}
			stopCaller()
			stop()
			h.g.approvals.finish(answer.message)
			answer.done <- err
			if err != nil {
				return response, err
			}
		case <-run.Done():
			return response, run.Err()
		}
	}
}
