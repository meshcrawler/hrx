package coveragecontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"github.com/meshcrawler/hrx/internal/coveragejournal"
)

var (
	errStageBusy     = errors.New("coverage stage already in flight")
	errStageStopped  = errors.New("coverage stage writer stopped")
	errStageOutcome  = errors.New("coverage stage outcome is not durable")
	errStageShutdown = errors.New("coverage stage writer shutdown unverified")
)

type stageStore interface {
	Append(context.Context, coveragejournal.Stage) (coveragejournal.Durability, error)
}
type stageTask struct {
	ctx    context.Context
	stage  coveragejournal.Stage
	digest [32]byte
}
type stageResult struct {
	ctx        context.Context
	stage      coveragejournal.Stage
	digest     [32]byte
	durability coveragejournal.Durability
	err        error
}

// stageWriter is confined to the coordinator except for run. Its single worker
// is the only caller of the store. Pending remains true until the coordinator
// consumes the result, so neither a blocked write nor an unread result permits
// an extra write. There is no unbounded queue or goroutine-per-retry fallback.
type stageWriter struct {
	tasks            chan stageTask
	results          chan stageResult
	ready            chan struct{}
	done             chan struct{}
	ctx              context.Context
	cancel           context.CancelFunc
	pending, stopped bool
}

func newStageWriter(store stageStore) *stageWriter {
	ctx, cancel := context.WithCancel(context.Background())
	w := &stageWriter{tasks: make(chan stageTask, 1), results: make(chan stageResult, 1), ready: make(chan struct{}, 1), done: make(chan struct{}), ctx: ctx, cancel: cancel}
	go w.run(store)
	return w
}

func (w *stageWriter) run(store stageStore) {
	defer close(w.done)
	for {
		select {
		case <-w.ctx.Done():
			return
		case task := <-w.tasks:
			ctx, cancel := context.WithCancel(task.ctx)
			stop := context.AfterFunc(w.ctx, cancel)
			// AfterFunc may not have run yet when stop won before this task.
			if w.ctx.Err() != nil {
				cancel()
			}
			durability, err := coveragejournal.Absent, ctx.Err()
			if err == nil {
				durability, err = store.Append(ctx, task.stage)
			}
			if err == nil {
				err = w.ctx.Err()
			}
			if err == nil {
				err = ctx.Err()
			}
			stop()
			cancel()
			if err == nil && durability != coveragejournal.Durable {
				err = errStageOutcome
			}
			w.results <- stageResult{task.ctx, task.stage, task.digest, durability, err}
			select {
			case w.ready <- struct{}{}:
			default:
			}
			if err != nil {
				return
			}
		}
	}
}

func (w *stageWriter) begin(ctx context.Context, s coveragejournal.Stage) error {
	if w.stopped {
		return errStageStopped
	}
	if w.pending {
		return errStageBusy
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(s.Payload) > coveragejournal.MaxPayload {
		return coveragejournal.ErrInvalid
	}
	for _, id := range []string{s.Epoch, s.Connection, s.Generation, s.Operation} {
		if len(id) == 0 || len(id) > 128 {
			return coveragejournal.ErrInvalid
		}
	}
	if s.Revision == 0 {
		return coveragejournal.ErrInvalid
	}
	s.Payload = bytes.Clone(s.Payload)
	encoded, err := json.Marshal(s)
	if err != nil {
		return err
	}
	w.pending = true
	w.tasks <- stageTask{ctx, s, sha256.Sum256(encoded)}
	return nil
}

// poll is nonblocking: event draining/reduction continues while storage stalls.
// Durable here is still only staging evidence; the live publication gate is a
// separate mandatory check by the coordinator after it consumes this result.
func (w *stageWriter) poll() (stageResult, bool) {
	select {
	case result := <-w.results:
		w.pending = false
		if result.err == nil {
			result.err = result.ctx.Err()
		}
		if result.err == nil {
			result.err = w.ctx.Err()
		}
		if result.err != nil {
			w.stopped = true
			w.cancel()
		}
		return result, true
	default:
		return stageResult{}, false
	}
}

// halt must follow synchronous gate revocation by the coordinator. It never
// waits on storage. A late result is historical and cannot restart this writer.
func (w *stageWriter) halt() { w.stopped = true; w.cancel() }

func (w *stageWriter) join(ctx context.Context) error {
	w.halt()
	select {
	case <-w.done:
		return nil
	default:
	}
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return errStageShutdown
	}
}
