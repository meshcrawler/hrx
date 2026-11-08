package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/catalog"
)

func TestCatalogEvidenceDatesDetailsAndStaleness(t *testing.T) {
	s := configure(t)
	snap := snapshot()
	snap.Binding = s.Binding
	cap := snap.Models[0].Harnesses["codex"]
	cap.Evidence = "live-tested"
	cap.TestedAt = time.Now().UTC().Format(time.RFC3339)
	snap.Inventory = []catalog.Entry{{ID: "example", Route: "provider/example", Provider: "openrouter", Account: "acme", Available: true, Authorized: true, Harnesses: map[string]catalog.Capability{"codex": cap}}}
	if err := catalog.Write(s.CachePath, snap); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if err := Run(context.Background(), []string{"catalog", "--harness", "codex", "--details"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"authorization checked", "auth=true", "compatibility=declared", "tested_at=" + cap.TestedAt, "ready/V", "wire=responses", "reasoning=low,high"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("missing", want, out.String())
		}
	}
	out.Reset()
	if err := Run(context.Background(), []string{"catalog", "--harness", "codex", "--json"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	var report CatalogReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Models[0].Harnesses["codex"].Verification != "verified" || report.AuthorizationCheckedAt["codex"].IsZero() {
		t.Fatal(report)
	}
	snap.RefreshedAt = time.Now().Add(-time.Hour)
	catalog.Write(s.CachePath, snap)
	out.Reset()
	if err := Run(context.Background(), []string{"catalog", "--harness", "codex"}, &out, &stderr); err == nil {
		t.Fatal("stale accepted implicitly")
	}
	out.Reset()
	if err := Run(context.Background(), []string{"catalog", "--harness", "codex", "--stale", "--compact"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "openrouter/acme[cached-V]") || !strings.Contains(out.String(), "stale=true") {
		t.Fatal(out.String())
	}
}

func TestCompletionDescriptionsOfflineEscapedAndFresh(t *testing.T) {
	s := configure(t)
	snap := snapshot()
	snap.Binding = s.Binding
	// An invalid credential helper proves completion cannot execute authentication.
	t.Setenv("HRX_TEST_KEY", "")
	snap.Models[0].ID = "gpt-5.9-sol"
	snap.Models[0].Route = "provider/gpt-5.9-sol:fast"
	old := snap.Models[0]
	newer := old
	newer.ID = "gpt-5.10-sol"
	newer.Route = "provider/gpt-5.10-sol"
	second := old
	second.Route = "other/gpt-5.9-sol"
	snap.Models = []catalog.Model{old, second, newer}
	if err := catalog.Write(s.CachePath, snap); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if err := Run(context.Background(), []string{"_complete", "model-descriptions", "codex"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`provider/gpt-5.9-sol\:fast:authorized; declared; not-tested; newer=gpt-5.10-sol`, "gpt-5.10-sol:authorized; declared; not-tested"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal(out.String())
		}
	}
	out.Reset()
	Run(context.Background(), []string{"_complete", "model-descriptions", "codex", "--latest"}, &out, &stderr)
	if strings.Contains(out.String(), "5.9") {
		t.Fatal("superseded completion", out.String())
	}
	out.Reset()
	Run(context.Background(), []string{"_complete", "reasoning-descriptions", "codex", "gpt-5.10-sol"}, &out, &stderr)
	if out.String() != "low:declared reasoning level; default\nhigh:declared reasoning level\n" {
		t.Fatal(out.String())
	}
	snap.GeneratedAt = time.Now().Add(-time.Hour)
	catalog.Write(s.CachePath, snap)
	out.Reset()
	Run(context.Background(), []string{"_complete", "model-descriptions", "codex"}, &out, &stderr)
	if out.Len() != 0 {
		t.Fatal("stale suggestions")
	}
	os.WriteFile(s.CachePath, []byte("broken-json"), 0600)
	Run(context.Background(), []string{"_complete", "model-descriptions", "codex"}, &out, &stderr)
	if out.Len() != 0 || stderr.Len() != 0 {
		t.Fatal("bad cache completion noisy")
	}
}

func TestCompactEvidenceAndAccountBoundaries(t *testing.T) {
	rows := []CatalogRow{
		{ID: "model", Route: "one", Provider: "openrouter", Account: "acme", Harnesses: map[string]CatalogAccess{"codex": {Status: "ready", Verification: "verified"}}},
		{ID: "model", Route: "two", Provider: "openrouter", Account: "acme", Harnesses: map[string]CatalogAccess{"codex": {Status: "ready", Verification: "stale-test"}}},
		{ID: "model", Route: "three", Provider: "openrouter", Account: "acme-dev", Harnesses: map[string]CatalogAccess{"codex": {Status: "not-authorized", Verification: "verified"}}},
		{ID: "model", Route: "four", Provider: "truefoundry", Account: "contoso", Harnesses: map[string]CatalogAccess{"codex": {Status: "ready", Verification: "undated-test"}}},
	}
	config := filepath.Join(t.TempDir(), "config.toml")
	labels := "[catalog.labels]\n\"openrouter/acme\" = \"OR-A\"\n\"openrouter/acme-dev\" = \"OR-AD\"\n\"truefoundry/contoso\" = \"TF-C\"\n"
	if err := os.WriteFile(config, []byte(labels), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRX_CONFIG", config)
	var out bytes.Buffer
	renderCompactCatalog(&out, rows, []string{"codex"})
	for _, want := range []string{"OR-A[V]", "OR-A[S]", "TF-C[U]"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal(out.String())
		}
	}
	if strings.Contains(out.String(), "OR-AD[V]") {
		t.Fatal("authorization crossed account boundary")
	}
}

func TestZshCompletionDescriptionsSyntax(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh unavailable")
	}
	path := filepath.Join(t.TempDir(), "_hrx")
	os.WriteFile(path, []byte(ZshCompletion), 0600)
	if output, err := exec.Command(zsh, "-n", path).CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	for _, fragment := range []string{"_describe -t models", "_describe -t reasoning", "model-descriptions", "reasoning-descriptions", "--details", "doctor)"} {
		if !strings.Contains(ZshCompletion, fragment) {
			t.Fatal("missing completion integration", fragment)
		}
	}
}
