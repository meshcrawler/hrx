package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/catalog"
	"github.com/meshcrawler/hrx/internal/session"
)

func profileConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("HRX_CONFIG", filepath.Join(dir, "config.toml"))
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.json"), []byte(`{"key":"profile-test-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

const validProfile = `[profiles.test]
harness = "claude"
model = "payer/example"
reasoning = "low"
credential_file = "key.json"
`

func TestProfilesSchemaPrecedenceAndPassthrough(t *testing.T) {
	dir := profileConfig(t, validProfile)
	o, err := parseLaunchFor("claude", []string{"--profile", "test", "--model", "other/exact", "--reasoning", "high", "--credential-file", "~/rotated.json", "--", "-p", "hello"})
	if err != nil {
		t.Fatal(err)
	}
	o, err = resolveProfileOptions("claude", o)
	if err != nil || o.Model != "other/exact" || o.Reasoning != "high" || o.CredentialFile != "~/rotated.json" || len(o.Extra) != 2 {
		t.Fatalf("%+v %v", o, err)
	}
	if o.provenance["model"] != "cli" {
		t.Fatal("override provenance missing")
	}
	p, err := profileDefinition("test")
	if err != nil || p.CredentialFile != filepath.Join(dir, "key.json") {
		t.Fatal(p, err)
	}
	if _, err = resolveProfileOptions("codex", LaunchOptions{ProfileName: "test"}); err == nil {
		t.Fatal("conflicting harness accepted")
	}
	a, err := expandProfileCommand([]string{"--profile=test", "--dry-run"})
	if err != nil || a[0] != "claude" {
		t.Fatal(a, err)
	}
	for _, change := range []string{"harness = 3", `harness = "unknown"`, `model = "bad model"`, `credential_file = ""`, `unknown = "do-not-print-this-value"`} {
		text := validProfile
		key := strings.Split(change, " =")[0]
		lines := strings.Split(text, "\n")
		found := false
		for i, line := range lines {
			if strings.HasPrefix(line, key+" =") {
				lines[i] = change
				found = true
			}
		}
		if !found {
			lines = append(lines, change)
		}
		os.WriteFile(filepath.Join(dir, "config.toml"), []byte(strings.Join(lines, "\n")), 0600)
		_, err := profileDefinition("test")
		if err == nil || strings.Contains(err.Error(), "do-not-print-this-value") {
			t.Fatalf("schema accepted or leaked: %s %v", change, err)
		}
	}
}

func TestProfilesCredentialFilesAndOfflineIndependence(t *testing.T) {
	dir := profileConfig(t, validProfile+"\n[profiles.bad]\nharness=3\n")
	key := filepath.Join(dir, "key.json")
	for _, tc := range []struct {
		name, content string
		mode          os.FileMode
		good          bool
	}{
		{"valid", `{"key":"profile-test-key"}`, 0600, true},
		{"public", `{"key":"profile-test-key"}`, 0644, false},
		{"empty", `{"key":""}`, 0600, false},
		{"bad-json", `profile-test-key`, 0600, false},
		{"whitespace", `{"key":"profile test key"}`, 0600, false},
	} {
		os.WriteFile(key, []byte(tc.content), 0600)
		os.Chmod(key, tc.mode)
		_, err := readProfileCredential(key)
		if (err == nil) != tc.good {
			t.Fatal(tc.name, err)
		}
		if err != nil && strings.Contains(err.Error(), "profile-test-key") {
			t.Fatal("secret leaked")
		}
	}
	os.WriteFile(key, []byte(`{"key":"profile-test-key"}`), 0600)
	link := filepath.Join(dir, "link")
	os.Symlink(key, link)
	if _, err := readProfileCredential(link); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := readProfileCredential(dir); err == nil {
		t.Fatal("directory accepted")
	}
	var out bytes.Buffer
	err := Run(context.Background(), []string{"profiles", "validate", "--all", "--offline", "--json"}, &out, &out)
	var results []profileValidation
	if err == nil || json.Unmarshal(out.Bytes(), &results) != nil || len(results) != 2 || results[0].Status != "fail" || results[1].Status != "pass" {
		t.Fatalf("%s %v", out.String(), err)
	}
	if strings.Contains(out.String(), "profile-test-key") {
		t.Fatal("secret in validation")
	}
	os.Remove(key)
	out.Reset()
	if err := Run(context.Background(), []string{"profiles", "show", "test", "--json"}, &out, &out); err != nil {
		t.Fatal("show reads credential", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
		t.Fatal("offline validation mutated session state")
	}
}

func TestProfilesResumeUsesSnapshotNotEditedConfiguration(t *testing.T) {
	dir := profileConfig(t, validProfile)
	p, err := profileDefinition("test")
	if err != nil {
		t.Fatal(err)
	}
	store := session.Store{Dir: filepath.Join(dir, "sessions")}
	r := session.Record{NativeID: "native", Harness: "claude", Project: "/project", Identity: "example", Route: p.Model, Reasoning: "low", Profile: p}
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("# profile was deleted\n"), 0600)
	o, _, err := restoreSession(store, "claude", "/project", LaunchOptions{Resume: "native"})
	if err != nil || o.profile == nil || o.Model != p.Model || o.CredentialFile != p.CredentialFile {
		t.Fatalf("%+v %v", o, err)
	}
	os.WriteFile(p.CredentialFile, []byte(`{"key":"rotated-key"}`), 0600)
	key, err := credential(context.Background(), Provider{ProfileCredential: o.CredentialFile})
	if err != nil || key != "rotated-key" {
		t.Fatal("rotation not read", err)
	}
	o, _, err = restoreSession(store, "claude", "/project", LaunchOptions{Resume: "native", Model: "other/model", CredentialFile: "/explicit/key"})
	if err != nil || o.Model != "other/model" || o.CredentialFile != "/explicit/key" || o.provenance["model"] != "cli" {
		t.Fatal(o, err)
	}
}

