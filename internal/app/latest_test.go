package app

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/meshcrawler/hrx/internal/catalog"
)

func TestLatestCatalogPreservesAccessibleFallback(t *testing.T) {
	old := CatalogRow{ID: "gpt-5.5-sol", Route: "old", Harnesses: map[string]CatalogAccess{"codex": {Status: "ready"}, "opencode": {Status: "ready"}}}
	newer := CatalogRow{ID: "gpt-5.6-sol", Route: "new", Harnesses: map[string]CatalogAccess{"codex": {Status: "not-authorized"}, "opencode": {Status: "ready"}}}
	rows := map[string]CatalogRow{"old": old, "new": newer}
	if len(latestCatalogRows(rows, []string{"codex", "opencode"})) != 2 {
		t.Fatal("lost newest accessible Codex option")
	}
	if len(latestCatalogRows(rows, []string{"opencode"})) != 1 {
		t.Fatal("failed to hide older OpenCode generation")
	}
	newer.Harnesses["opencode"] = CatalogAccess{Status: "unavailable"}
	if len(latestCatalogRows(rows, []string{"opencode"})) != 2 {
		t.Fatal("unavailable successor hid usable model")
	}
}

func TestLatestListsCompletionAndCatalog(t *testing.T) {
	s := configure(t)
	snap := snapshot()
	snap.Binding = s.Binding
	base := snap.Models[0]
	snap.Models = nil
	for _, id := range []string{"gpt-5.5-sol", "gpt-5.6-sol"} {
		m := base
		m.ID = id
		m.Route = id
		snap.Models = append(snap.Models, m)
		snap.Inventory = append(snap.Inventory, catalog.Entry{ID: id, Route: id, Available: true, Authorized: true, Harnesses: m.Harnesses})
	}
	if err := catalog.Write(s.CachePath, snap); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"_complete", "models", "codex", "--latest"}, {"models", "--harness", "codex", "--latest"}, {"models", "--latest"}, {"catalog", "--harness", "codex", "--latest", "--json"}} {
		var out, errOut bytes.Buffer
		if err := Run(context.Background(), args, &out, &errOut); err != nil {
			t.Fatal(args, err)
		}
		if strings.Contains(out.String(), "gpt-5.5-sol") || !strings.Contains(out.String(), "gpt-5.6-sol") {
			t.Fatalf("wrong latest list: %v %s", args, out.String())
		}
		if args[0] == "catalog" {
			var report CatalogReport
			json.Unmarshal(out.Bytes(), &report)
			if report.IdentityCount != 1 || report.SourceCount != 1 || report.ReadyRoutes["codex"] != 1 || !report.LatestOnly {
				t.Fatal("incorrect filtered counts")
			}
		}
	}
	stored, _ := catalog.Read(s.CachePath)
	if len(stored.Models) != 2 {
		t.Fatal("filter changed cache")
	}
	o, err := ParseLaunch([]string{"--latest", "--model", "gpt-5.5-sol"})
	if err != nil {
		t.Fatal(err)
	}
	launch, err := BuildLaunch(s, snap, o)
	if err != nil || launch.Model != "gpt-5.5-sol" {
		t.Fatal("explicit older model blocked", err)
	}
}
