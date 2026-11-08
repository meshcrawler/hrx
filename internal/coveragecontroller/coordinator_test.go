package coveragecontroller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/meshcrawler/hrx/internal/coveragejournal"
)

type fakePublicationGate struct {
	id        connectionID
	revision  uint64
	lost      chan struct{}
	revoked   bool
	published []uint64
	onRevoke  func()
}

func (g *fakePublicationGate) identity() (connectionID, uint64) { return g.id, g.revision }
func (g *fakePublicationGate) done() <-chan struct{}            { return g.lost }
func (g *fakePublicationGate) publish(n uint64, _ [32]byte) error {
	if g.revoked {
		return errCoordinatorLost
	}
	select {
	case <-g.lost:
		return errCoordinatorLost
	default:
	}
	g.published = append(g.published, n)
	return nil
}
func (g *fakePublicationGate) revoke() error {
	g.revoked = true
	if g.onRevoke != nil {
		g.onRevoke()
	}
	return nil
}
func setupCoordinator(t *testing.T) (*coordinator, *fakePublicationGate, *controlledStore) {
	t.Helper()
	g := &fakePublicationGate{id: testConnection, revision: 1, lost: make(chan struct{})}
	s := fakeStore()
	c, err := newCoordinator(g, testStream(t), s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.unblock(); c.stop(context.Background()) })
	return c, g, s
}
func feedPair(t *testing.T, c *coordinator, start uint64, item string) {
	t.Helper()
	if err := c.accept(nativeItem(start, "item/started", item)); err != nil {
		t.Fatal(err)
	}
	if err := c.accept(nativeItem(start+1, "item/completed", item)); err != nil {
		t.Fatal(err)
	}
}
func finishStage(t *testing.T, c *coordinator, s *controlledStore) {
	t.Helper()
	s.unblock()
	result := <-c.writer.results
	c.writer.results <- result
}

func TestCoordinatorStagesBeforePublishingAndContinuesDraining(t *testing.T) {
	c, g, s := setupCoordinator(t)
	feedPair(t, c, 1, "first")
	if err := c.pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	stage := <-s.entered
	if stage.Revision != 1 || stage.Operation != "1" || stage.Prefix != 2 {
		t.Fatal(stage)
	}
	feedPair(t, c, 3, "second")
	if err := c.pump(context.Background()); err != nil || len(g.published) != 0 {
		t.Fatal("published before durable", err, g.published)
	}
	finishStage(t, c, s)
	if err := c.pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(g.published) != 1 || g.published[0] != 2 {
		t.Fatal(g.published)
	}
	stage = <-s.entered
	if stage.Operation != "2" || stage.Prefix != 4 {
		t.Fatal(stage)
	}
}

func TestCoordinatorLossBeforeLateStageNeverPublishes(t *testing.T) {
	c, g, s := setupCoordinator(t)
	feedPair(t, c, 1, "item")
	c.pump(context.Background())
	<-s.entered
	close(g.lost)
	if err := c.pump(context.Background()); !errors.Is(err, errCoordinatorLost) || !g.revoked {
		t.Fatal(err)
	}
	finishStage(t, c, s)
	if err := c.pump(context.Background()); err == nil || len(g.published) != 0 {
		t.Fatal("late stage resurrected", err)
	}
}

func TestCoordinatorThreadBoundaryRevokesBeforeLateStage(t *testing.T) {
	for _, method := range []string{"thread/closed", "thread/archived", "thread/deleted", "thread/reverted", "thread/settings/updated"} {
		t.Run(method, func(t *testing.T) {
			c, g, store := setupCoordinator(t)
			feedPair(t, c, 1, "item")
			if err := c.pump(context.Background()); err != nil {
				t.Fatal(err)
			}
			<-store.entered
			raw := []byte(fmt.Sprintf(`{"method":%q,"params":{"threadId":"thread"}}`, method))
			if err := c.accept(frame{testConnection, 3, notificationFrame, raw}); err == nil || !g.revoked {
				t.Fatal("thread boundary retained publication authority", err)
			}
			finishStage(t, c, store)
			if err := c.pump(context.Background()); err == nil || len(g.published) != 0 {
				t.Fatal("late durable stage restored invalidated thread", err)
			}
		})
	}
}

