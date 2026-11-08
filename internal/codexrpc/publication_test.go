package codexrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
)

// In-memory only, like client_test.go: no sockets, native CLI, DB or provider.

func notify(t *testing.T, w *fakeWire, c *Client, n int) uint64 {
	t.Helper()
	var through uint64
	for range n {
		w.in <- packet{raw: []byte(`{"method":"item/started","params":{}}`)}
		through = next(t, c).Ordinal
	}
	return through
}

func digest(b byte) [32]byte { return [32]byte{b} }

func TestPublicationRequiresExactOpenAdmissionAndDeliveredPrefix(t *testing.T) {
	w := newWire()
	c := client(t, w, "gate", DefaultLimits())
	if _, err := c.Publish(Binding{}, 0, digest(1)); !errors.Is(err, ErrPublicationRefused) {
		t.Fatal("publication without admission accepted")
	}
	b, err := c.Admit()
	if err != nil || b.Epoch == "" || b.Nonce == "" || b.Generation != "gate" || b.Revision != 1 {
		t.Fatalf("admission %+v %v", b, err)
	}
	through := notify(t, w, c, 2)
	if _, err := c.Publish(b, through+1, digest(1)); !errors.Is(err, ErrPublicationRefused) {
		t.Fatal("publication beyond the delivered prefix accepted")
	}
	p, err := c.Publish(b, through, digest(1))
	if err != nil || p.Through != through || p.Binding != b || p.Sequence != 1 {
		t.Fatalf("publication %+v %v", p, err)
	}
	if _, err := c.Publish(b, through-1, digest(2)); !errors.Is(err, ErrPublicationRefused) {
		t.Fatal("publication moved backwards")
	}
	forged := b
	forged.Epoch = "other-process"
	if _, err := c.Publish(forged, through, digest(2)); !errors.Is(err, ErrPublicationRefused) {
		t.Fatal("foreign process epoch accepted")
	}
	current, ok := c.Current()
	if !ok || current != p {
		t.Fatal("current publication is not the installed value")
	}
	p.Digest = digest(9) // the returned value is a copy, not shared authority
	if again, _ := c.Current(); again.Digest != digest(1) {
		t.Fatal("caller mutation changed the published value")
	}
}

func TestEveryFinishPathClearsPublicationBeforeDone(t *testing.T) {
	paths := map[string]func(t *testing.T, w *fakeWire, c *Client){
		"read error": func(t *testing.T, w *fakeWire, c *Client) { w.in <- packet{err: io.EOF} },
		"protocol":   func(t *testing.T, w *fakeWire, c *Client) { w.in <- packet{raw: []byte(`{"result":{}}`)} },
		"overflow": func(t *testing.T, w *fakeWire, c *Client) {
			// The reader stops at overflow, so the sender must not block past Done.
			go func() {
				for range c.limits.Messages + 1 {
					select {
					case w.in <- packet{raw: []byte(`{"method":"item/started"}`)}:
					case <-c.Done():
						return
					}
				}
			}()
		},
		"stop": func(t *testing.T, w *fakeWire, c *Client) { c.Stop(context.Background()) },
		"canceled rpc": func(t *testing.T, w *fakeWire, c *Client) {
			ctx, cancel := context.WithCancel(context.Background())
			go func() { receive(t, w.out); cancel() }()
			c.Call(ctx, "thread/read", nil)
		},
	}
	for name, finish := range paths {
		t.Run(name, func(t *testing.T) {
			w := newWire()
			limits := DefaultLimits()
			if name == "overflow" {
				limits.Messages = 2
			}
			c := client(t, w, "finish-"+name[:4], limits)
			b, _ := c.Admit()
			through := notify(t, w, c, 1)
			if _, err := c.Publish(b, through, digest(1)); err != nil {
				t.Fatal(err)
			}
			finish(t, w, c)
			untilDone(t, c)
			// Done is closed only after finishLocked cleared authority under mu.
			if _, ok := c.Current(); ok {
				t.Fatal("publication survived terminal invalidation")
			}
			if _, err := c.Publish(b, through, digest(2)); err == nil {
				t.Fatal("publication accepted after terminal invalidation")
			}
			if _, err := c.Admit(); err == nil {
				t.Fatal("admission reopened after terminal invalidation")
			}
		})
	}
}

