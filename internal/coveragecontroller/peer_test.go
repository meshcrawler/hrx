package coveragecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func newPeerWire() *controllerWire {
	return &controllerWire{in: make(chan []byte, 8), out: make(chan []byte, 8), closed: make(chan struct{})}
}

func TestPeerBulletinRefusalDoesNotCloseOwner(t *testing.T) {
	g, native := actualGate(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	store := fakeStore()
	defer store.unblock()
	h, err := newGateway(ctx, g, store, "fixture", time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close(ctx)
	for _, source := range []workSource{boardSource, directMessageSource} {
		peer := newPeerWire()
		done := make(chan error, 1)
		go func() { done <- h.servePeer(ctx, source, peer) }()
		peer.in <- []byte(`{"id":17,"method":"thread/queue/add","params":{"threadId":"thread","input":[]}}`)
		select {
		case raw := <-peer.out:
			var result struct {
				ID    int
				Error json.RawMessage
			}
			if json.Unmarshal(raw, &result) != nil || result.ID != 17 || len(result.Error) == 0 {
				t.Fatal(string(raw))
			}
		case <-ctx.Done():
			t.Fatal("missing correlated refusal")
		}
		_ = peer.Close()
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("peer did not join")
		}
		if h.state.Load() != 0 {
			t.Fatal("auxiliary disconnect killed owner")
		}
	}
	select {
	case raw := <-native.out:
		t.Fatal("early native write", string(raw))
	default:
	}
}

func TestPeerSingleTerminalAndDisconnectRevokesOwner(t *testing.T) {
	g, _ := actualGate(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	store := fakeStore()
	defer store.unblock()
	h, err := newGateway(ctx, g, store, "fixture", time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close(ctx)
	peer := newPeerWire()
	done := make(chan error, 1)
	go func() { done <- h.servePeer(ctx, terminalSource, peer) }()
	// A reduced event proves the terminal has claimed the single consumer.
	if err := h.events.push(frame{raw: []byte(`{"method":"fixture"}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-peer.out:
	case <-ctx.Done():
		t.Fatal("terminal did not claim events")
	}
	other := newPeerWire()
	if err := h.servePeer(ctx, terminalSource, other); !errors.Is(err, errRouterBusy) {
		t.Fatal(err)
	}
	select {
	case <-other.closed:
	default:
		t.Fatal("rejected terminal left open")
	}
	_ = peer.Close()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("terminal did not join")
	}
	select {
	case <-h.done:
	case <-ctx.Done():
		t.Fatal("terminal loss failed to revoke owner")
	}
	if h.state.Load() != 2 {
		t.Fatal("owner remains live")
	}
}

func TestPeerTerminalEventFailureReachesReadyAndReport(t *testing.T) {
	g, _ := actualGate(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	store := fakeStore()
	defer store.unblock()
	h, err := newGateway(ctx, g, store, "fixture", time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close(ctx)
	peer := newPeerWire()
	joined := make(chan error, 1)
	go func() { joined <- h.servePeer(ctx, terminalSource, peer) }()

	// A bootstrap event failure can close the terminal stream before its
	// startup caller publishes the cause. The terminal worker must preserve
	// that failure rather than replace it with generic cancellation.
	h.events.stop(errThreadBoundary)
	select {
	case err := <-joined:
		if !errors.Is(err, errThreadBoundary) {
			t.Fatal("terminal lost original event failure", err)
		}
	case <-ctx.Done():
		t.Fatal("terminal workers did not join")
	}
	runtime := &Runtime{gateway: h}
	if _, err := runtime.Ready(ctx); !errors.Is(err, errThreadBoundary) {
		t.Fatal("Ready masked terminal bootstrap failure", err)
	}
	if !errors.Is(h.report.Cause, errThreadBoundary) ||
		!errors.Is(context.Cause(h.ctx), errThreadBoundary) {
		t.Fatal("owner report masked terminal bootstrap failure", h.report.Cause)
	}
}