func TestCoordinatorThreadCloseRevokesExistingPublication(t *testing.T) {
	for _, method := range []string{"thread/closed", "thread/settings/updated"} {
		t.Run(method, func(t *testing.T) {
			c, g, store := setupCoordinator(t)
			feedPair(t, c, 1, "item")
			if err := c.pump(context.Background()); err != nil {
				t.Fatal(err)
			}
			<-store.entered
			finishStage(t, c, store)
			if err := c.pump(context.Background()); err != nil || len(g.published) != 1 {
				t.Fatal("missing pre-close publication", err)
			}
			raw := []byte(fmt.Sprintf(`{"method":%q,"params":{"threadId":"thread"}}`, method))
			if err := c.accept(frame{testConnection, 3, notificationFrame, raw}); err == nil || !g.revoked {
				t.Fatal("closed thread retained existing publication", err)
			}
			if err := c.pump(context.Background()); err == nil || len(g.published) != 1 {
				t.Fatal("closed thread republished", err)
			}
		})
	}
}

func TestCoordinatorTimeoutRevokesBeforeBlockedWorkerJoin(t *testing.T) {
	c, g, s := setupCoordinator(t)
	feedPair(t, c, 1, "item")
	ctx, cancel := context.WithCancel(context.Background())
	c.pump(ctx)
	<-s.entered
	g.onRevoke = func() {
		select {
		case <-c.writer.done:
			t.Error("revocation waited on worker")
		default:
		}
	}
	cancel()
	if err := c.pump(context.Background()); !errors.Is(err, context.Canceled) || !g.revoked {
		t.Fatal(err)
	}
	if err := c.stop(ctx); !errors.Is(err, errStageShutdown) {
		t.Fatal("blocked worker called joined", err)
	}
	finishStage(t, c, s)
	if len(g.published) != 0 {
		t.Fatal(g.published)
	}
}

func TestCoordinatorMismatchedDurableResultFailsClosed(t *testing.T) {
	c, g, s := setupCoordinator(t)
	feedPair(t, c, 1, "item")
	c.pump(context.Background())
	<-s.entered
	s.unblock()
	result := <-c.writer.results
	result.stage.Revision++
	c.writer.results <- result
	if err := c.pump(context.Background()); !errors.Is(err, errStageBinding) || !g.revoked || len(g.published) != 0 {
		t.Fatal(err)
	}
}

// Hold the result after the real journal has synced, not before a fake write.
type afterSyncStore struct {
	journal    *coveragejournal.Journal
	synced     chan struct{}
	release    chan struct{}
	once       sync.Once
	syncedOnce sync.Once
}

func (s *afterSyncStore) Append(ctx context.Context, stage coveragejournal.Stage) (coveragejournal.Durability, error) {
	d, err := s.journal.Append(ctx, stage)
	s.syncedOnce.Do(func() { close(s.synced) })
	<-s.release
	return d, err
}
func (s *afterSyncStore) unblock() { s.once.Do(func() { close(s.release) }) }

// Real disk/process-state integration stays on GitLab, excluded from local pure tests.
func TestDiskCoordinatorLossAfterSyncBeforePublication(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := coveragejournal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := &afterSyncStore{journal: j, synced: make(chan struct{}), release: make(chan struct{})}
	g := &fakePublicationGate{id: testConnection, revision: 1, lost: make(chan struct{})}
	c, err := newCoordinator(g, testStream(t), store)
	if err != nil {
		j.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { store.unblock(); c.stop(context.Background()); j.Close() })
	feedPair(t, c, 1, "item")
	if err := c.pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-store.synced
	if len(j.History()) != 1 {
		t.Fatal("stage not actually durable at barrier")
	}
	close(g.lost)
	if err := c.pump(context.Background()); !errors.Is(err, errCoordinatorLost) || !g.revoked {
		t.Fatal(err)
	}
	store.unblock()
	if err := c.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(g.published) != 0 {
		t.Fatal("published after loss", g.published)
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
	if len(history) != 1 || history[0].Prefix != 2 || history[0].Connection != testConnection.nonce {
		t.Fatal(history)
	}
	// The same generation text on a new connection does not import that stage.
	newID := testConnection
	newID.nonce = "successor"
	newGate := &fakePublicationGate{id: newID, revision: 1, lost: make(chan struct{})}
	newStream, err := newStream(newID, "thread", 8, 8192, 2048, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	successor, err := newCoordinator(newGate, newStream, reopened)
	if err != nil {
		t.Fatal(err)
	}
	defer successor.stop(context.Background())
	if err := successor.pump(context.Background()); err != nil || len(newGate.published) != 0 {
		t.Fatal("history restored authority", err)
	}
	if n, err := successor.stream.prefix.frontier(); err != nil || n != 0 {
		t.Fatal("history restored consumed prefix", n, err)
	}
}
