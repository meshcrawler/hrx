package matrix

import (
	"reflect"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/catalog"
)

func fixture() Input {
	in := Input{HRXVersion: "test-commit", HarnessVersions: map[string]string{}, Snapshots: map[string]catalog.Snapshot{}}
	for _, harness := range []string{"codex", "claude", "opencode"} {
		in.HarnessVersions[harness] = "test-version"
		cap := catalog.Capability{WireAPI: map[string]string{"codex": "responses", "claude": "messages", "opencode": "chat_completions"}[harness], Reasoning: []string{"low", "high"}, DefaultReasoning: "low"}
		entry := catalog.Entry{ID: "model", Route: "route", Provider: "provider", Account: "account", Available: true, Authorized: true, Harnesses: map[string]catalog.Capability{harness: cap}}
		in.Snapshots[harness] = catalog.Snapshot{Version: 1, GeneratedAt: time.Unix(100, 0), RefreshedAt: time.Unix(101, 0), Inventory: []catalog.Entry{entry}}
	}
	return in
}

func TestManifestExhaustiveAxesAndDefaults(t *testing.T) {
	in := fixture()
	m, err := Generate(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Cells) != 3*3*48 {
		t.Fatalf("incomplete Cartesian product: %d", len(m.Cells))
	}
	seen := map[string]bool{}
	for _, cell := range m.Cells {
		if seen[cell.ID] {
			t.Fatal("duplicate cell ID")
		}
		seen[cell.ID] = true
		if cell.Expected != "launch" || cell.Status != "not-run" || cell.BackendCoverage != "unknown" {
			t.Fatalf("invented test/backend evidence: %+v", cell)
		}
	}
	again, _ := Generate(in)
	if !reflect.DeepEqual(m, again) {
		t.Fatal("manifest is not deterministic")
	}
}

func TestManifestPreservesAccountsAndNegativeCases(t *testing.T) {
	in := fixture()
	s := in.Snapshots["codex"]
	duplicate := s.Inventory[0]
	duplicate.Upstream = "duplicate-alias"
	other := s.Inventory[0]
	other.Account = "second-account"
	other.Authorized = false
	s.Inventory = append(s.Inventory, duplicate, other)
	in.Snapshots["codex"] = s
	m, err := Generate(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Cells) != 4*3*48 {
		t.Fatal("source collapsed or duplicated", len(m.Cells))
	}
	negative := 0
	for _, cell := range m.Cells {
		if cell.Account == "second-account" {
			negative++
			if cell.Expected != "reject" || cell.Reason != "not-authorized" || cell.Status != "not-run" {
				t.Fatal("unauthorized tuple omitted or counted as tested")
			}
		}
	}
	if negative != 3*48 {
		t.Fatal("negative matrix incomplete")
	}
}

func TestManifestRejectsIncompleteOrConflictingInputs(t *testing.T) {
	for _, mutate := range []func(*Input){
		func(in *Input) { delete(in.Snapshots, "claude") },
		func(in *Input) { in.HRXVersion = "" },
		func(in *Input) { s := in.Snapshots["codex"]; s.Inventory = nil; in.Snapshots["codex"] = s },
		func(in *Input) {
			s := in.Snapshots["codex"]
			e := s.Inventory[0]
			e.Authorized = false
			s.Inventory = append(s.Inventory, e)
			in.Snapshots["codex"] = s
		},
	} {
		in := fixture()
		mutate(&in)
		if _, err := Generate(in); err == nil {
			t.Fatal("incomplete/conflicting input accepted")
		}
	}
}
