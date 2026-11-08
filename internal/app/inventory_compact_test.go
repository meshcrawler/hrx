package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/meshcrawler/hrx/internal/catalog"
)

// compactTestLabels mirrors a typical [catalog.labels] table.
var compactTestLabels = map[string]string{
	"openrouter/acme":             "OR-A",
	"openrouter/acme-dev":         "OR-AD",
	"truefoundry/contoso":         "TF-C",
	"fireworks/acme":              "FW-A",
	"fireworks/fireworks-contoso": "FW-C",
}

func useCompactTestLabels(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[catalog.labels]\n"
	for key, label := range compactTestLabels {
		body += fmt.Sprintf("%q = %q\n", key, label)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRX_CONFIG", path)
}

func TestCompactProviderLabels(t *testing.T) {
	for _, tc := range []struct{ provider, account, want string }{
		{"openrouter", "acme", "OR-A"},
		{"openrouter", "acme-dev", "OR-AD"},
		{"truefoundry", "contoso", "TF-C"},
		{"fireworks", "acme", "FW-A"},
		{"fireworks", "fireworks-contoso", "FW-C"},
		{"openai", "", "OAI"},
		{"anthropic", "", "ANT"},
		{"chatgpt", "", "CGPT"},
		{"openai", "another-payer", "openai/another-payer"},
		{"openrouter", "my-other-account", "openrouter/my-other-account"},
		{"unknown-provider", "", "unknown-provider/"},
		{"", "", "?"},
	} {
		if got := catalogProviderLabel(tc.provider, tc.account, compactTestLabels); got != tc.want {
			t.Errorf("%s/%s: got %s, want %s", tc.provider, tc.account, got, tc.want)
		}
	}
	// Without configuration, accounts keep their full names.
	if got := catalogProviderLabel("openrouter", "acme", nil); got != "openrouter/acme" {
		t.Errorf("unconfigured label: got %s", got)
	}
	// A blank configured label falls back instead of rendering an empty column.
	if got := catalogProviderLabel("openrouter", "acme", map[string]string{"openrouter/acme": " "}); got != "openrouter/acme" {
		t.Errorf("blank label: got %s", got)
	}
}

func TestCompactAggregatesProvidersButNotAuthorization(t *testing.T) {
	useCompactTestLabels(t)
	rows := []CatalogRow{
		{ID: "sol", Provider: "truefoundry", Account: "contoso", Route: "tfy/sol", Harnesses: map[string]CatalogAccess{"codex": {Status: "ready"}}},
		{ID: "sol", Provider: "truefoundry", Account: "contoso", Upstream: "alternate"},
		{ID: "sol", Provider: "openrouter", Account: "acme", Route: "or/sol", Harnesses: map[string]CatalogAccess{"codex": {Status: "not-authorized"}, "opencode": {Status: "ready"}}},
		{ID: "sol", Provider: "openrouter", Account: "acme", Route: "or/sol:batch", Harnesses: map[string]CatalogAccess{"opencode": {Status: "ready"}}},
		{ID: "sol", Provider: "openrouter", Account: "acme-dev", Route: "or-df/sol", Harnesses: map[string]CatalogAccess{"codex": {Status: "unavailable"}}},
		{ID: "discovered", Provider: "openai"},
	}
	var out bytes.Buffer
	if err := renderCompactCatalog(&out, rows, []string{"codex", "claude", "opencode"}); err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, line := range strings.Split(out.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && (fields[0] == "sol" || fields[0] == "discovered") {
			if _, exists := got[fields[0]]; exists {
				t.Fatal("duplicate model row", line)
			}
			got[fields[0]] = fields[1:]
		}
	}
	want := map[string][]string{"sol": {"OR-A,OR-AD,TF-C", "TF-C[D]", "-", "OR-A[D]", "-"}, "discovered": {"OAI", "-", "-", "-", "-"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v\n%s", got, want, out.String())
	}
	if !strings.Contains(out.String(), "TF-C = truefoundry/contoso") {
		t.Fatal("missing legend")
	}
	var again bytes.Buffer
	if err := renderCompactCatalog(&again, rows, []string{"codex", "claude", "opencode"}); err != nil {
		t.Fatal(err)
	}
	if again.String() != out.String() {
		t.Fatal("nondeterministic output")
	}
}

func TestCompactCatalogCLIAndFullJSON(t *testing.T) {
	s := configure(t)
	snap := snapshot()
	snap.Binding = s.Binding
	snap.Inventory = []catalog.Entry{
		{ID: "gpt-5.2-codex", Route: "old", Provider: "truefoundry", Account: "contoso", Available: true, Authorized: true, Harnesses: snap.Models[0].Harnesses},
		{ID: "gpt-5.3-codex", Route: "new", Provider: "truefoundry", Account: "contoso", Available: true, Authorized: true, Harnesses: snap.Models[0].Harnesses},
		{ID: "gpt-5.3-codex", Provider: "openrouter", Account: "acme", Available: true},
		{ID: "unrelated", Provider: "anthropic", Available: true},
	}
	if err := catalog.Write(s.CachePath, snap); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	args := []string{"catalog", "--compact", "--harness", "codex", "--latest", "--search", "gpt-"}
	if err := Run(context.Background(), args, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "gpt-5.3-codex") != 1 || strings.Contains(out.String(), "gpt-5.2-codex") || strings.Contains(out.String(), "unrelated") {
		t.Fatal("compact/latest/search did not compose", out.String())
	}
	if strings.Contains(out.String(), "claude:") || strings.Contains(out.String(), "opencode:") {
		t.Fatal("harness filtering lost")
	}
	out.Reset()
	if err := Run(context.Background(), append(args, "--json"), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	var report CatalogReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Models) != 2 || report.SourceCount != 2 || report.IdentityCount != 1 {
		t.Fatal("JSON lost detail", report)
	}
}
