package codexrpc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestConsumedRenewalAllowsRepeatedCallsWithoutRewritingPublication(t *testing.T) {
	w := newWire()
	c := client(t, w, "consumed", DefaultLimits())
	b, _ := c.Admit()
	p, err := c.Publish(b, 0, digest(7))
	if err != nil {
		t.Fatal(err)
	}
	f, err := c.RenewConsumed(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := uint64(1); i <= 2; i++ {
		go func() {
			var request map[string]json.RawMessage
			_ = json.Unmarshal(<-w.out, &request)
			raw, _ := json.Marshal(map[string]any{"id": request["id"], "result": map[string]any{}})
			w.in <- packet{raw: raw}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		r, err := c.CallAtConsumed(ctx, f, "thread/read", nil)
		cancel()
		if err != nil || r.Ordinal != i {
			t.Fatalf("call %d: %+v %v", i, r, err)
		}
		if _, err := c.CallAtConsumed(context.Background(), f, "thread/read", nil); !errors.Is(err, ErrPublicationRefused) {
			t.Fatal("response did not stale the consumed frontier", err)
		}
		f, err = c.RenewConsumed(p, r.Ordinal)
		if err != nil {
			t.Fatal(err)
		}
		if current, ok := c.Current(); !ok || current != p || current.Through != 0 {
			t.Fatal("renewal rewrote immutable journal evidence")
		}
	}
	if _, err := c.RenewConsumed(p, 1); !errors.Is(err, ErrPublicationRefused) {
		t.Fatal("live frontier moved backwards")
	}
	if _, err := c.RenewConsumed(p, 3); !errors.Is(err, ErrPublicationRefused) {
		t.Fatal("undelivered frontier accepted")
	}
	if err := c.Revoke(b); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CallAtConsumed(context.Background(), f, "thread/read", nil); !errors.Is(err, ErrPublicationRefused) {
		t.Fatal("revoked frontier admitted work", err)
	}
}

func TestConsumedFrontierRejectsNewFramesAndSupersededPublication(t *testing.T) {
	w := newWire()
	c := client(t, w, "consumed-frames", DefaultLimits())
	b, _ := c.Admit()
	p, _ := c.Publish(b, 0, digest(1))
	f, _ := c.RenewConsumed(p, 0)
	through := notify(t, w, c, 1)
	if _, err := c.CallAtConsumed(context.Background(), f, "thread/read", nil); !errors.Is(err, ErrPublicationRefused) {
		t.Fatal("new frame did not block old live frontier")
	}
	f, err := c.RenewConsumed(p, through)
	if err != nil {
		t.Fatal(err)
	}
	newPublication, _ := c.Publish(b, through, digest(2))
	if _, err := c.CallAtConsumed(context.Background(), f, "thread/read", nil); !errors.Is(err, ErrPublicationRefused) {
		t.Fatal("old publication's frontier survived republish")
	}
	if _, err := c.RenewConsumed(p, through); !errors.Is(err, ErrPublicationRefused) {
		t.Fatal("old receipt renewed after republish")
	}
	if _, err := c.RenewConsumed(newPublication, through); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.out:
		t.Fatal("refused work reached wire")
	default:
	}
}