func TestProfilesOnlineValidationAndDryRunIsolation(t *testing.T) {
	for _, harness := range []string{"codex", "claude", "opencode"} {
		t.Run(harness, func(t *testing.T) {
			wire := map[string]string{"codex": "responses", "claude": "messages", "opencode": "chat_completions"}[harness]
			mode := "good"
			auths := []string{}
			inference := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/dashboard/models.json":
					if r.Header.Get("Authorization") != "" {
						t.Error("credential sent to public catalog")
					}
					generated := time.Now()
					if mode == "stale" {
						generated = generated.Add(-time.Hour)
					}
					cap := map[string]any{"wire_api": wire, "reasoning_levels": []string{"low", "high"}, "default_reasoning": "low"}
					caps := map[string]any{harness: cap}
					if mode == "capability" {
						caps = map[string]any{}
					}
					input := 100000
					if mode == "limits" {
						input = 0
					}
					json.NewEncoder(w).Encode(map[string]any{"schema_version": 1, "generated_at": generated, "coverage": map[string]any{"providers": []any{map[string]any{"source": "gateway", "ok": true}}}, "models": []any{map[string]any{"canonical_id": "example", "available": true, "sources": []any{map[string]any{"gateway_route": "payer/example", "available": true, "details": map[string]any{"harnesses": caps, "max_input_tokens": input, "max_output_tokens": 10000}}}}}})
				case "/v1/models":
					auths = append(auths, r.Header.Get("Authorization"))
					if mode == "revoked" {
						w.WriteHeader(401)
						return
					}
					id := "payer/example"
					if mode == "unauthorized" {
						id = "other/example"
					}
					json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": id}}})
				default:
					inference++
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			original := http.DefaultTransport
			http.DefaultTransport = server.Client().Transport
			defer func() { http.DefaultTransport = original }()
			body := strings.Replace(validProfile, `harness = "claude"`, `harness = "`+harness+`"`, 1)
			body += fmt.Sprintf("\n[%s.provider]\nbase_url=%q\nenv_key=\"IGNORED_AMBIENT_KEY\"\n", harness, server.URL+"/v1")
			dir := profileConfig(t, body)
			t.Setenv("CODEX_HOME", dir)
			t.Setenv("HRX_CODEX_PROVIDER", "")
			t.Setenv("IGNORED_AMBIENT_KEY", "wrong-key")
			if harness == "codex" {
				// Codex provider configuration is separate from HRX profiles.
				codex := filepath.Join(dir, "codex")
				os.Mkdir(codex, 0700)
				t.Setenv("CODEX_HOME", codex)
				os.WriteFile(filepath.Join(codex, "config.toml"), []byte(fmt.Sprintf("model_provider=\"fixture\"\n[model_providers.fixture]\nwire_api=\"responses\"\nbase_url=%q\nenv_key=\"IGNORED_AMBIENT_KEY\"\n", server.URL+"/v1")), 0600)
				os.WriteFile(filepath.Join(codex, "models_cache.json"), []byte(`{"models":[{"slug":"example"}]}`), 0600)
			}
			for _, next := range []string{"good", "stale", "unauthorized", "revoked", "capability", "limits"} {
				mode = next
				var out bytes.Buffer
				err := Run(context.Background(), []string{"profiles", "validate", "test", "--json"}, &out, &out)
				if (err == nil) != (next == "good") {
					t.Fatalf("%s: %v %s", next, err, out.String())
				}
				if strings.Contains(out.String(), "profile-test-key") {
					t.Fatal("secret leaked")
				}
			}
			mode = "good"
			var out, diagnostics bytes.Buffer
			if err := Run(context.Background(), []string{"--profile", "test", "--dry-run"}, &out, &diagnostics); err != nil {
				t.Fatal(err)
			}
			var launch Launch
			if err := json.Unmarshal(out.Bytes(), &launch); err != nil {
				t.Fatal(err, out.String())
			}
			if launch.Profile == nil || launch.Model != "payer/example" || launch.Provenance["model"] != "profile" {
				t.Fatalf("%+v", launch)
			}
			if strings.Contains(out.String()+diagnostics.String(), "profile-test-key") || inference != 0 {
				t.Fatal("secret leaked or inference performed")
			}
			for _, a := range auths {
				if a != "Bearer profile-test-key" {
					t.Fatal("wrong key scope", a)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
				t.Fatal("validation/dry-run created session state")
			}
		})
	}
}

func TestProfilesExactRouteNeverFallsBackToCanonicalName(t *testing.T) {
	s := testSettings(t)
	s.Harness = "claude"
	snap := otherSnapshot("claude")
	err := validateProfileRoute(s, snap, LaunchOptions{Model: "example", CredentialFile: "/private/key"})
	if err == nil || !strings.Contains(err.Error(), "provider/example") {
		t.Fatal("bare identity accepted or no candidates", err)
	}
	snap.Models = append(snap.Models, catalog.Model{ID: "example", Route: "second/example"})
	if err := validateProfileRoute(s, snap, LaunchOptions{Model: "example", CredentialFile: "/private/key"}); err == nil {
		t.Fatal("ambiguous payer selected")
	}
}

func TestProfilesNamesCannotCollideWithHarnessConfigFields(t *testing.T) {
	for _, name := range []string{"model", "provider", "test"} {
		profileConfig(t, strings.Replace(validProfile, "profiles.test", "profiles."+name, 1)+"\n[claude.provider]\nbase_url=\"https://gateway.example/v1\"\n")
		p, err := profileDefinition(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := loadHarnessSettings("claude", p.CredentialFile); err != nil {
			t.Fatal(name, err)
		}
	}
}

func TestProfilesKeyScopesAndRotationNeverReuseAuthorization(t *testing.T) {
	requests := []string{}
	publicRequests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			key := r.Header.Get("Authorization")
			requests = append(requests, key)
			route := "payer/a"
			if key == "Bearer key-b" {
				route = "payer/b"
			}
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": route}}})
			return
		}
		if r.URL.Path != "/dashboard/models.json" {
			t.Error("unexpected endpoint")
			w.WriteHeader(500)
			return
		}
		publicRequests++
		models := []any{}
		for _, name := range []string{"a", "b"} {
			models = append(models, map[string]any{"canonical_id": name, "available": true, "sources": []any{map[string]any{"gateway_route": "payer/" + name, "available": true, "details": map[string]any{"harnesses": map[string]any{"claude": map[string]any{"wire_api": "messages"}}, "max_input_tokens": 100000, "max_output_tokens": 10000}}}})
		}
		json.NewEncoder(w).Encode(map[string]any{"schema_version": 1, "generated_at": time.Now(), "coverage": map[string]any{"providers": []any{map[string]any{"source": "gateway", "ok": true}}}, "models": models})
	}))
	defer server.Close()
	old := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	defer func() { http.DefaultTransport = old }()
	dir := profileConfig(t, fmt.Sprintf(`[claude.provider]
base_url=%q
[profiles.a]
harness="claude"
model="payer/a"
credential_file="a.json"
[profiles.b]
harness="claude"
model="payer/b"
credential_file="b.json"
`, server.URL+"/v1"))
	os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{"key":"key-a"}`), 0600)
	os.WriteFile(filepath.Join(dir, "b.json"), []byte(`{"key":"key-b"}`), 0600)
	var out bytes.Buffer
	if err := runProfiles(context.Background(), []string{"validate", "--all", "--json"}, &out); err != nil {
		t.Fatal(err, out.String())
	}
	if len(requests) != 2 || requests[0] == requests[1] {
		t.Fatal("key scopes mixed")
	}
	if publicRequests != 1 {
		t.Fatal("public catalog was not shared within the validation batch")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "cache", "hrx", "*.json"))
	if len(files) != 2 {
		t.Fatal("profile cache bindings collide", files)
	}
	os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{"key":"key-b"}`), 0600)
	out.Reset()
	if err := runProfiles(context.Background(), []string{"validate", "a", "--json"}, &out); err == nil || !strings.Contains(out.String(), "not-authorized") {
		t.Fatal("rotated key reused old authorization", err, out.String())
	}
	if len(requests) != 3 {
		t.Fatal("did not fetch fresh authorization")
	}
}
