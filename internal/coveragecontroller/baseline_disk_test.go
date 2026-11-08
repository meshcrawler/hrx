package coveragecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/coveragejournal"
)

// GitLab only: actual gate, real journal, owned receive loop, fake transport.
func TestDiskBaselineStagePublicationAndLoss(t *testing.T) {
	for _, loseAfterSync := range []bool{false, true} {
		name := "publish-after-sync"
		if loseAfterSync {
			name = "loss-after-sync-before-result"
		}
		t.Run(name, func(t *testing.T) {
			g, wire := actualGate(t)
			b := collectFixtureBootstrap(t, g, wire)
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
			budget, cancelBudget := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelBudget()
			c, err := newBaselineCoordinator(budget, g, b, store, 8, 2)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reports := make(chan serveReport, 1)
			go func() { reports <- c.serve(ctx, g, time.Second, 100*time.Millisecond) }()
			select {
			case <-store.synced:
			case <-budget.Done():
				t.Fatal("baseline journal sync timed out")
			}
			if _, ok := g.client.Current(); ok {
				t.Fatal("sync alone published baseline")
			}
			if loseAfterSync {
				wire.Close()
			} else {
				store.unblock()
				waitCurrent(t, g)
				current, ok := g.client.Current()
				if !ok || current.Binding != g.binding || current.Through != 5 {
					t.Fatal("wrong baseline publication", current)
				}
				cancel()
			}
			r := serveResult(t, reports)
			if r.Cause == nil || !r.ReceiverJoined {
				t.Fatal(r)
			}
			if loseAfterSync {
				if r.WriterJoined || !errors.Is(r.CleanupError, errStageShutdown) {
					t.Fatal("stuck baseline writer reported joined", r)
				}
			} else if !r.WriterJoined || r.CleanupError != nil {
				t.Fatal(r)
			}
			store.unblock()
			if err := c.writer.join(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, ok := g.client.Current(); ok {
				t.Fatal("late baseline restored authority")
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
			if len(history) != 1 || history[0].Prefix != 5 || history[0].Revision != g.binding.Revision || history[0].Connection != g.binding.Nonce || history[0].Operation != "1" {
				t.Fatal("baseline history lost its binding", history)
			}
			var evidence baselineEvidence
			if json.Unmarshal(history[0].Payload, &evidence) != nil || evidence.Kind != "fresh-baseline" || evidence.Thread != "thread" || evidence.Session != "session" {
				t.Fatal("bad baseline history", evidence)
			}
		})
	}
}
