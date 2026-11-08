package hub

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/monitor"
)

func TestPostgresSnapshotCapacityIsExplicit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	h := testHost(t, s, "a")
	data, _ := json.Marshal(record(h, "b"))
	if _, err := s.Pool.Exec(ctx, `INSERT INTO hrx_hub_runs(host_id,run_id,sequence,record)
SELECT $1,lpad(to_hex(n),32,'0'),1,$2::jsonb FROM generate_series(1,5001) n`, h.ID, data); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Snapshot(ctx)
	if !errors.Is(err, ErrCapacity) || len(snapshot.Agents) != 0 {
		t.Fatal("capacity overflow returned misleading partial snapshot", err)
	}
}

func TestPostgresRetentionPreservesFencing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	h := testHost(t, s, "a")
	old := record(h, "b")
	old.Status = "exited"
	live := record(h, "c")
	b1 := Batch{Version: Version, Sequence: 1, Records: []monitor.Record{old}}
	b2 := Batch{Version: Version, Sequence: 2, Records: []monitor.Record{live}}
	for _, b := range []Batch{b1, b2} {
		if err := s.Ingest(ctx, h, b); err != nil {
			t.Fatal(err)
		}
	}
	// Recreate the v1 schema shape with existing data, then apply v2.
	if _, err := s.Pool.Exec(ctx, "ALTER TABLE hrx_hub_runs DROP COLUMN archived; DELETE FROM hrx_hub_schema_migrations WHERE version=2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("v1 upgrade", err)
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE hrx_hub_runs SET received_at=clock_timestamp()-interval '40 days'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, "UPDATE hrx_hub_receipts SET received_at=clock_timestamp()-interval '40 days'"); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(ctx, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.Snapshot(ctx)
	if err != nil || len(snapshot.Agents) != 1 || snapshot.Agents[0].RunID != live.RunID {
		t.Fatalf("compaction removed active run: %+v %v", snapshot, err)
	}
	if err := s.Ingest(ctx, h, b2); err != nil {
		t.Fatal("latest retry must survive", err)
	}
	if err := s.Ingest(ctx, h, b1); !errors.Is(err, ErrConflict) {
		t.Fatal("expired receipt must fail closed", err)
	}
	old.Status = "running"
	if err := s.Ingest(ctx, h, Batch{Version: Version, Sequence: 3, Records: []monitor.Record{old}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err = s.Snapshot(ctx)
	if err != nil || len(snapshot.Agents) != 1 {
		t.Fatal("archived run revived", err)
	}
	if _, err := s.Acquire(ctx, h, LeaseRequest{Version: Version, Identity: old.Agent, RunID: old.RunID}); !errors.Is(err, ErrConflict) {
		t.Fatal("archived run reacquired", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal("migration restart", err)
	}
}
