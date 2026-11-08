package coveragecontroller

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

var errEventBackpressure = errors.New("coverage event consumer fell behind")

// eventQueue is the bounded UI output side of the single receive owner. It
// carries only already-reduced notifications, never permission to submit work.
// Overflow invalidates the controller rather than blocking native reception.
type eventQueue struct {
	frames   chan frame
	bytes    atomic.Int64
	maxBytes int64
	done     chan struct{}
	once     sync.Once
	cause    error
}

func newEventQueue(messages, totalBytes int) (*eventQueue, error) {
	if messages <= 0 || totalBytes <= 0 {
		return nil, errPrefixLimit
	}
	return &eventQueue{frames: make(chan frame, messages), maxBytes: int64(totalBytes), done: make(chan struct{})}, nil
}
func (q *eventQueue) stop(err error) { q.once.Do(func() { q.cause = err; close(q.done) }) }
func (q *eventQueue) push(f frame) error {
	select {
	case <-q.done:
		return q.cause
	default:
	}
	size := int64(len(f.raw))
	if q.bytes.Add(size) > q.maxBytes {
		q.bytes.Add(-size)
		return errEventBackpressure
	}
	f.raw = bytes.Clone(f.raw)
	select {
	case q.frames <- f:
		return nil
	default:
		q.bytes.Add(-size)
		return errEventBackpressure
	}
}
func (q *eventQueue) next(ctx context.Context) (frame, error) {
	select {
	case <-q.done:
		return frame{}, q.cause
	default:
	}
	select {
	case <-q.done:
		return frame{}, q.cause
	case <-ctx.Done():
		return frame{}, ctx.Err()
	case f := <-q.frames:
		q.bytes.Add(-int64(len(f.raw)))
		select {
		case <-q.done:
			return frame{}, q.cause
		default:
			return f, nil
		}
	}
}
