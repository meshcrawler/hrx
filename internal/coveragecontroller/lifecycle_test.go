package coveragecontroller

import (
	"errors"
	"testing"
)

func compaction(started bool, n uint64) compactionEvent {
	return compactionEvent{testConnection, "thread", "turn", "item", n, started, int64(n) * 1000}
}
func reducer(t *testing.T) *compactions {
	t.Helper()
	c, err := newCompactions(testConnection, "thread", 2)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLifecyclePairsOnlyExactAdmittedIdentity(t *testing.T) {
	c := reducer(t)
	if p, err := c.apply(compaction(true, 4)); p != nil || err != nil {
		t.Fatal(p, err)
	}
	p, err := c.apply(compaction(false, 9))
	if err != nil || p == nil || p.startOrdinal != 4 || p.completedOrdinal != 9 || p.thread != "thread" || p.turn != "turn" || p.item != "item" {
		t.Fatal(p, err)
	}
	if _, err := c.apply(compaction(false, 10)); !errors.Is(err, errLifecycleOrder) {
		t.Fatal("duplicate accepted", err)
	}
}

func TestLifecycleNeverPairsAcrossLossOrReconnect(t *testing.T) {
	c := reducer(t)
	c.apply(compaction(true, 1))
	loss := errors.New("connection lost")
	c.invalidate(loss)
	if p, err := c.apply(compaction(false, 2)); p != nil || !errors.Is(err, loss) {
		t.Fatal(p, err)
	}
	newID := testConnection
	newID.nonce = "reconnected"
	next, err := newCompactions(newID, "thread", 2)
	if err != nil {
		t.Fatal(err)
	}
	e := compaction(false, 1)
	e.connection = newID
	if p, err := next.apply(e); p != nil || !errors.Is(err, errLifecycleOrder) {
		t.Fatal("old start reconstructed", p, err)
	}
}

func TestLifecycleRejectsWrongBindingAndIncompletePairs(t *testing.T) {
	for _, kind := range []string{"thread", "turn", "item", "nonce", "generation", "epoch", "backwards", "duplicate-start"} {
		t.Run(kind, func(t *testing.T) {
			c := reducer(t)
			c.apply(compaction(true, 5))
			e := compaction(false, 6)
			switch kind {
			case "thread":
				e.thread = "other"
			case "turn":
				e.turn = "other"
			case "item":
				e.item = "other"
			case "nonce":
				e.connection.nonce = "other"
			case "generation":
				e.connection.generation = "other"
			case "epoch":
				e.connection.epoch = "other"
			case "backwards":
				e.ordinal = 4
			case "duplicate-start":
				e.started = true
			}
			if p, err := c.apply(e); p != nil || err == nil || len(c.items) != 0 {
				t.Fatal(p, err)
			}
		})
	}
}

func TestLifecycleBoundsIncludeCompletedTombstones(t *testing.T) {
	c := reducer(t)
	for i := uint64(1); i <= 2; i++ {
		e := compaction(true, 2*i-1)
		e.item = string(rune('a' + i))
		c.apply(e)
		e.started = false
		e.ordinal++
		if _, err := c.apply(e); err != nil {
			t.Fatal(err)
		}
	}
	e := compaction(true, 5)
	e.item = "third"
	if _, err := c.apply(e); !errors.Is(err, errLifecycleLimit) {
		t.Fatal(err)
	}
}

func TestLifecycleRetainsNativeClockRegressionWithoutInventedDuration(t *testing.T) {
	c := reducer(t)
	e := compaction(true, 1)
	e.timeMS = 200
	c.apply(e)
	e.started = false
	e.ordinal = 2
	e.timeMS = 100
	p, err := c.apply(e)
	if err != nil || p.startedAtMS != 200 || p.completedAtMS != 100 {
		t.Fatal(p, err)
	}
}
