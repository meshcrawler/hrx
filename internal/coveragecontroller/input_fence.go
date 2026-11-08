package coveragecontroller

import (
	"context"
	"sync"
)

// inputFence serializes recovery against input writers, not against the sole
// native receiver, reducer, journal writer or server-request answers. Waiting
// recovery takes precedence over new shared leases. The zero value is usable.
type inputFence struct {
	mu      sync.Mutex
	readers int
	writers int
	owned   bool
	changed chan struct{}
}

func (f *inputFence) signal() {
	if f.changed != nil {
		close(f.changed)
	}
	f.changed = make(chan struct{})
}

func (f *inputFence) acquire(ctx context.Context, exclusive bool) (func(), error) {
	f.mu.Lock()
	if f.changed == nil {
		f.changed = make(chan struct{})
	}
	if exclusive {
		f.writers++
		f.signal()
	}
	for {
		if err := ctx.Err(); err != nil {
			if exclusive {
				f.writers--
				f.signal()
			}
			f.mu.Unlock()
			return nil, err
		}
		if !f.owned && (exclusive && f.readers == 0 || !exclusive && f.writers == 0) {
			if exclusive {
				f.writers--
				f.owned = true
			} else {
				f.readers++
			}
			f.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					f.mu.Lock()
					defer f.mu.Unlock()
					if exclusive {
						f.owned = false
					} else {
						f.readers--
					}
					f.signal()
				})
			}, nil
		}
		changed := f.changed
		f.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		f.mu.Lock()
	}
}

// exclusiveInput is the continuation integration seam. Callers must retain the
// lease through their owned read witnesses, pending verification, journal commit
// and native submission. Cancellation never releases a still-running operation.
func (h *gateway) exclusiveInput(ctx context.Context) (func(), error) {
	if ctx == nil {
		return nil, errAttention
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return nil, errAttention
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := func() bool { return false }
	if h.ctx != nil {
		stop = context.AfterFunc(h.ctx, cancel)
		if h.ctx.Err() != nil {
			cancel()
		}
	}
	release, err := h.input.acquire(ctx, true)
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	return func() { release(); stop(); cancel() }, nil
}
