package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/meshcrawler/hrx/internal/monitor"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("HRX_HUB_TEST_DSN")
	if dsn == "" {
		t.Skip("use tests/integration/hub_store.py for disposable PostgreSQL tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 12)
	if _, err = rand.Read(b); err != nil {
		t.Fatal(err)
	}
	schema := "hrx_test_" + hex.EncodeToString(b)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		if err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	s := &Store{Pool: p, LeaseTTL: 45 * time.Second, StaleAfter: 45 * time.Second}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func testHost(t *testing.T, s *Store, id string) Host {
	t.Helper()
	ctx := context.Background()
	token := strings.Repeat(id, 64)
	e := Enrollment{Version: Version, HostID: strings.Repeat(id, 32), Name: "host-" + id, Prefixes: []string{"team/"}, TokenDigest: Digest(token)}
	if err := s.Enroll(ctx, e); err != nil {
		t.Fatal(err)
	}
	h, err := s.Authenticate(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func record(h Host, id string) monitor.Record {
	now := time.Now().UTC()
	return monitor.Record{RunID: strings.Repeat(id, 32), HostID: h.ID, Host: "untrusted display", Agent: "team/dev/agent", Harness: "codex", Model: "fixture", StartedAt: now, LastSeen: now, LastActivity: now, Activity: "launch", Status: "running", Credential: monitor.Credential{Provider: "unmanaged"}}
}

func TestPostgresSnapshotOwnership(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	h := testHost(t, s, "a")
	r := record(h, "b")
	lease, err := s.Acquire(ctx, h, LeaseRequest{Version: Version, Identity: r.Agent, RunID: r.RunID})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Ingest(ctx, h, Batch{Version: Version, Sequence: 1, Records: []monitor.Record{r}}); err != nil {
		t.Fatal(err)
	}
	check := func(state string, generation int64) {
		t.Helper()
		snapshot, err := s.Snapshot(ctx)
		if err != nil || len(snapshot.Agents) != 1 {
			t.Fatalf("snapshot: %+v %v", snapshot, err)
		}
		ownership := snapshot.Agents[0].Ownership
		if ownership == nil || ownership.State != state || ownership.Generation != generation {
			t.Fatalf("ownership: %+v", ownership)
		}
		if (ownership.ExpiresAt == nil) != (state == "superseded") {
			t.Fatalf("expiry must belong to this run: %+v", ownership)
		}
	}
	check("held", lease.Generation)
	if err = s.Release(ctx, h, LeaseRequest{Version: Version, Identity: r.Agent, RunID: r.RunID, Generation: lease.Generation}); err != nil {
		t.Fatal(err)
	}
	check("inactive", lease.Generation)
	next := record(h, "c")
	_, err = s.Acquire(ctx, h, LeaseRequest{Version: Version, Identity: next.Agent, RunID: next.RunID})
	if err != nil {
		t.Fatal(err)
	}
	check("superseded", lease.Generation)
}

func TestPostgresEnrollmentAndRevocation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	h := testHost(t, s, "a")
	e := Enrollment{Version: Version, HostID: h.ID, Name: h.Name, Prefixes: h.Prefixes, TokenDigest: h.TokenDigest}
	if err := s.Enroll(ctx, e); err != nil {
		t.Fatal("retry", err)
	}
	e.Prefixes = []string{"other/"}
	if err := s.Enroll(ctx, e); !errors.Is(err, ErrConflict) {
		t.Fatal("enrollment expanded authority", err)
	}
	if err := s.Rotate(ctx, h.ID, Digest(strings.Repeat("new", 32))); err != nil {
		t.Fatal(err)
	}
	if err := s.Heartbeat(ctx, h, nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("stale authenticated credential remained usable", err)
	}
	newHost, err := s.Authenticate(ctx, strings.Repeat("new", 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Heartbeat(ctx, newHost, nil); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revoked host remained usable", err)
	}
	if _, err := s.Authenticate(ctx, strings.Repeat("new", 32)); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revoked credential authenticated", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("migration not restart-safe", err)
	}
}

func TestPostgresObservationReplayExitAndFreshness(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	h := testHost(t, s, "a")
	r := record(h, "b")
	b := Batch{Version: Version, Sequence: 1, Records: []monitor.Record{r}}
	if err := s.Ingest(ctx, h, b); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, h, b); err != nil {
		t.Fatal("identical retry rejected", err)
	}
	changed := b
	changed.Records = append([]monitor.Record(nil), b.Records...)
	changed.Records[0].Model = "changed"
	if err := s.Ingest(ctx, h, changed); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting retry accepted", err)
	}
	snap, err := s.Snapshot(ctx)
	if err != nil || len(snap.Agents) != 1 || snap.Agents[0].Status != "stale" {
		t.Fatal("backlog invented liveness", err)
	}
	if err = s.Heartbeat(ctx, h, []string{r.RunID}); err != nil {
		t.Fatal(err)
	}
	snap, err = s.Snapshot(ctx)
	if err != nil || snap.Agents[0].Status != "running" || snap.Agents[0].Host != h.Name {
		t.Fatal("heartbeat/host binding failed", err)
	}
	if !snap.Agents[0].LastActivity.Equal(r.LastActivity) {
		t.Fatal("heartbeat invented activity")
	}
	ended := r
	ended.Status = "exited"
	now := time.Now().UTC()
	ended.EndedAt = &now
	if err = s.Ingest(ctx, h, Batch{Version: Version, Sequence: 2, Records: []monitor.Record{ended}}); err != nil {
		t.Fatal(err)
	}
	if err = s.Ingest(ctx, h, Batch{Version: Version, Sequence: 3, Records: []monitor.Record{r}}); err != nil {
		t.Fatal(err)
	}
	if err = s.Ingest(ctx, h, b); err != nil {
		t.Fatal("old duplicate replay failed", err)
	}
	if err = s.Heartbeat(ctx, h, []string{r.RunID}); err != nil {
		t.Fatal(err)
	}
	snap, err = s.Snapshot(ctx)
	if err != nil || snap.Agents[0].Status != "exited" || snap.Agents[0].EndedAt == nil {
		t.Fatal("ended run resurrected", err)
	}
	if _, err = s.Acquire(ctx, h, LeaseRequest{Version: Version, Identity: r.Agent, RunID: r.RunID}); !errors.Is(err, ErrConflict) {
		t.Fatal("ended run reacquired identity", err)
	}
}

func TestPostgresObservationScopeAndAtomicity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	h := testHost(t, s, "a")
	r := record(h, "b")
	bad := record(h, "c")
	bad.Agent = "other/dev/agent"
	if err := s.Ingest(ctx, h, Batch{Version: Version, Sequence: 1, Records: []monitor.Record{r, bad}}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("scope accepted", err)
	}
	snap, err := s.Snapshot(ctx)
	if err != nil || len(snap.Agents) != 0 {
		t.Fatal("partial batch committed", err)
	}
	bad = r
	bad.HostID = strings.Repeat("d", 32)
	if err = s.Ingest(ctx, h, Batch{Version: Version, Sequence: 1, Records: []monitor.Record{bad}}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("host impersonation accepted", err)
	}
	if err = s.Ingest(ctx, h, Batch{Version: Version, Sequence: 1, Records: []monitor.Record{r}}); err != nil {
		t.Fatal("failed transaction consumed sequence", err)
	}
	r.Agent = "team/different/agent"
	if err = s.Ingest(ctx, h, Batch{Version: Version, Sequence: 2, Records: []monitor.Record{r}}); !errors.Is(err, ErrConflict) {
		t.Fatal("run identity rewritten", err)
	}
}

