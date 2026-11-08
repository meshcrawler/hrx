package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/meshcrawler/hrx/internal/catalog"
)

func testSettings(t testing.TB) Settings {
	t.Helper()
	return Settings{ProviderName: "test", DefaultModel: "example", Binding: "binding", CachePath: filepath.Join(t.TempDir(), "catalog.json"), Provider: Provider{BaseURL: "https://gateway.example/v1", WireAPI: "responses", EnvKey: "HRX_TEST_KEY"}}
}

func snapshot() catalog.Snapshot {
	return catalog.Snapshot{Version: 1, Binding: "binding", RefreshedAt: time.Now(), GeneratedAt: time.Now(), Models: []catalog.Model{{ID: "example", Route: "provider/example", Harnesses: map[string]catalog.Capability{"codex": {WireAPI: "responses", Reasoning: []string{"low", "high"}, DefaultReasoning: "low"}}}}}
}

func TestLaunchResolutionEffortAndPassthrough(t *testing.T) {
	s := testSettings(t)
	o, err := ParseLaunch([]string{"--model=example", "--reasoning", "high", "--", "exec", "--ephemeral", "a prompt with spaces"})
	if err != nil {
		t.Fatal(err)
	}
	l, err := BuildLaunch(s, snapshot(), o)
	if err != nil {
		t.Fatal(err)
	}
	if l.Model != "provider/example" || l.Reasoning != "high" || l.Args[len(l.Args)-1] != "a prompt with spaces" {
		t.Fatalf("%+v", l)
	}
	for _, arg := range []string{l.Args[1], l.Args[3], l.Args[7]} {
		var decoded map[string]any
		if err := toml.Unmarshal([]byte(arg), &decoded); err != nil {
			t.Fatalf("invalid TOML override %q: %v", arg, err)
		}
	}
	o.Reasoning = "ultra"
	if _, err := BuildLaunch(s, snapshot(), o); err == nil {
		t.Fatal("unsupported effort accepted")
	}
	o.Reasoning = ""
	l, err = BuildLaunch(s, snapshot(), o)
	if err != nil || l.Reasoning != "low" {
		t.Fatal("default effort not applied")
	}
}

func TestPassthroughCannotOverrideValidatedConfig(t *testing.T) {
	for _, args := range [][]string{{"--model"}, {"--model=x", "--model=y"}, {"--reasoning="}, {"--", "-mother"}, {"--", "--model=other"}, {"--", "--config=x"}, {"--", "exec", "-c", "model='other'"}, {"--", "--profile", "other"}, {"--", "--oss"}} {
		if _, err := ParseLaunch(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
}

func TestAuthOverrideTOMLAndDryRunRedaction(t *testing.T) {
	s := testSettings(t)
	s.Provider.EnvKey = ""
	s.Provider.Auth = Auth{Command: "ssh", Args: []string{"host", "python3 -c 'print(\"private-handle\")'"}, TimeoutMS: 10000}
	l, err := BuildLaunch(s, snapshot(), LaunchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err = toml.Unmarshal([]byte(l.Args[3]), &parsed); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(l)
	if strings.Contains(string(data), "private-handle") || strings.Contains(string(data), "ssh") {
		t.Fatal("auth helper leaked to diagnostic output")
	}
}

func TestRefreshUsesAuthOnlyOnModelsAndCachesSanitizedData(t *testing.T) {
	t.Setenv("HRX_TEST_KEY", "test-secret-never-cache")
	s := testSettings(t)
	requests := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests[r.URL.Path]++
		if r.URL.Path == "/dashboard/models.json" {
			if r.Header.Get("Authorization") != "" {
				t.Error("credential sent to dashboard")
			}
			payload := map[string]any{"schema_version": 1, "generated_at": time.Now(), "coverage": map[string]any{"providers": []any{map[string]any{"source": "gateway", "ok": true}}}, "models": []any{map[string]any{"canonical_id": "example", "available": true, "sources": []any{map[string]any{"gateway_route": "provider/example", "available": true, "details": map[string]any{"harnesses": snapshot().Models[0].Harnesses, "secret": "server-secret"}}}}}}
			json.NewEncoder(w).Encode(payload)
		} else if r.URL.Path == "/v1/models" {
			if r.Header.Get("Authorization") != "Bearer test-secret-never-cache" {
				t.Error("missing authorization")
			}
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "provider/example"}, map[string]any{"id": "other"}}, "secret": "another-server-secret"})
		} else {
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	s.Provider.BaseURL = server.URL + "/v1"
	s.CatalogURL = server.URL + "/dashboard/models.json"
	got, err := Refresh(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 1 || requests["/v1/models"] != 1 || requests["/dashboard/models.json"] != 1 {
		t.Fatalf("%+v %v", got, requests)
	}
	data, err := os.ReadFile(s.CachePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") {
		t.Fatal("secret cached")
	}
}

func TestRefreshFailureDoesNotReplaceCache(t *testing.T) {
	t.Setenv("HRX_TEST_KEY", "not-a-real-key")
	s := testSettings(t)
	if err := catalog.Write(s.CachePath, snapshot()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.CachePath)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "do-not-print-secret", 401) }))
	defer server.Close()
	s.CatalogURL = server.URL
	_, err := Refresh(context.Background(), s)
	if err == nil || strings.Contains(err.Error(), "do-not-print") {
		t.Fatal("unsafe error", err)
	}
	after, _ := os.ReadFile(s.CachePath)
	if !bytes.Equal(before, after) {
		t.Fatal("last good cache replaced on failure")
	}
}