func TestPublicationIncludesDeliveredRPCResponses(t *testing.T) {
	for _, mode := range []string{"response-only", "notification-before-response", "remote-error"} {
		t.Run(mode, func(t *testing.T) {
			w := newWire()
			c := client(t, w, "response-gate", DefaultLimits())
			b, err := c.Admit()
			if err != nil {
				t.Fatal(err)
			}
			returned := make(chan outcome, 1)
			go func() { r, err := c.Call(context.Background(), "thread/read", nil); returned <- outcome{r, err} }()
			var request struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(receive(t, w.out), &request); err != nil {
				t.Fatal(err)
			}
			if mode == "notification-before-response" {
				w.in <- packet{raw: []byte(`{"method":"thread/started","params":{}}`)}
			}
			reply := map[string]any{"id": request.ID, "result": map[string]any{}}
			if mode == "remote-error" {
				delete(reply, "result")
				reply["error"] = map[string]any{"code": -1, "message": "rejected"}
			}
			raw, _ := json.Marshal(reply)
			w.in <- packet{raw: raw}
			got := receive(t, returned)
			if mode == "remote-error" {
				if !errors.Is(got.err, ErrRemote) {
					t.Fatal(got.err)
				}
			} else if got.err != nil {
				t.Fatal(got.err)
			}
			if got.response.Ordinal == 0 {
				t.Fatal("missing response ordinal")
			}
			if mode == "notification-before-response" {
				// Consume the earlier notification after Call returned: its lower
				// ordinal must not move the delivered sanity bound backwards.
				if n := next(t, c).Ordinal; n >= got.response.Ordinal {
					t.Fatal(n, got.response.Ordinal)
				}
			}
			if _, err := c.Publish(b, got.response.Ordinal, digest(7)); err != nil {
				t.Fatal("response prefix refused", err)
			}
		})
	}
}

func TestRevokeBeforeLatePublishKeepsTransportOpen(t *testing.T) {
	w := newWire()
	c := client(t, w, "revoke", DefaultLimits())
	b, _ := c.Admit()
	through := notify(t, w, c, 1)
	if _, err := c.Publish(b, through, digest(1)); err != nil {
		t.Fatal(err)
	}
	// A controller-only invalidation lands while a staged candidate is still in flight.
	if err := c.Revoke(b); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Current(); ok {
		t.Fatal("revoked publication still current")
	}
	if _, err := c.Publish(b, through, digest(2)); !errors.Is(err, ErrPublicationRefused) {
		t.Fatal("late stage published after revocation")
	}
	if c.Snapshot().State != TransportOpen {
		t.Fatal("revocation closed the transport")
	}
	if err := c.Revoke(b); !errors.Is(err, ErrNotAdmitted) {
		t.Fatal("stale revocation accepted")
	}
	next, err := c.Admit()
	if err != nil || next.Revision != b.Revision+1 {
		t.Fatalf("readmission %+v %v", next, err)
	}
	if err := c.Revoke(b); !errors.Is(err, ErrNotAdmitted) {
		t.Fatal("a stale binding revoked the newer admission")
	}
	if _, err := c.Publish(next, through, digest(3)); err != nil {
		t.Fatal("new admission cannot publish")
	}
}

func TestRevisionReplacementAndExhaustionFailClosed(t *testing.T) {
	w := newWire()
	c := client(t, w, "revisions", DefaultLimits())
	first, _ := c.Admit()
	through := notify(t, w, c, 1)
	c.Publish(first, through, digest(1))
	second, _ := c.Admit()
	if second.Revision != first.Revision+1 {
		t.Fatal("revision reused")
	}
	if _, ok := c.Current(); ok {
		t.Fatal("superseded admission kept its publication")
	}
	if _, err := c.Publish(first, through, digest(2)); !errors.Is(err, ErrPublicationRefused) {
		t.Fatal("superseded binding published")
	}
	c.mu.Lock()
	c.revision = ^uint64(0) - 1
	c.mu.Unlock()
	last, err := c.Admit()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Admit(); !errors.Is(err, ErrRevisionsExhausted) {
		t.Fatal("revision exhaustion did not fail closed")
	}
	if _, err := c.Publish(last, through, digest(3)); !errors.Is(err, ErrPublicationRefused) {
		t.Fatal("exhaustion left the last admission open")
	}
}

func TestSameGenerationOnNewClientIsNotTheSameAuthority(t *testing.T) {
	oldWire, newWireFixture := newWire(), newWire()
	old := client(t, oldWire, "same-generation", DefaultLimits())
	successor := client(t, newWireFixture, "same-generation", DefaultLimits())
	stale, _ := old.Admit()
	fresh, _ := successor.Admit()
	if stale.Nonce == fresh.Nonce || stale.Epoch != fresh.Epoch {
		t.Fatal("clients share a nonce or the process epoch differs")
	}
	through := notify(t, newWireFixture, successor, 1)
	if _, err := successor.Publish(stale, through, digest(1)); !errors.Is(err, ErrPublicationRefused) {
		t.Fatal("old client's binding published on a same-generation successor")
	}
	if _, ok := successor.Current(); ok {
		t.Fatal("successor reports coverage it never published")
	}
}

func TestConcurrentReadersNeverSeeAuthorityAfterDone(t *testing.T) {
	w := newWire()
	c := client(t, w, "readers", DefaultLimits())
	b, _ := c.Admit()
	through := notify(t, w, c, 1)
	var wg sync.WaitGroup
	violations := make(chan string, 64)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				done := false
				select {
				case <-c.Done():
					done = true
				default:
				}
				p, ok := c.Current()
				if ok && (p.Binding != b || p.Through != through) {
					violations <- "torn or foreign publication"
				}
				if done {
					if ok {
						violations <- "publication observed after Done"
					}
					return
				}
			}
		}()
	}
	for i := range 50 {
		c.Publish(b, through, digest(byte(i)))
	}
	w.in <- packet{err: io.EOF}
	untilDone(t, c)
	wg.Wait()
	close(violations)
	for v := range violations {
		t.Fatal(v)
	}
}
