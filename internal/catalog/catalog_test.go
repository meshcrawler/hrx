package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t testing.TB) (Dashboard, Authorization, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	var d Dashboard
	err := json.Unmarshal([]byte(`{"schema_version":1,"generated_at":"2026-10-11T00:00:00Z","coverage":{"providers":[{"source":"gateway","ok":true}]},"models":[{"canonical_id":"test-model","available":true,"sources":[{"gateway_route":"provider/test-model","available":true,"details":{"harnesses":{"codex":{"wire_api":"responses","reasoning_levels":["low","high"],"default_reasoning":"low"}}}}]}]}`), &d)
	if err != nil {
		t.Fatal(err)
	}
	var a Authorization
	if err := json.Unmarshal([]byte(`{"data":[{"id":"provider/test-model"}]}`), &a); err != nil {
		t.Fatal(err)
	}
	return d, a, now
}

func TestJoinAuthorizationAndCapabilities(t *testing.T) {
	d, a, now := fixture(t)
	s, err := Join(d, a, "binding", now)
	if err != nil || len(s.Models) != 1 {
		t.Fatalf("%+v %v", s, err)
	}
	m, err := s.Resolve("test-model", "codex")
	if err != nil || m.Route != "provider/test-model" {
		t.Fatalf("%+v %v", m, err)
	}
	if got := strings.Join(s.Names("codex"), ","); got != "test-model" {
		t.Fatal(got)
	}
	if _, err = s.Resolve("test-model", "other"); err == nil {
		t.Fatal("unsupported harness resolved")
	}
	a.Data = nil
	s, err = Join(d, a, "binding", now)
	if err != nil || len(s.Models) != 0 {
		t.Fatal("unauthorized routes retained")
	}
	_, a, _ = fixture(t)
	d.Models[0].Sources[0].Details.Harnesses = nil
	s, err = Join(d, a, "binding", now)
	if err != nil || len(s.Models) != 0 {
		t.Fatal("unknown capability accepted")
	}
}

func TestJoinPreservesSeparateInputTotalAndOutputLimits(t *testing.T) {
	d, a, now := fixture(t)
	d.Models[0].Sources[0].Details.Context = 922000
	d.Models[0].Sources[0].Details.ContextLength = 1050000
	d.Models[0].Sources[0].Details.Output = 128000
	s, err := Join(d, a, "binding", now)
	if err != nil {
		t.Fatal(err)
	}
	m := s.Models[0]
	if m.Context != 922000 || m.TotalContext != 1050000 || m.Output != 128000 {
		t.Fatalf("limits conflated: %+v", m)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var restored Snapshot
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Models[0].TotalContext != 1050000 {
		t.Fatal("cache lost total capacity")
	}
	d.Models[0].Sources[0].Details.Context = 0
	s, err = Join(d, a, "binding", now)
	if err != nil || s.Models[0].Context != 0 {
		t.Fatalf("unknown input inferred from total: %+v %v", s, err)
	}
}

func TestJoinRejectsStaleFailedAndConflictingSource(t *testing.T) {
	for _, kind := range []string{"stale", "future", "failed", "schema", "conflict", "effort"} {
		t.Run(kind, func(t *testing.T) {
			d, a, now := fixture(t)
			switch kind {
			case "stale":
				d.GeneratedAt = now.Add(-MaxAge - time.Second)
			case "future":
				d.GeneratedAt = now.Add(time.Hour)
			case "failed":
				d.Coverage.Providers[0].OK = false
			case "schema":
				d.Version = 2
			case "conflict":
				m := d.Models[0]
				m.ID = "other"
				d.Models = append(d.Models, m)
			case "effort":
				cap := d.Models[0].Sources[0].Details.Harnesses["codex"]
				cap.Reasoning = []string{"invalid"}
				d.Models[0].Sources[0].Details.Harnesses["codex"] = cap
			}
			if _, err := Join(d, a, "binding", now); err == nil {
				t.Fatal("invalid catalog accepted")
			}
		})
	}
}

func TestDedupAndAliasPreference(t *testing.T) {
	d, a, now := fixture(t)
	d.Models[0].Sources = append(d.Models[0].Sources, d.Models[0].Sources[0])
	s, err := Join(d, a, "binding", now)
	if err != nil || len(s.Models) != 1 {
		t.Fatalf("dedup failed %v", err)
	}
	public := s.Models[0]
	public.Route = "test-model"
	s.Models = append(s.Models, public)
	m, err := s.Resolve("test-model", "codex")
	if err != nil || m.Route != "test-model" {
		t.Fatalf("lost alias %v", err)
	}
	s.Models[1].Route = "another/test-model"
	if _, err := s.Resolve("test-model", "codex"); err == nil {
		t.Fatal("ambiguous route guessed")
	}
	if len(s.Names("codex")) != 2 {
		t.Fatal("ambiguous routes not completable")
	}
}

func TestSharedSourceChangesNeedNoClientModelList(t *testing.T) {
	d, a, now := fixture(t)
	newModel := d.Models[0]
	newModel.ID = "brand-new-model"
	newModel.Sources = append([]Source(nil), newModel.Sources...)
	newModel.Sources[0].Route = "new-provider/brand-new-model"
	d.Models = append(d.Models, newModel)
	a.Data = append(a.Data, a.Data[0])
	a.Data[1].ID = newModel.Sources[0].Route
	s, err := Join(d, a, "binding", now)
	if err != nil || len(s.Models) != 2 {
		t.Fatalf("new model not discovered: %v", err)
	}
	if m, err := s.Resolve("brand-new-model", "codex"); err != nil || m.Route != "new-provider/brand-new-model" {
		t.Fatalf("new identity not resolved: %v", err)
	}
	a.Data = a.Data[:1]
	s, err = Join(d, a, "binding", now)
	if err != nil || len(s.Models) != 1 {
		t.Fatal("revoked route persisted")
	}
	d.Models[0].Sources[0].Available = false
	s, err = Join(d, a, "binding", now)
	if err != nil || len(s.Models) != 0 {
		t.Fatal("removed source persisted")
	}
}

func TestAtomicCacheAndBinding(t *testing.T) {
	d, a, now := fixture(t)
	s, _ := Join(d, a, "binding", now)
	path := filepath.Join(t.TempDir(), "nested", "catalog.json")
	if err := Write(path, s); err != nil {
		t.Fatal(err)
	}
	r, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Validate("binding", now, false); err != nil {
		t.Fatal(err)
	}
	if err = r.Validate("changed", now, false); err == nil {
		t.Fatal("foreign binding accepted")
	}
	if err = r.Validate("binding", now.Add(time.Hour), false); err == nil {
		t.Fatal("stale accepted")
	}
	if err = r.Validate("binding", now.Add(time.Hour), true); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("unsafe cache permissions %o", info.Mode().Perm())
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err == nil {
		t.Fatal("partial JSON accepted")
	}
}

func BenchmarkResolve(b *testing.B) {
	d, a, now := fixture(b)
	s, _ := Join(d, a, "binding", now)
	for b.Loop() {
		_, _ = s.Resolve("test-model", "codex")
	}
}
