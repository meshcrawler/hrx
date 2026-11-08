package coveragecontroller

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/meshcrawler/hrx/internal/codexrpc"
)

var errReceiveShutdown = errors.New("coverage receive loop shutdown unverified")

type serveReport struct {
	Cause                        error
	ReceiverJoined, WriterJoined bool
	WorkerJoined                 bool
	Transport                    codexrpc.Shutdown
	CleanupError                 error
}

// serve owns Next after bootstrap Call/response-prefix assembly is complete.
// No Call may run concurrently outside this owner. This is still a dormant
// post-admission loop: it neither creates a native baseline nor releases a UI.
// It always revokes before cancellation or potentially blocking cleanup.
func (c *coordinator) serve(ctx context.Context, g *codexGate, stageBudget, shutdownBudget time.Duration) serveReport {
	return c.serveWork(ctx, g, stageBudget, shutdownBudget, nil)
}

func (c *coordinator) serveWork(ctx context.Context, g *codexGate, stageBudget, shutdownBudget time.Duration, requests <-chan *workRequest) serveReport {
	if c.gate != g || stageBudget <= 0 || shutdownBudget <= 0 {
		return serveReport{Cause: c.fail(errStageBinding), CleanupError: errStageBinding}
	}
	runCtx, cancel := context.WithCancel(ctx)
	frames := make(chan frame, c.stream.prefix.maxMessages)
	failures := make(chan error, 1)
	receiverDone := make(chan struct{})
	var bufferedBytes atomic.Int64
	go func() {
		defer close(receiverDone)
		for {
			f, err := g.receive(runCtx)
			if err != nil {
				select {
				case failures <- err:
				default:
				}
				return
			}
			size := int64(len(f.raw))
			if size > int64(c.stream.prefix.maxFrame) || bufferedBytes.Add(size) > int64(c.stream.prefix.maxBytes) {
				g.revoke()
				select {
				case failures <- errPrefixLimit:
				default:
				}
				return
			}
			select {
			case frames <- f:
			case <-runCtx.Done():
				return
			default:
				// The receiver cannot wait for a potentially busy reducer before
				// fencing an observed overflow. Gate revoke is bounded and atomic.
				g.revoke()
				select {
				case failures <- errPrefixLimit:
				default:
				}
				return
			}
		}
	}()
	var stageCancel context.CancelFunc
	advance := func() error {
		budget, cancelBudget := context.WithTimeout(runCtx, stageBudget)
		err := c.pump(budget)
		if c.flightContext == budget {
			if stageCancel != nil {
				stageCancel()
			}
			stageCancel = cancelBudget
		} else {
			cancelBudget()
			if c.flightContext == nil && stageCancel != nil {
				stageCancel()
				stageCancel = nil
			}
		}
		return err
	}

	var active *workRequest
	var completed *workResult
	var workerDone <-chan struct{}
	results := make(chan workResult, 1)
	var workCancel context.CancelFunc
	var stopCaller func() bool
	release := func(result workResult) {
		active.reply <- result
		active = nil
		completed = nil
		if stopCaller != nil {
			stopCaller()
			stopCaller = nil
		}
		if workCancel != nil {
			workCancel()
			workCancel = nil
		}
	}
	var approval *approvalReply
	var approvalWorker <-chan struct{}
	var approvalWriting bool
	approvalResults := make(chan error, 1)
	loopErr := advance()
	for loopErr == nil {
		var expired <-chan struct{}
		if c.flightContext != nil {
			expired = c.flightContext.Done()
		}

		var incoming <-chan *workRequest
		var callerDone <-chan struct{}
		if active == nil {
			incoming = requests
		} else {
			callerDone = active.ctx.Done()
		}
		var answers <-chan *approvalReply
		var answerCanceled <-chan struct{}
		if g.approvals != nil && approval == nil {
			answers = g.approvals.replies
		}
		if approval != nil {
			answerCanceled = approval.ctx.Done()
		}
		select {
		case answer := <-answers:
			approval = answer
		case <-answerCanceled:
			loopErr = approval.ctx.Err()
		case err := <-approvalResults:
			g.approvals.finish(approval.message)
			approval.done <- err
			approval = nil
			approvalWriting = false
			if err != nil {
				loopErr = err
			}
		case req, ok := <-incoming:
			if !ok {
				requests = nil
				break
			}
			if req == nil {
				loopErr = errStageBinding
				break
			}
			if err := req.ctx.Err(); err != nil {
				req.reply <- workResult{err: err}
				break
			}
			if err := c.validateWork(req); err != nil {
				req.reply <- workResult{err: err}
				break
			}
			frontier, err := c.renewWork(g)
			if err != nil {
				req.reply <- workResult{err: err}
				break
			}
			active = req
			callCtx, cancelCall := context.WithCancel(runCtx)
			workCancel = cancelCall
			stopCaller = context.AfterFunc(req.ctx, cancelCall)
			done := make(chan struct{})
			workerDone = done
			go func() {
				defer close(done)
				response, err := g.client.CallAtConsumed(callCtx, frontier, req.method, req.params)
				results <- workResult{response: response, err: err}
			}()
		case <-callerDone:
			loopErr = active.ctx.Err()
		case result := <-results:
			if result.response.Ordinal == 0 {
				// Only these local refusals are known not to write. A write-time fence
				// refusal invalidates the client, so check its terminal signal too.
				select {
				case <-g.done():
					loopErr = errCoordinatorLost
				default:
				}
				if loopErr == nil && (errors.Is(result.err, codexrpc.ErrRequest) || errors.Is(result.err, codexrpc.ErrPublicationRefused)) {
					release(result)
				} else if loopErr == nil {
					loopErr = errCoordinatorLost
				}
				break
			}
			f, err := g.responseFrame(result.response, nil)
			if err != nil {
				loopErr = err
				break
			}
			loopErr = c.accept(f)
			completed = &result
		case <-ctx.Done():
			loopErr = context.Cause(ctx)
		case <-g.done():
			loopErr = errCoordinatorLost
		case err := <-failures:
			loopErr = err
		case f := <-frames:
			bufferedBytes.Add(-int64(len(f.raw)))
			loopErr = c.accept(f)
		case <-c.writer.ready:
		case <-expired:
		}
		if loopErr == nil {
			loopErr = advance()
		}
		if loopErr == nil && approval != nil && !approvalWriting && !g.approvals.live(approval.message) {
			loopErr = errServerRequest
		}
		if loopErr == nil && approval != nil && !approvalWriting {
			frontier, err := c.renewWork(g)
			if err == nil {
				approvalWriting = true
				done := make(chan struct{})
				approvalWorker = done
				answer := approval
				go func() {
					defer close(done)
					writeCtx, stop := context.WithTimeout(runCtx, stageBudget)
					defer stop()
					stopCaller := context.AfterFunc(answer.ctx, stop)
					defer stopCaller()
					approvalResults <- g.client.ReplyAtConsumed(writeCtx, frontier, answer.message, answer.result, answer.remoteError)
				}()
			} else if !errors.Is(err, errWorkNotReady) && !errors.Is(err, codexrpc.ErrPublicationRefused) {
				loopErr = err
			}
		}
		if loopErr == nil && completed != nil {
			loopErr = active.ctx.Err()
		}
		if loopErr == nil && completed != nil {
			// Remote errors have ordinals too. Neither success nor error escapes until
			// notifications before it are reduced and outstanding compactions durable.
			frontier, err := c.renewWork(g)
			if err == nil && frontier.Through >= completed.response.Ordinal {
				release(*completed)
			}
			if err != nil && !errors.Is(err, errWorkNotReady) && !errors.Is(err, codexrpc.ErrPublicationRefused) {
				loopErr = err
			}
		}
	}
	c.fail(loopErr)
	if approval != nil {
		approval.done <- loopErr
	}
	if active != nil {
		release(workResult{err: loopErr})
	}
	cancel()
	if stageCancel != nil {
		stageCancel()
	}
	shutdown, stopShutdown := context.WithTimeout(context.Background(), shutdownBudget)
	defer stopShutdown()
	report := serveReport{Cause: c.cause, WorkerJoined: workerDone == nil}
	transport, transportErr := g.client.Stop(shutdown)
	report.Transport = transport
	writerErr := c.writer.join(shutdown)
	report.WriterJoined = writerErr == nil
	select {
	case <-receiverDone:
		report.ReceiverJoined = true
	default:
		select {
		case <-receiverDone:
			report.ReceiverJoined = true
		case <-shutdown.Done():
		}
	}
	var receiverErr error
	if !report.ReceiverJoined {
		receiverErr = errReceiveShutdown
	}
	var workerErr error
	if workerDone != nil {
		select {
		case <-workerDone:
			report.WorkerJoined = true
		case <-shutdown.Done():
			workerErr = errors.New("coverage work loop shutdown unverified")
		}
	}
	var approvalErr error
	if approvalWorker != nil {
		select {
		case <-approvalWorker:
		case <-shutdown.Done():
			approvalErr = errReceiveShutdown
		}
	}
	report.WorkerJoined = report.WorkerJoined && approvalErr == nil
	report.CleanupError = errors.Join(transportErr, writerErr, receiverErr, workerErr, approvalErr)
	return report
}