func TestCredentialHelperOutputSuppressed(t *testing.T) {
	p := Provider{Auth: Auth{Command: "sh", Args: []string{"-c", "printf secret-on-stdout; printf secret-on-stderr >&2; exit 1"}}}
	_, err := credential(context.Background(), p)
	if err == nil || strings.Contains(err.Error(), "secret-on") {
		t.Fatal("helper output leaked", err)
	}
	p.Auth.Args = []string{"-c", "printf '%s\n' test-key"}
	got, err := credential(context.Background(), p)
	if err != nil || got != "test-key" {
		t.Fatalf("helper failed %v", err)
	}
}

func configure(t *testing.T) Settings {
	t.Helper()
	directory := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HRX_CONFIG", "")
	t.Setenv("CODEX_HOME", directory)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	config := `model = "example"
model_provider = "gateway"
[model_providers.gateway]
base_url = "https://gateway.example/v1"
wire_api = "responses"
env_key = "HRX_TEST_KEY"
`
	if err := os.WriteFile(filepath.Join(directory, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCompletionIsOfflineAndStaleIsQuiet(t *testing.T) {
	s := configure(t)
	t.Setenv("HRX_TEST_KEY", "")
	c := snapshot()
	c.Binding = s.Binding
	if err := catalog.Write(s.CachePath, c); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args []string
		want string
	}{{[]string{"_complete", "models"}, "example\n"}, {[]string{"_complete", "reasoning", "example"}, "low\nhigh\n"}, {[]string{"_complete", "reasoning", "missing"}, ""}} {
		var out, stderr bytes.Buffer
		if err := Run(context.Background(), test.args, &out, &stderr); err != nil {
			t.Fatal(err)
		}
		if out.String() != test.want || stderr.Len() != 0 {
			t.Fatalf("unexpected completion %q %q", out.String(), stderr.String())
		}
	}
	c.RefreshedAt = time.Now().Add(-time.Hour)
	catalog.Write(s.CachePath, c)
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"_complete", "models"}, &out, &out); err != nil || out.Len() != 0 {
		t.Fatal("stale completion emitted candidates")
	}
}

func TestConfigRejectsInsecureOrAmbiguousProviders(t *testing.T) {
	s := configure(t)
	if s.CatalogURL != "https://gateway.example/dashboard/models.json" {
		t.Fatal(s.CatalogURL)
	}
	data, _ := os.ReadFile(s.ConfigPath)
	for _, bad := range []string{strings.ReplaceAll(string(data), "https://", "http://"), strings.ReplaceAll(string(data), "https://gateway", "https://user:secret@gateway"), string(data) + "[model_providers.gateway.auth]\ncommand=\"ssh\"\n"} {
		os.WriteFile(s.ConfigPath, []byte(bad), 0600)
		if _, err := LoadSettings(); err == nil {
			t.Fatal("unsafe config accepted")
		}
	}
}
