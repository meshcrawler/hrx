package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/meshcrawler/hrx/internal/catalog"
)

func TestGeneratedCodexCatalogUsesOnlyAuthorizedRoutesAndGatewayEfforts(t *testing.T) {
	s := testSettings(t)
	s.TemplatePath = filepath.Join(t.TempDir(), "templates.json")
	data := []byte(`{"unrelated_secret":"not-copied","models":[{"slug":"old-template","tool_mode":"code_mode_only","multi_agent_version":"v2","use_responses_lite":true,"support_verbosity":true,"model_messages":{"instructions_template":"fixture instructions"},"context_window":12345,"supported_reasoning_levels":[{"effort":"ultra"}]},{"slug":"unauthorized-model"}]}`)
	if err := os.WriteFile(s.TemplatePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	snap := snapshot()
	cap := snap.Models[0].Harnesses["codex"]
	cap.CatalogTemplate = "old-template"
	snap.Models[0].Harnesses["codex"] = cap
	path, skipped, err := GenerateCodexCatalog(s, snap)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture model has no known context capacity: it must be skipped
	// with a report, never emitted with a fabricated 32K window.
	if len(skipped) != 1 || skipped[0] != "provider/example" {
		t.Fatalf("unknown-capacity route not reported as skipped: %v", skipped)
	}
	generated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(generated), "ultra") || strings.Contains(string(generated), "unauthorized-model") || strings.Contains(string(generated), "not-copied") {
		t.Fatal("template overrode shared catalog or leaked root metadata")
	}
	if strings.Contains(string(generated), "tool_mode") || strings.Contains(string(generated), "multi_agent_version") {
		t.Fatal("borrowed template inherited model-specific tool orchestration")
	}
	var parsed struct {
		Models []struct {
			Slug    string
			Context int    `json:"context_window"`
			Default string `json:"default_reasoning_level"`
		}
	}
	if err = json.Unmarshal(generated, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Models) != 0 {
		t.Fatalf("unknown-capacity route was emitted with a fabricated window: %+v", parsed)
	}
	original, _ := os.ReadFile(s.TemplatePath)
	if string(original) != string(data) {
		t.Fatal("user template modified")
	}
	second, _, err := GenerateCodexCatalog(s, snap)
	if err != nil || second != path {
		t.Fatal("non-deterministic generated path")
	}
	snap.Models[0].Context = 128000
	boundedPath, _, err := GenerateCodexCatalog(s, snap)
	if err != nil {
		t.Fatal(err)
	}
	bounded, _ := os.ReadFile(boundedPath)
	var safe struct {
		Models []struct {
			Context    int      `json:"context_window"`
			MaxContext int      `json:"max_context_window"`
			Lite       bool     `json:"use_responses_lite"`
			Verbosity  bool     `json:"support_verbosity"`
			Modalities []string `json:"input_modalities"`
		}
	}
	if err := json.Unmarshal(bounded, &safe); err != nil {
		t.Fatal(err)
	}
	if len(safe.Models) != 1 || safe.Models[0].Context != 128000 || safe.Models[0].MaxContext != 128000 || safe.Models[0].Lite || safe.Models[0].Verbosity || strings.Join(safe.Models[0].Modalities, ",") != "text" {
		t.Fatalf("borrowed template capabilities not bounded: %+v", safe)
	}
	if err := json.Unmarshal(bounded, &parsed); err != nil || parsed.Models[0].Default != "low" || parsed.Models[0].Slug != "provider/example" {
		t.Fatalf("gateway route/effort not retained: %+v (%v)", parsed, err)
	}
	if strings.Contains(string(bounded), "tool_mode") || strings.Contains(string(bounded), "multi_agent_version") {
		t.Fatal("borrowed template inherited model-specific orchestration")
	}
	s.GeneratedCatalogPath = boundedPath
	l, err := BuildLaunch(s, snap, LaunchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(l.Args, " "), "model_catalog_json=") {
		t.Fatal("catalog not pinned at launch")
	}
	cap.CatalogTemplate = "missing"
	snap.Models[0].Harnesses["codex"] = cap
	if _, _, err := GenerateCodexCatalog(s, snap); err == nil {
		t.Fatal("missing template guessed")
	}
}

func TestCodexUnknownInputCapacityCannotLaunch(t *testing.T) {
	for _, options := range []LaunchOptions{{}, {DryRun: true}, {Resume: "existing-session"}} {
		s := testSettings(t)
		snap := snapshot()
		cap := snap.Models[0].Harnesses["codex"]
		cap.CatalogTemplate = "borrowed"
		snap.Models[0].Harnesses["codex"] = cap
		// A total capacity is not a declared input allowance.
		snap.Models[0].TotalContext = 1050000
		if _, err := BuildLaunch(s, snap, options); err == nil || !strings.Contains(err.Error(), "input capacity is unknown") {
			t.Fatalf("unknown selected capacity did not fail clearly: %v", err)
		}
	}
}

func TestCodexKnownRouteSurvivesUnrelatedUnknownRoute(t *testing.T) {
	s := testSettings(t)
	s.TemplatePath = filepath.Join(t.TempDir(), "templates.json")
	if err := os.WriteFile(s.TemplatePath, []byte(`{"models":[{"slug":"borrowed","context_window":32768}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	snap := snapshot()
	cap := snap.Models[0].Harnesses["codex"]
	cap.CatalogTemplate = "borrowed"
	snap.Models[0].Harnesses["codex"] = cap
	snap.Models[0].Context = 922000
	snap.Models[0].TotalContext = 1050000
	snap.Models[0].Output = 128000
	unknownCap := cap
	unknownCap.CatalogTemplate = "absent-template"
	snap.Models = append(snap.Models, catalog.Model{ID: "unknown", Route: "provider/unknown", Harnesses: map[string]catalog.Capability{"codex": unknownCap}})
	path, skipped, err := GenerateCodexCatalog(s, snap)
	if err != nil || len(skipped) != 1 || skipped[0] != "provider/unknown" {
		t.Fatalf("unrelated unknown route blocked catalog: %v %v", skipped, err)
	}
	s.GeneratedCatalogPath = path
	if _, err := BuildLaunch(s, snap, LaunchOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Models []struct {
			Context int `json:"context_window"`
		}
	}
	if err := json.Unmarshal(data, &result); err != nil || len(result.Models) != 1 || result.Models[0].Context != 922000 {
		t.Fatalf("input allowance replaced by fallback or total: %s (%v)", data, err)
	}
}
