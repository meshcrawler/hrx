package coveragecontroller

import (
	"context"
	"errors"
	"testing"
	"time"
)

func serveResult(t *testing.T, ch <-chan serveReport) serveReport {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("receive loop did not stop")
		return serveReport{}
	}
}
func waitCurrent(t *testing.T, g *codexGate) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if _, ok := g.client.Current(); ok {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("no publication")
		case <-tick.C:
		}
	}
}
func sendPair(w *controllerWire) {
	w.in <- nativeItem(1, "item/started", "item").raw
	w.in <- nativeItem(2, "item/completed", "item").raw
}

func TestServePublishesWithoutExternalPumpAndJoinsOnCancel(t *testing.T) {
	c, g, w, s := actualCoordinator(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reports := make(chan serveReport, 1)
	go func() { reports <- c.serve(ctx, g, time.Second, time.Second) }()
	sendPair(w)
	select {
	case <-s.entered:
	case r := <-reports:
		t.Fatal("loop exited before staging", r)
	}
	s.unblock()
	waitCurrent(t, g)
	cancel()
	r := serveResult(t, reports)
	if !errors.Is(r.Cause, context.Canceled) || !r.ReceiverJoined || !r.WriterJoined || r.CleanupError != nil {
		t.Fatal(r)
	}
	if _, ok := g.client.Current(); ok {
		t.Fatal("cancel left authority open")
	}
}

func TestServeStageDeadlineRevokesWhileIdleAndReportsBlockedWriter(t *testing.T) {
	c, g, w, s := actualCoordinator(t)
	reports := make(chan serveReport, 1)
	go func() { reports <- c.serve(context.Background(), g, time.Second, 20*time.Millisecond) }()
	sendPair(w)
	select {
	case <-s.entered:
	case r := <-reports:
		t.Fatal("loop exited before store entered", r)
	}
	r := serveResult(t, reports)
	if !errors.Is(r.Cause, context.DeadlineExceeded) || !r.ReceiverJoined || r.WriterJoined || !errors.Is(r.CleanupError, errStageShutdown) {
		t.Fatal(r)
	}
	if _, ok := g.client.Current(); ok {
		t.Fatal("storage timeout left current publication")
	}
	// Eventual disk completion does not rewrite the failed shutdown receipt.
	s.unblock()
	if err := c.writer.join(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.WriterJoined {
		t.Fatal("retroactive clean shutdown")
	}
}

func TestServeTransportLossDuringBlockedStage(t *testing.T) {
	c, g, w, s := actualCoordinator(t)
	reports := make(chan serveReport, 1)
	go func() { reports <- c.serve(context.Background(), g, time.Minute, 20*time.Millisecond) }()
	sendPair(w)
	select {
	case <-s.entered:
	case r := <-reports:
		t.Fatal("loop exited before staging", r)
	}
	w.Close()
	r := serveResult(t, reports)
	if r.Cause == nil || r.WriterJoined || !r.ReceiverJoined || !errors.Is(r.CleanupError, errStageShutdown) {
		t.Fatal(r)
	}
	s.unblock()
	if err := c.writer.join(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.client.Current(); ok {
		t.Fatal("late result restored publication")
	}
}
