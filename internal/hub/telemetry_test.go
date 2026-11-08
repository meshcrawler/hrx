package hub

import (
	"context"
	"github.com/meshcrawler/hrx/internal/monitor"
	"testing"
	"time"
)

func TestPostgresTelemetryRoundtrip(t *testing.T) {
	s := testStore(t)
	h := testHost(t, s, "a")
	ctx := context.Background()
	current, legacy := record(h, "b"), record(h, "c")
	current.SessionID = "native-session"
	now := time.Now().UTC()
	limit := int64(2000)
	current.Metrics = &monitor.SessionMetrics{Session: current.SessionID, Source: "codex-jsonl", Version: "0.163.0", Availability: "available", CollectedAt: now, Context: &monitor.ContextMetric{Used: 800, Limit: &limit, At: now, Accuracy: "estimate"}, Compactions: &monitor.CompactionMetric{Count: 2, Coverage: "observed", At: now}}
	b := Batch{Version: Version, Sequence: 1, Records: []monitor.Record{current, legacy}}
	if err := s.Ingest(ctx, h, b); err != nil {
		t.Fatal(err)
	}
	if err := s.Ingest(ctx, h, b); err != nil {
		t.Fatal("idempotent telemetry replay", err)
	}
	snapshot, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Agents) != 2 {
		t.Fatal("mixed old/new observations lost")
	}
	for _, row := range snapshot.Agents {
		if row.RunID == current.RunID {
			if row.Metrics == nil || row.Metrics.Context.Used != 800 || *row.Metrics.Context.Limit != 2000 || row.Metrics.Compactions.Count != 2 || row.Metrics.Session != current.SessionID {
				t.Fatalf("metrics changed in hub: %+v", row.Metrics)
			}
		} else if row.Metrics != nil {
			t.Fatal("legacy run acquired foreign telemetry")
		}
	}
	current.Metrics.Session = "other-session"
	b.Sequence = 2
	b.Records = []monitor.Record{current}
	if err := b.validate(h); err != ErrInvalid {
		t.Fatal("cross-session telemetry accepted", err)
	}
}
