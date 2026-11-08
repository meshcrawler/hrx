package backend

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func leaseStoreFixture(t *testing.T) (StateStore, Reservation) {
	t.Helper()
	s := StateStore{Directory: filepath.Join(t.TempDir(), "state")}
	r, e := s.Reserve("worker", "host", strings.Repeat("a", 64))
	if e != nil {
		t.Fatal(e)
	}
	return s, r
}
func TestStoredLeaseConcurrentReuseAndRevocation(t *testing.T) {
	s, r := leaseStoreFixture(t)
	var mints, deletes atomic.Int32
	b := brokerFor(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "DELETE" {
			deletes.Add(1)
			w.WriteHeader(204)
			return
		}
		mints.Add(1)
		json.NewEncoder(w).Encode(leaseResponse())
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := s.AcquireLease(context.Background(), r, leaseRequest(), b); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if mints.Load() != 1 {
		t.Fatalf("minted %d grants", mints.Load())
	}
	path := filepath.Join(s.Directory, r.DeploymentID+".lease.json")
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("credential permissions")
	}
	if e := s.RevokeLease(context.Background(), r, b); e != nil {
		t.Fatal(e)
	}
	if e := s.RevokeLease(context.Background(), r, b); e != nil {
		t.Fatal(e)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "private-") {
		t.Fatal("tombstone retains credentials")
	}
	if _, e := s.AcquireLease(context.Background(), r, leaseRequest(), b); e == nil {
		t.Fatal("revoked lease resurrected")
	}
	if deletes.Load() != 1 || mints.Load() != 1 {
		t.Fatal("duplicate teardown or mint")
	}
}
func TestLostMintResponseCanBeRevoked(t *testing.T) {
	s, r := leaseStoreFixture(t)
	var calls int
	var ids []string
	revoked := false
	b := brokerFor(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "DELETE" {
			revoked = true
			w.WriteHeader(204)
			return
		}
		calls++
		ids = append(ids, req.Header.Get("Idempotency-Key"))
		if calls == 1 {
			w.WriteHeader(500)
			return
		} // Accepted upstream, reply lost.
		json.NewEncoder(w).Encode(leaseResponse())
	})
	if _, e := s.AcquireLease(context.Background(), r, leaseRequest(), b); e == nil {
		t.Fatal("expected ambiguous failure")
	}
	if e := s.RevokeLease(context.Background(), r, b); e != nil {
		t.Fatal(e)
	}
	if !revoked || len(ids) != 2 || ids[0] != ids[1] {
		t.Fatal("lost grant was not recovered with same request ID")
	}
}
func TestExpiredStoredLeaseDoesNotSilentlyMint(t *testing.T) {
	s, r := leaseStoreFixture(t)
	count := 0
	b := brokerFor(t, func(w http.ResponseWriter, req *http.Request) { count++; json.NewEncoder(w).Encode(leaseResponse()) })
	if _, e := s.AcquireLease(context.Background(), r, leaseRequest(), b); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(s.Directory, r.DeploymentID+".lease.json")
	saved, e := readLeaseRecord(path)
	if e != nil {
		t.Fatal(e)
	}
	saved.Lease.ExpiresAt = time.Now().Add(-time.Minute)
	if e = writeLeaseRecord(path, saved); e != nil {
		t.Fatal(e)
	}
	if _, e = s.AcquireLease(context.Background(), r, leaseRequest(), b); !errors.Is(e, ErrLeaseExpired) {
		t.Fatalf("expiry not surfaced: %v", e)
	}
	if count != 1 {
		t.Fatal("expired grant silently replaced")
	}
}

func TestReplacementGenerationRetryAndRevokeOrder(t *testing.T) {
	s, res := leaseStoreFixture(t)
	ctx := context.Background()
	var sequence, ids []string
	loseReplacement := true
	broker := brokerFor(t, func(w http.ResponseWriter, r *http.Request) {
		sequence = append(sequence, r.Method)
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		id := r.Header.Get("Idempotency-Key")
		ids = append(ids, id)
		if id == leaseRequestID(res.DeploymentID, 1) && loseReplacement {
			loseReplacement = false
			w.WriteHeader(500)
			return
		}
		response := leaseResponse()
		response["lease_id"] = id
		json.NewEncoder(w).Encode(response)
	})
	original, err := s.AcquireLease(ctx, res, leaseRequest(), broker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReplaceLease(ctx, res, 1, broker); err == nil {
		t.Fatal("expected lost response")
	}
	replaced, err := s.ReplaceLease(ctx, res, 1, broker)
	if err != nil {
		t.Fatal(err)
	}
	if replaced.ID == original.ID || ids[1] != ids[2] || ids[0] == ids[1] {
		t.Fatal("replacement idempotency binding failed")
	}
	if strings.Join(sequence, ",") != "POST,DELETE,POST,POST" {
		t.Fatalf("unexpected ordering: %v", sequence)
	}
	again, err := s.ReplaceLease(ctx, res, 1, broker)
	if err != nil || again.ID != replaced.ID {
		t.Fatal("replacement retry changed grant")
	}
	reused, err := s.AcquireLease(ctx, res, leaseRequest(), broker)
	if err != nil || reused.ID != replaced.ID {
		t.Fatal("ordinary retry did not use replacement")
	}
	if len(sequence) != 4 {
		t.Fatal("retry contacted broker unnecessarily")
	}
	if _, err = s.ReplaceLease(ctx, res, 3, broker); err == nil {
		t.Fatal("skipped generation")
	}
	if err = s.RevokeLease(ctx, res, broker); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReplaceLease(ctx, res, 1, broker); err == nil {
		t.Fatal("resurrected revoked generation")
	}
}

func TestReplacementRevokeFailureDoesNotMint(t *testing.T) {
	s, res := leaseStoreFixture(t)
	ctx := context.Background()
	mints := 0
	broker := brokerFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(503)
			return
		}
		mints++
		json.NewEncoder(w).Encode(leaseResponse())
	})
	if _, err := s.AcquireLease(ctx, res, leaseRequest(), broker); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceLease(ctx, res, 1, broker); err == nil {
		t.Fatal("accepted failed revocation")
	}
	if mints != 1 {
		t.Fatal("minted before confirmed revocation")
	}
	saved, err := readLeaseRecord(filepath.Join(s.Directory, res.DeploymentID+".lease.json"))
	if err != nil || saved.Generation != 0 || saved.Revoked {
		t.Fatal("lost original grant journal")
	}
}