func TestPostgresLeaseConcurrencyAndFencing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := testHost(t, s, "a")
	b := testHost(t, s, "b")
	requests := []struct {
		host    Host
		request LeaseRequest
	}{{a, LeaseRequest{Version: Version, Identity: "team/mayor/agent", RunID: strings.Repeat("c", 32)}}, {b, LeaseRequest{Version: Version, Identity: "team/mayor/agent", RunID: strings.Repeat("d", 32)}}}
	type result struct {
		index int
		lease Lease
		err   error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, item := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			l, err := s.Acquire(ctx, item.host, item.request)
			results <- result{i, l, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	winner := -1
	var lease Lease
	losers := 0
	for r := range results {
		if r.err == nil {
			winner = r.index
			lease = r.lease
		} else if errors.Is(r.err, ErrConflict) {
			losers++
		} else {
			t.Fatal(r.err)
		}
	}
	if winner < 0 || losers != 1 {
		t.Fatal("exclusive acquisition failed")
	}
	w := requests[winner]
	other := requests[1-winner]
	retry, err := s.Acquire(ctx, w.host, w.request)
	if err != nil || retry.Generation != lease.Generation || !retry.ExpiresAt.Equal(lease.ExpiresAt) {
		t.Fatal("acquire retry changed lease", err)
	}
	wr := w.request
	wr.Generation = lease.Generation
	if _, err = s.Renew(ctx, w.host, wr); err != nil {
		t.Fatal(err)
	}
	if err = s.Release(ctx, w.host, wr); err != nil {
		t.Fatal(err)
	}
	next, err := s.Acquire(ctx, other.host, other.request)
	if err != nil || next.Generation <= lease.Generation {
		t.Fatal("successor did not get a new generation", err)
	}
	if err = s.Release(ctx, w.host, wr); !errors.Is(err, ErrConflict) {
		t.Fatal("old release affected successor", err)
	}
	if _, err = s.Renew(ctx, w.host, wr); !errors.Is(err, ErrConflict) {
		t.Fatal("old owner renewed", err)
	}
	if _, err = s.Acquire(ctx, w.host, w.request); !errors.Is(err, ErrConflict) {
		t.Fatal("delayed acquisition revived old run", err)
	}
	nr := other.request
	nr.Generation = next.Generation
	if err = s.Release(ctx, other.host, nr); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Acquire(ctx, w.host, w.request); !errors.Is(err, ErrConflict) {
		t.Fatal("old run reacquired after successor ended", err)
	}
}

func TestPostgresLeaseExpiryUsesDatabaseTime(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := testHost(t, s, "a")
	b := testHost(t, s, "b")
	for i, h := range []Host{a, b} {
		observation := record(h, []string{"c", "d"}[i])
		stamp := time.Date([]int{2099, 1970}[i], 1, 1, 0, 0, 0, 0, time.UTC)
		observation.StartedAt, observation.LastSeen, observation.LastActivity = stamp, stamp, stamp
		if err := s.Ingest(ctx, h, Batch{Version: Version, Sequence: 1, Records: []monitor.Record{observation}}); err != nil {
			t.Fatal(err)
		}
	}
	r := LeaseRequest{Version: Version, Identity: "team/dev/agent", RunID: strings.Repeat("c", 32)}
	l, err := s.Acquire(ctx, a, r)
	if err != nil {
		t.Fatal(err)
	}
	if l.ExpiresAt.Sub(l.ServerTime) < 44*time.Second || l.ExpiresAt.Sub(l.ServerTime) > 46*time.Second {
		t.Fatal("unexpected TTL")
	}
	if time.Since(l.ServerTime).Abs() > time.Minute {
		t.Fatal("host observation clock affected lease server time")
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE hrx_hub_leases SET expires_at=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	r.Generation = l.Generation
	if _, err = s.Renew(ctx, a, r); !errors.Is(err, ErrConflict) {
		t.Fatal("expired lease renewed", err)
	}
	next := LeaseRequest{Version: Version, Identity: r.Identity, RunID: strings.Repeat("d", 32)}
	if successor, err := s.Acquire(ctx, b, next); err != nil || successor.Generation != l.Generation+1 {
		t.Fatal("expired lease unavailable", err)
	}
}
