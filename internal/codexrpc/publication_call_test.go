package codexrpc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func publishedClient(t *testing.T) (*Client, *fakeWire, Published) {
	t.Helper()
	w := newWire()
	c := client(t, w, "call-publication", DefaultLimits())
	b, err := c.Admit()
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.Publish(b, notify(t, w, c, 1), digest(1))
	if err != nil {
		t.Fatal(err)
	}
	return c, w, p
}

func waitCallState(t *testing.T, c *Client, ready func(Snapshot) bool) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !ready(c.Snapshot()) {
		select {
		case <-deadline.C:
			t.Fatal("RPC state did not reach test barrier")
		case <-tick.C:
		}
	}
}

func TestCallAtPublicationRefusesChangedReceipt(t *testing.T) {
	for name, change := range map[string]func(*Published){
		"epoch":      func(p *Published) { p.Binding.Epoch = "foreign" },
		"nonce":      func(p *Published) { p.Binding.Nonce = "foreign" },
		"generation": func(p *Published) { p.Binding.Generation = "foreign" },
		"revision":   func(p *Published) { p.Binding.Revision++ },
		"sequence":   func(p *Published) { p.Sequence++ },
		"prefix":     func(p *Published) { p.Through++ },
		"digest":     func(p *Published) { p.Digest = digest(9) },
	} {
		t.Run(name, func(t *testing.T) {
			c, w, p := publishedClient(t)
			change(&p)
			if _, err := c.CallAtPublication(context.Background(), p, "thread/read", nil); !errors.Is(err, ErrPublicationRefused) {
				t.Fatal("changed publication admitted", err)
			}
			if len(w.out) != 0 || len(w.writeEntered) != 0 {
				t.Fatal("refused call reached the wire")
			}
		})
	}
}

func TestCallAtPublicationRechecksAfterWriterWait(t *testing.T) {
	testCallFenceAfterWriterWait(t, false)
}

func TestConsumedRechecksAfterWriterWait(t *testing.T) {
	testCallFenceAfterWriterWait(t, true)
}

func testCallFenceAfterWriterWait(t *testing.T, consumed bool) {
	for _, mode := range []string{"revoke", "replace", "unconsumed-frame"} {
		t.Run(mode, func(t *testing.T) {
			c, w, p := publishedClient(t)
			f, err := c.RenewConsumed(p, p.Through)
			if err != nil {
				t.Fatal(err)
			}
			<-c.writeToken
			var once sync.Once
			release := func() { once.Do(func() { c.writeToken <- struct{}{} }) }
			t.Cleanup(release)
			done := make(chan error, 1)
			go func() {
				var err error
				if consumed {
					_, err = c.CallAtConsumed(context.Background(), f, "thread/read", nil)
				} else {
					_, err = c.CallAtPublication(context.Background(), p, "thread/read", nil)
				}
				done <- err
			}()
			waitCallState(t, c, func(s Snapshot) bool { return s.RPCPending })
			switch mode {
			case "revoke":
				if err := c.Revoke(p.Binding); err != nil {
					t.Fatal(err)
				}
			case "replace":
				if _, err := c.Publish(p.Binding, p.Through, digest(2)); err != nil {
					t.Fatal(err)
				}
			case "unconsumed-frame":
				w.in <- packet{raw: []byte(`{"method":"thread/settings/updated","params":{}}`)}
				waitCallState(t, c, func(s Snapshot) bool { return s.Frames > p.Through })
			}
			release()
			if err := receive(t, done); !errors.Is(err, ErrPublicationRefused) {
				t.Fatal(err)
			}
			if len(w.out) != 0 || len(w.writeEntered) != 0 {
				t.Fatal("stale call reached wire")
			}
			untilDone(t, c)
		})
	}
}

func TestCallAtPublicationCannotRetractAdmittedWrite(t *testing.T) {
	c, w, p := publishedClient(t)
	w.writeGate = make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(w.writeGate) }) }
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() {
		_, err := c.CallAtPublication(context.Background(), p, "thread/read", nil)
		done <- err
	}()
	receive(t, w.writeEntered)
	// Revocation must not wait for external I/O or pretend to retract it.
	if err := c.Revoke(p.Binding); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Current(); ok {
		t.Fatal("publication survived revocation")
	}
	release()
	var request struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(receive(t, w.out), &request); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"id": request.ID, "result": map[string]any{}})
	w.in <- packet{raw: raw}
	if err := receive(t, done); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CallAtPublication(context.Background(), p, "thread/read", nil); !errors.Is(err, ErrPublicationRefused) {
		t.Fatal(err)
	}
}
