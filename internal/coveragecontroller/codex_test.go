package coveragecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/meshcrawler/hrx/internal/codexrpc"
	"github.com/meshcrawler/hrx/internal/coveragejournal"
)

// Actual codexrpc gate, in-memory transport only: no native process or sockets.
type controllerWire struct {
	in, out chan []byte
	closed  chan struct{}
	once    sync.Once
}

func (w *controllerWire) Read(ctx context.Context, _ int) ([]byte, error) {
	select {
	case b := <-w.in:
		return b, nil
	case <-w.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (w *controllerWire) Write(ctx context.Context, b []byte) error {
	select {
	case w.out <- b:
		return nil
	case <-w.closed:
		return io.EOF
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (w *controllerWire) Close() error { w.once.Do(func() { close(w.closed) }); return nil }

func actualGate(t *testing.T) (*codexGate, *controllerWire) {
	t.Helper()
	w := &controllerWire{in: make(chan []byte, 8), out: make(chan []byte, 8), closed: make(chan struct{})}
	client, err := codexrpc.New(w, "fixture-generation", codexrpc.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := client.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	b, err := client.Admit()
	if err != nil {
		t.Fatal(err)
	}
	return &codexGate{client: client, binding: b}, w
}
func actualCoordinator(t *testing.T) (*coordinator, *codexGate, *controllerWire, *controlledStore) {
	t.Helper()
	g, w := actualGate(t)
	id, _ := g.identity()
	stream, err := newStream(id, "thread", 8, 8192, 2048, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	s := fakeStore()
	c, err := newCoordinator(g, stream, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.unblock(); c.stop(context.Background()) })
	return c, g, w, s
}
func actualPair(t *testing.T, c *coordinator, g *codexGate, w *controllerWire) {
	t.Helper()
	for _, method := range []string{"item/started", "item/completed"} {
		w.in <- nativeItem(1, method, "item").raw
		f, err := g.receive(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err = c.accept(f); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodexCoordinatorPublishesThenRevokesActualGate(t *testing.T) {
	c, g, w, s := actualCoordinator(t)
	actualPair(t, c, g, w)
	if err := c.pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-s.entered
	if _, ok := g.client.Current(); ok {
		t.Fatal("published before durable stage")
	}
	finishStage(t, c, s)
	if err := c.pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, ok := g.client.Current()
	if !ok || p.Through != 2 || p.Binding != g.binding {
		t.Fatal(p, ok)
	}
	c.fail(errors.New("controller invalidation"))
	if _, ok := g.client.Current(); ok {
		t.Fatal("publication survived controller revoke")
	}
	if g.client.Snapshot().State != codexrpc.TransportOpen {
		t.Fatal("revoke closed transport")
	}
}

func TestCodexCoordinatorTransportLossRejectsLateStage(t *testing.T) {
	c, g, w, s := actualCoordinator(t)
	actualPair(t, c, g, w)
	if err := c.pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-s.entered
	w.Close()
	<-g.client.Done()
	if _, ok := g.client.Current(); ok {
		t.Fatal("terminal gate still current")
	}
	finishStage(t, c, s)
	if err := c.pump(context.Background()); !errors.Is(err, errCoordinatorLost) {
		t.Fatal(err)
	}
	if _, ok := g.client.Current(); ok {
		t.Fatal("late durable stage published")
	}
}

func TestCodexInputMergesRemoteErrorResponseWithEarlierNotification(t *testing.T) {
	c, g, w, _ := actualCoordinator(t)
	type returned struct {
		frame frame
		err   error
	}
	results := make(chan returned, 1)
	go func() { f, err := g.call(context.Background(), "thread/read", nil); results <- returned{f, err} }()
	var request struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(<-w.out, &request); err != nil {
		t.Fatal(err)
	}
	w.in <- []byte(`{"method":"thread/started","params":{}}`)
	raw, _ := json.Marshal(map[string]any{"id": request.ID, "error": map[string]any{"code": -1, "message": "rejected"}})
	w.in <- raw
	result := <-results
	if !errors.Is(result.err, codexrpc.ErrRemote) || result.frame.ordinal != 2 {
		t.Fatal(result)
	}
	if err := c.accept(result.frame); err != nil {
		t.Fatal(err)
	}
	if n, _ := c.stream.prefix.frontier(); n != 0 {
		t.Fatal("skipped notification", n)
	}
	f, err := g.receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = c.accept(f); err != nil {
		t.Fatal(err)
	}
	if n, err := c.stream.prefix.frontier(); err != nil || n != 2 {
		t.Fatal(n, err)
	}
}

// Combined actual gate and real disk journal; GitLab only. The transport remains
// in-memory, so this does not certify a native subscription or baseline.
func TestDiskCodexGateJournalPublicationBoundaries(t *testing.T) {
	for _, mode := range []string{"loss-after-sync", "revoke-after-sync", "replace-revision", "publish-before-loss"} {
		t.Run(mode, func(t *testing.T) {
			g, w := actualGate(t)
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			j, err := coveragejournal.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			store := &afterSyncStore{journal: j, synced: make(chan struct{}), release: make(chan struct{})}
			id, _ := g.identity()
			s, err := newStream(id, "thread", 8, 8192, 2048, 8, 2)
			if err != nil {
				j.Close()
				t.Fatal(err)
			}
			c, err := newCoordinator(g, s, store)
			if err != nil {
				j.Close()
				t.Fatal(err)
			}
			t.Cleanup(func() { store.unblock(); c.stop(context.Background()); j.Close() })
			actualPair(t, c, g, w)
			if err := c.pump(context.Background()); err != nil {
				t.Fatal(err)
			}
			<-store.synced
			history := j.History()
			if len(history) != 1 || history[0].Connection != g.binding.Nonce || history[0].Revision != g.binding.Revision {
				t.Fatal("missing exact durable stage", history)
			}
			if _, ok := g.client.Current(); ok {
				t.Fatal("publication preceded store result")
			}
			var successor codexrpc.Binding
			switch mode {
			case "loss-after-sync":
				w.Close()
				<-g.client.Done()
			case "revoke-after-sync":
				c.fail(errors.New("baseline revoked"))
			case "replace-revision":
				successor, err = g.client.Admit()
				if err != nil {
					t.Fatal(err)
				}
				if _, err = g.client.Publish(successor, 2, [32]byte{9}); err != nil {
					t.Fatal(err)
				}
			}
			store.unblock()
			result := <-c.writer.results
			c.writer.results <- result
			err = c.pump(context.Background())
			current, ok := g.client.Current()
			switch mode {
			case "publish-before-loss":
				if err != nil || !ok || current.Binding != g.binding || current.Digest != stageDigest(history[0]) {
					t.Fatal("durable publication missing", current, err)
				}
				w.Close()
				<-g.client.Done()
				if _, ok := g.client.Current(); ok {
					t.Fatal("publication survived loss")
				}
			case "replace-revision":
				if !errors.Is(err, codexrpc.ErrPublicationRefused) || !ok || current.Binding != successor {
					t.Fatal("stale stage affected successor", current, err)
				}
			default:
				if err == nil || ok {
					t.Fatal("late stage published", current, err)
				}
			}
			c.stop(context.Background())
			if err := c.writer.join(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := j.Close(); err != nil {
				t.Fatal(err)
			}
			recovered, err := coveragejournal.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			history = recovered.History()
			if len(history) != 1 {
				t.Fatal("historical evidence lost", history)
			}
			newGate, _ := actualGate(t)
			if newGate.binding.Nonce == g.binding.Nonce || newGate.binding.Generation != g.binding.Generation {
				t.Fatal("invalid reconnect fixture")
			}
			newID, _ := newGate.identity()
			newStream, err := newStream(newID, "thread", 8, 8192, 2048, 8, 2)
			if err != nil {
				t.Fatal(err)
			}
			newController, err := newCoordinator(newGate, newStream, recovered)
			if err != nil {
				t.Fatal(err)
			}
			defer newController.stop(context.Background())
			if err := newController.pump(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, ok := newGate.client.Current(); ok {
				t.Fatal("historical stage restored current authority")
			}
		})
	}
}
