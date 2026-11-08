package coveragecontroller

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/meshcrawler/hrx/internal/coveragejournal"
)

type controlledStore struct {
	once       sync.Once
	entered    chan coveragejournal.Stage
	release    chan struct{}
	durability coveragejournal.Durability
	err        error
}

func (s *controlledStore) Append(_ context.Context, stage coveragejournal.Stage) (coveragejournal.Durability, error) {
	s.entered <- stage
	<-s.release
	return s.durability, s.err
}
func (s *controlledStore) unblock() { s.once.Do(func() { close(s.release) }) }
func fakeStore() *controlledStore {
	return &controlledStore{entered: make(chan coveragejournal.Stage, 1), release: make(chan struct{}), durability: coveragejournal.Durable}
}
func stagedSample() coveragejournal.Stage {
	return coveragejournal.Stage{Epoch: "epoch", Connection: "nonce", Generation: "generation", Revision: 1, Operation: "operation", Prefix: 4, Payload: []byte("evidence")}
}

func TestStagingSingleFlightAndImmutableCandidate(t *testing.T) {
	s := fakeStore()
	w := newStageWriter(s)
	t.Cleanup(func() { s.unblock(); w.join(context.Background()) })
	stage := stagedSample()
	if err := w.begin(context.Background(), stage); err != nil {
		t.Fatal(err)
	}
	stage.Payload[0] = 'X'
	got := <-s.entered
	if string(got.Payload) != "evidence" {
		t.Fatal("mutable candidate")
	}
	if err := w.begin(context.Background(), stagedSample()); !errors.Is(err, errStageBusy) {
		t.Fatal(err)
	}
	if _, ok := w.poll(); ok {
		t.Fatal("result before storage completion")
	}
	s.unblock()
	// This channel receive is a deterministic handoff, not a time-based poll.
	result := <-w.results
	w.results <- result
	if err := w.begin(context.Background(), stagedSample()); !errors.Is(err, errStageBusy) {
		t.Fatal("unread result allowed second stage", err)
	}
	result, ok := w.poll()
	if !ok || result.err != nil || result.durability != coveragejournal.Durable || result.digest == [32]byte{} {
		t.Fatal(result, ok)
	}
}

func TestStagingBlockedStoreDoesNotPreventDrainingOrHonestShutdown(t *testing.T) {
	s := fakeStore()
	w := newStageWriter(s)
	t.Cleanup(func() { s.unblock(); w.join(context.Background()) })
	if err := w.begin(context.Background(), stagedSample()); err != nil {
		t.Fatal(err)
	}
	<-s.entered
	stream := testStream(t)
	if err := stream.accept(nativeItem(1, "item/started", "item")); err != nil {
		t.Fatal(err)
	}
	if err := stream.accept(nativeItem(2, "item/completed", "item")); err != nil {
		t.Fatal(err)
	}
	if n, err := stream.prefix.frontier(); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	w.halt()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.join(ctx); !errors.Is(err, errStageShutdown) {
		t.Fatal("blocked writer called clean", err)
	}
	s.unblock()
	if err := w.join(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, ok := w.poll()
	if !ok || !errors.Is(result.err, context.Canceled) {
		t.Fatal("late success accepted", result, ok)
	}
	if err := w.begin(context.Background(), stagedSample()); !errors.Is(err, errStageStopped) {
		t.Fatal("writer resurrected", err)
	}
}

func TestStagingCanceledAndIndeterminateResultsCannotRetry(t *testing.T) {
	for _, kind := range []string{"canceled", "indeterminate", "absent-without-error"} {
		t.Run(kind, func(t *testing.T) {
			s := fakeStore()
			if kind == "indeterminate" {
				s.durability = coveragejournal.Indeterminate
				s.err = errors.New("sync error")
			}
			if kind == "absent-without-error" {
				s.durability = coveragejournal.Absent
			}
			w := newStageWriter(s)
			t.Cleanup(func() { s.unblock(); w.join(context.Background()) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := w.begin(ctx, stagedSample()); err != nil {
				t.Fatal(err)
			}
			<-s.entered
			if kind == "canceled" {
				cancel()
			}
			s.unblock()
			<-w.done
			result, ok := w.poll()
			if !ok || result.err == nil {
				t.Fatal(result, ok)
			}
			if err := w.begin(context.Background(), stagedSample()); !errors.Is(err, errStageStopped) {
				t.Fatal(err)
			}
		})
	}
}

func TestStagingCancellationAfterDurableResultBeforePoll(t *testing.T) {
	s := fakeStore()
	w := newStageWriter(s)
	t.Cleanup(func() { s.unblock(); w.join(context.Background()) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.begin(ctx, stagedSample()); err != nil {
		t.Fatal(err)
	}
	<-s.entered
	s.unblock()
	result := <-w.results
	if result.err != nil || result.durability != coveragejournal.Durable {
		t.Fatal(result)
	}
	w.results <- result
	cancel()
	result, ok := w.poll()
	if !ok || !errors.Is(result.err, context.Canceled) || result.durability != coveragejournal.Durable {
		t.Fatal("canceled durable result accepted, or durable evidence relabeled absent", result, ok)
	}
	if err := w.begin(context.Background(), stagedSample()); !errors.Is(err, errStageStopped) {
		t.Fatal(err)
	}
}

// Real filesystem exercise: GitLab only, excluded from local pure-test runs.
func TestDiskStagingWorkerPreservesHistoricalEvidence(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := coveragejournal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	w := newStageWriter(j)
	t.Cleanup(func() { w.join(context.Background()); j.Close() })
	if err := w.begin(context.Background(), stagedSample()); err != nil {
		t.Fatal(err)
	}
	result := <-w.results
	w.results <- result
	result, ok := w.poll()
	if !ok || result.err != nil || result.durability != coveragejournal.Durable {
		t.Fatal(result, ok)
	}
	if err := w.join(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := coveragejournal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	history := reopened.History()
	if len(history) != 1 || history[0].Revision != result.stage.Revision || history[0].Prefix != 4 || string(history[0].Payload) != "evidence" {
		t.Fatal("staged evidence mismatch", history)
	}
}
