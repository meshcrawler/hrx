package coveragecontroller

import (
	"context"
	"errors"
	"testing"
)

func TestEventQueuePreservesReducedOrderAndRevokesOnOverflow(t *testing.T) {
	c, g, _ := setupCoordinator(t)
	q, err := newEventQueue(1, 2048)
	if err != nil {
		t.Fatal(err)
	}
	c.stream.events = q
	later := frame{testConnection, 2, notificationFrame, []byte(`{"method":"thread/tokenUsage/updated","params":{}}`)}
	if err := c.accept(later); err != nil {
		t.Fatal(err)
	}
	select {
	case <-q.frames:
		t.Fatal("out-of-order notification escaped")
	default:
	}
	if err := c.accept(frame{testConnection, 1, responseFrame, []byte(`{"result":{}}`)}); err != nil {
		t.Fatal(err)
	}
	f, err := q.next(context.Background())
	if err != nil || f.ordinal != 2 {
		t.Fatal(f, err)
	}
	// Two unread notifications cannot block the native owner or silently drop one.
	later.ordinal = 3
	if err := c.accept(later); err != nil {
		t.Fatal(err)
	}
	later.ordinal = 4
	if err := c.accept(later); !errors.Is(err, errEventBackpressure) {
		t.Fatal(err)
	}
	if !g.revoked {
		t.Fatal("overflow did not revoke admission")
	}
	if _, err := q.next(context.Background()); !errors.Is(err, errEventBackpressure) {
		t.Fatal("stale event escaped invalidation", err)
	}
}
func TestEventQueueByteLimitAndOwnership(t *testing.T) {
	q, _ := newEventQueue(2, 3)
	f := frame{raw: []byte("abc")}
	if err := q.push(f); err != nil {
		t.Fatal(err)
	}
	f.raw[0] = 'z'
	if err := q.push(frame{raw: []byte("x")}); !errors.Is(err, errEventBackpressure) {
		t.Fatal(err)
	}
	got, err := q.next(context.Background())
	if err != nil || string(got.raw) != "abc" {
		t.Fatal(got, err)
	}
	if err := q.push(f); err != nil {
		t.Fatal("consumed bytes not released", err)
	}
}
