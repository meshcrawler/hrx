package coveragecontroller

import (
	"errors"
	"testing"
)

var testConnection = connectionID{"epoch", "nonce", "generation"}

func fixturePrefix(t *testing.T) *prefix {
	t.Helper()
	p, err := newPrefix(testConnection, 4, 64, 32)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func event(n uint64, kind frameKind) frame { return frame{testConnection, n, kind, []byte("frame")} }

func TestPrefixMergesResponseAndNotificationDeliveryOrders(t *testing.T) {
	orders := [][]uint64{{3, 2, 1}, {2, 1, 3}, {1, 3, 2}, {1, 2, 3}, {2, 3, 1}, {3, 1, 2}}
	for _, order := range orders {
		p := fixturePrefix(t)
		var reduced []uint64
		for _, n := range order {
			kind := notificationFrame
			if n == 2 {
				kind = responseFrame
			}
			if err := p.add(event(n, kind)); err != nil {
				t.Fatal(err)
			}
			for {
				f, ok, err := p.next()
				if err != nil {
					t.Fatal(err)
				}
				if !ok {
					break
				}
				before, _ := p.frontier()
				if before != uint64(len(reduced)) {
					t.Fatal("advanced before reduction")
				}
				reduced = append(reduced, f.ordinal)
				if err := p.commit(f.ordinal); err != nil {
					t.Fatal(err)
				}
			}
		}
		if len(reduced) != 3 || reduced[0] != 1 || reduced[1] != 2 || reduced[2] != 3 || p.bytes != 0 {
			t.Fatal(order, reduced)
		}
	}
}

func TestPrefixGapCannotBecomeConsumed(t *testing.T) {
	p := fixturePrefix(t)
	p.add(event(2, responseFrame))
	p.add(event(3, notificationFrame))
	if _, ok, err := p.next(); ok || err != nil {
		t.Fatal(ok, err)
	}
	if n, err := p.frontier(); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	if err := p.commit(3); !errors.Is(err, errPrefixReduction) {
		t.Fatal(err)
	}
	if n, err := p.frontier(); n != 0 || err == nil {
		t.Fatal("invalid prefix survived", n, err)
	}
}

func TestPrefixLossAfterPeekPreventsCommit(t *testing.T) {
	p := fixturePrefix(t)
	p.add(event(1, notificationFrame))
	p.next()
	canceled := errors.New("Call canceled after reply enqueue")
	p.invalidate(canceled)
	if err := p.commit(1); !errors.Is(err, canceled) {
		t.Fatal(err)
	}
	if err := p.add(event(2, responseFrame)); !errors.Is(err, canceled) {
		t.Fatal(err)
	}
	if p.bytes != 0 || len(p.pending) != 0 {
		t.Fatal("retained invalid data")
	}
}

func TestPrefixRejectsDuplicateAndReconnectedIdentity(t *testing.T) {
	for _, kind := range []string{"duplicate-pending", "duplicate-consumed", "reconnect", "zero", "bad-kind"} {
		t.Run(kind, func(t *testing.T) {
			p := fixturePrefix(t)
			p.add(event(1, notificationFrame))
			f := event(1, responseFrame)
			switch kind {
			case "duplicate-consumed":
				p.next()
				p.commit(1)
			case "reconnect":
				f.ordinal = 2
				f.connection.nonce = "new-connection"
			case "zero":
				f.ordinal = 0
			case "bad-kind":
				f.ordinal = 2
				f.kind = 0
			}
			if err := p.add(f); err == nil {
				t.Fatal("invalid frame accepted")
			}
			if _, err := p.frontier(); err == nil {
				t.Fatal("authority survived error")
			}
		})
	}
}

func TestPrefixBoundsPendingHolesAndCopiesFrames(t *testing.T) {
	p := fixturePrefix(t)
	f := event(2, responseFrame)
	p.add(f)
	f.raw[0] = 'X'
	p.add(event(1, notificationFrame))
	got, _, _ := p.next()
	got.raw[0] = 'Y'
	p.commit(1)
	got, _, _ = p.next()
	if string(got.raw) != "frame" {
		t.Fatal("caller mutated buffered frame")
	}
	for n := uint64(3); n <= 5; n++ {
		if err := p.add(event(n, notificationFrame)); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.add(event(6, serverRequestFrame)); !errors.Is(err, errPrefixLimit) {
		t.Fatal(err)
	}
	if p.bytes != 0 || len(p.pending) != 0 {
		t.Fatal("overflow retained pending data")
	}
	p = fixturePrefix(t)
	f = event(1, responseFrame)
	f.raw = make([]byte, 33)
	if err := p.add(f); !errors.Is(err, errPrefixLimit) {
		t.Fatal(err)
	}
}

func TestPrefixCannotWrapOrdinal(t *testing.T) {
	p := fixturePrefix(t)
	p.consumed = ^uint64(0)
	if _, _, err := p.next(); !errors.Is(err, errPrefixOrder) {
		t.Fatal(err)
	}
}
