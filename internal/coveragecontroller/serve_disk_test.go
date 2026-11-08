package coveragecontroller

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/coveragejournal"
)

// GitLab only: real filesystem journal and gate, with an in-memory transport.
func TestDiskServeDurableStageAndTransportLoss(t *testing.T) {
	for _, loseBeforeReturn := range []bool{false, true} {
		name := "published-before-loss"
		if loseBeforeReturn {
			name = "loss-before-store-return"
		}
		t.Run(name, func(t *testing.T) {
			g, wire := actualGate(t)
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			journal, err := coveragejournal.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()
			store := &afterSyncStore{journal: journal, synced: make(chan struct{}), release: make(chan struct{})}
			defer store.unblock()
			id, _ := g.identity()
			stream, err := newStream(id, "thread", 8, 8192, 2048, 8, 2)
			if err != nil {
				t.Fatal(err)
			}
			c, err := newCoordinator(g, stream, store)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reports := make(chan serveReport, 1)
			go func() { reports <- c.serve(ctx, g, 10*time.Second, 100*time.Millisecond) }()
			sendPair(wire)
			select {
			case <-store.synced:
			case <-time.After(5 * time.Second):
				t.Fatal("journal did not sync")
			}
			if _, ok := g.client.Current(); ok {
				t.Fatal("published before storage result")
			}
			if !loseBeforeReturn {
				store.unblock()
				waitCurrent(t, g)
			}
			wire.Close()
			report := serveResult(t, reports)
			if report.Cause == nil || !report.ReceiverJoined {
				t.Fatal(report)
			}
			if loseBeforeReturn {
				if report.WriterJoined || !errors.Is(report.CleanupError, errStageShutdown) {
					t.Fatal("blocked storage reported clean", report)
				}
			} else if !report.WriterJoined || report.CleanupError != nil {
				t.Fatal(report)
			}
			store.unblock()
			if err := c.writer.join(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, ok := g.client.Current(); ok {
				t.Fatal("late completion restored authority")
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := coveragejournal.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			history := reopened.History()
			if len(history) != 1 || history[0].Connection != g.binding.Nonce || history[0].Revision != g.binding.Revision || history[0].Prefix != 2 {
				t.Fatal("durable historical binding lost", history)
			}
		})
	}
}
