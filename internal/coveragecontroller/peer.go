package coveragecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/meshcrawler/hrx/internal/codexrpc"
)

// servePeer transfers ownership of a downstream connection. The listener chooses
// source, never a client field. Exactly one terminal consumes the native event
// stream; auxiliary peers receive only their own RPC replies. This adapter does
// not create a listener or activate the launcher.
func (h *gateway) servePeer(parent context.Context, source workSource, wire codexrpc.Wire) error {
	if parent == nil || wire == nil {
		return codexrpc.ErrConfig
	}
	if source != terminalSource && source != boardSource && source != directMessageSource {
		return codexrpc.ErrRequest
	}
	select {
	case h.peers <- struct{}{}:
		defer func() { <-h.peers }()
	default:
		_ = wire.Close()
		return errRouterBusy
	}
	if source == terminalSource && !h.terminal.CompareAndSwap(false, true) {
		_ = wire.Close()
		return errRouterBusy
	}
	// Terminal ownership is deliberately not reusable within a gateway lifetime.
	// Reconnection requires a fresh admission, not a second event consumer.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(h.ctx, cancel)
	defer stop()
	incoming, _ := newEventQueue(16, 8<<20)
	answers, _ := newEventQueue(16, 8<<20)
	finished := make(chan error, 4)
	var writes sync.Mutex
	write := func(raw []byte) error {
		writes.Lock()
		defer writes.Unlock()
		bounded, done := context.WithTimeout(ctx, h.stageBudget)
		defer done()
		return wire.Write(bounded, raw)
	}
	go func() {
		for {
			raw, err := wire.Read(ctx, h.router.maxFrame)
			if err != nil {
				finished <- fmt.Errorf("peer read: %w", err)
				return
			}
			if len(raw) > h.router.maxFrame {
				finished <- codexrpc.ErrRequest
				return
			}
			// Responses to native requests bypass the sequential work queue: native
			// may be waiting for this answer before completing an outstanding RPC.
			var envelope map[string]json.RawMessage
			if json.Unmarshal(raw, &envelope) == nil && envelope != nil && envelope["method"] == nil {
				if source != terminalSource {
					finished <- errServerRequest
					return
				}
				if err = answers.push(frame{raw: raw}); err != nil {
					finished <- err
					return
				}
				continue
			}
			if err = incoming.push(frame{raw: raw}); err != nil {
				finished <- err
				return
			}
		}
	}()
	go func() {
		for {
			next, err := incoming.next(ctx)
			if err != nil {
				finished <- err
				return
			}
			reply, err := h.handle(ctx, source, next.raw)
			if err != nil {
				// Valid requests get a fixed, correlated failure without leaking internal
				// journal paths. Invalid envelopes close the connection.
				var envelope map[string]json.RawMessage
				if checkJSON(next.raw) != nil || json.Unmarshal(next.raw, &envelope) != nil || !requestID(envelope["id"]) {
					finished <- err
					return
				}
				reply, _ = json.Marshal(map[string]any{"id": envelope["id"], "error": map[string]any{"code": -32001, "message": "coverage admission unavailable"}})
			}
			if len(reply) != 0 {
				if err = write(reply); err != nil {
					finished <- fmt.Errorf("peer reply write: %w", err)
					return
				}
			}
		}
	}()
	workers := 2
	if source == terminalSource {
		workers += 2
		go func() {
			for {
				response, err := answers.next(ctx)
				if err != nil {
					finished <- err
					return
				}
				if err := h.answer(ctx, source, response.raw); err != nil {
					finished <- err
					return
				}
			}
		}()
		go func() {
			for {
				event, err := h.events.next(ctx)
				if err != nil {
					finished <- err
					return
				}
				if err = write(event.raw); err != nil {
					finished <- fmt.Errorf("peer event write: %w", err)
					return
				}
			}
		}()
	}
	var cause error
	select {
	case cause = <-finished:
		workers--
	case <-ctx.Done():
		cause = ctx.Err()
	}
	cancel()
	incoming.stop(cause)
	answers.stop(cause)
	if source == terminalSource {
		h.cancelCause(cause)
	}
	closeErr := wire.Close()
	timer := time.NewTimer(h.shutdownBudget)
	defer timer.Stop()
	for workers > 0 {
		select {
		case <-finished:
			workers--
		case <-timer.C:
			return errors.Join(cause, closeErr, codexrpc.ErrShutdownUnverified)
		}
	}
	return errors.Join(cause, closeErr)
}
