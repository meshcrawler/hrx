package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/meshcrawler/hrx/internal/catalog"
)

func TestCatalogDeduplicatesOnlyVisibleRows(t *testing.T) {
	s := configure(t)
	snap := snapshot()
	snap.Binding = s.Binding
	snap.Inventory = []catalog.Entry{
		{ID: "duplicate", Provider: "provider", Account: "one", Upstream: "first", Available: true},
		{ID: "duplicate", Provider: "provider", Account: "one", Upstream: "second", Available: true},
		{ID: "duplicate", Provider: "provider", Account: "two", Upstream: "third", Available: true},
		{ID: "duplicate", Provider: "provider", Account: "one", Route: "route-a", Available: true},
		{ID: "duplicate", Provider: "provider", Account: "one", Route: "route-b", Available: true},
	}
	if err := catalog.Write(s.CachePath, snap); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), []string{"catalog", "--harness", "codex"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), "duplicate"); got != 4 {
		t.Fatalf("got %d visible rows: %s", got, out.String())
	}
	out.Reset()
	if err := Run(context.Background(), []string{"catalog", "--harness", "codex", "--json"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	var report CatalogReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Models) != 5 || report.SourceCount != 5 {
		t.Fatal("lost source evidence")
	}
}

func TestFullCatalogFromOfflineCache(t *testing.T) {
	s := configure(t)
	snap := snapshot()
	snap.Binding = s.Binding
	snap.Inventory = []catalog.Entry{
		{ID: "example", Route: "provider/example", Available: true, Authorized: true, Harnesses: snap.Models[0].Harnesses},
		{ID: "not-permitted", Route: "other/model", Available: true, Harnesses: map[string]catalog.Capability{}},
		{ID: "provider-only", Provider: "another", Available: true, Harnesses: map[string]catalog.Capability{}},
	}
	if err := catalog.Write(s.CachePath, snap); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), []string{"catalog", "--harness", "codex", "--json"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	var report CatalogReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.IdentityCount != 3 || report.RouteCount != 2 || report.ReadyRoutes["codex"] != 1 {
		t.Fatalf("wrong full catalog counts: %+v", report)
	}
	out.Reset()
	if err := Run(context.Background(), []string{"catalog", "--harness", "codex", "--json", "--search", "provider-only"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(out.Bytes(), &report)
	if report.IdentityCount != 1 || report.Models[0].Harnesses["codex"].Status != "no-gateway-route" {
		t.Fatal("filter lost excluded source")
	}
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := Run(context.Background(), []string{"catalog", "--harness", "codex", "--output", path}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &report); err != nil || report.IdentityCount != 3 {
		t.Fatal("invalid catalog export", err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("unsafe export permissions")
	}
}
