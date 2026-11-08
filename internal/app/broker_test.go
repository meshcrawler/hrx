package app

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBrokerStandaloneIntegration(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("broker tests require Python 3.11+", err)
	}
	var accounts, tokens atomic.Int32
	gitlab := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "fixture-admin" {
			w.WriteHeader(403)
			return
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(400)
			return
		}
		switch r.URL.Path {
		case "/api/v4/groups/7/service_accounts":
			accounts.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"id": 42, "username": body["username"]})
		case "/api/v4/groups/7/service_accounts/42/personal_access_tokens":
			tokens.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"user_id": 42, "token": "standalone-token", "expires_at": body["expires_at"]})
		default:
			w.WriteHeader(404)
		}
	}))
	defer gitlab.Close()
	dir := t.TempDir()
	clientSecret := strings.Repeat("client", 8)
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	ca := write("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: gitlab.Certificate().Raw}))
	admin := write("admin", []byte("fixture-admin"))
	client := write("client", []byte(clientSecret))
	config, _ := json.Marshal(map[string]any{"gitlab_url": gitlab.URL, "gitlab_token_file": admin, "state_dir": filepath.Join(dir, "state"), "group_id": 7, "clients": map[string]string{"host/hrx/agent": fmt.Sprintf("%x", sha256.Sum256([]byte(clientSecret)))}, "enrollment_clients": map[string]any{"fixture": map[string]string{"sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(clientSecret))), "prefix": "host/", "profile": "developer"}}})
	configPath := write("config.json", config)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Use an explicit test-only CA context: platform Python builds do not all
	// honor SSL_CERT_FILE. Keep certificate and hostname verification enabled.
	// The production broker, redirect policy and provisioning requests are unchanged.
	script := `import sys, json, os, ssl
sys.path.insert(0, sys.argv[1])
import broker
from urllib.request import HTTPSHandler
from broker import Broker, Handler, ThreadingHTTPServer
context = ssl.create_default_context(cafile=sys.argv[3])
assert context.check_hostname and context.verify_mode == ssl.CERT_REQUIRED
build_opener = broker.build_opener
broker.build_opener = lambda *handlers: build_opener(*handlers, HTTPSHandler(context=context))
os.umask(0o077)
server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
with open(sys.argv[2]) as f: server.broker = Broker(json.load(f))
print(server.server_port, flush=True)
server.serve_forever()
`
	module, err := filepath.Abs("../../services/gitlab-service-account-broker")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, python, "-c", script, module, configPath, ca)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatal("standalone broker did not start")
	}
	t.Setenv("HRX_GITLAB_BROKER_URL", "http://127.0.0.1:"+scanner.Text())
	t.Setenv("HRX_GITLAB_BROKER_TOKEN_FILE", client)
	o := LaunchOptions{Plugin: "gitlab-service-account-broker", AgentIdentity: "host/hrx/agent"}
	for i := 0; i < 2; i++ {
		l := Launch{}
		if _, err := prepareBroker(ctx, o, &l); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(l.Env, "\n"), "GITLAB_TOKEN=standalone-token") {
			t.Fatal("missing service credential")
		}
	}
	if accounts.Load() != 1 || tokens.Load() != 1 {
		t.Fatal("broker did not reuse identity")
	}
	o.AgentIdentity = "other/hrx/agent"
	if _, err := prepareBroker(ctx, o, &Launch{}); err == nil {
		t.Fatal("broker allowed impersonation")
	}
	// Exercise automatic enrollment against the real standalone broker.
	t.Setenv("HRX_GITLAB_BROKER_TOKEN_FILE", "")
	userConfig := write("hrx.toml", []byte(fmt.Sprintf("[plugins.gitlab-service-account-broker]\nenrollment_token_file = %q\n", client)))
	t.Setenv("HRX_CONFIG", userConfig)
	o.AgentIdentity = "host/new/agent"
	for i := 0; i < 2; i++ {
		l := Launch{}
		if _, err := prepareBroker(ctx, o, &l); err != nil {
			t.Fatal("automatic enrollment failed", err)
		}
		if l.Broker.Credential.Profile != "developer" || strings.Contains(strings.Join(l.Env, "\n"), clientSecret) {
			t.Fatal("incorrect policy or enrollment credential leaked")
		}
	}
	if accounts.Load() != 2 || tokens.Load() != 2 {
		t.Fatal("enrollment or account was not reused")
	}
	if _, err := readBrokerSecret(filepath.Join(dir, "client-tokens", "host", "new", "agent", "broker.token")); err != nil {
		t.Fatal(err)
	}
}

func brokerFixture(t *testing.T) (*httptest.Server, LaunchOptions) {
	t.Helper()
	secret := strings.Repeat("c", 40)
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRX_GITLAB_BROKER_TOKEN_FILE", path)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if r.Method != "POST" || r.URL.Path != "/v1/credentials" || r.Header.Get("Authorization") != "Bearer "+secret || json.NewDecoder(r.Body).Decode(&body) != nil || body["identity"] != "host/hrx/agent" {
			w.WriteHeader(403)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"identity": body["identity"], "username": "hrx_fixture", "user_id": 42, "token": "fixture-service-token", "host": "gitlab.example", "expires_at": time.Now().Add(72 * time.Hour).Format("2006-01-02")})
	}))
	t.Cleanup(s.Close)
	t.Setenv("HRX_GITLAB_BROKER_URL", s.URL)
	return s, LaunchOptions{Plugin: "gitlab-service-account-broker", AgentIdentity: "host/hrx/agent"}
}

func TestBrokerOptions(t *testing.T) {
	for _, harness := range []string{"codex", "claude", "opencode"} {
		_, err := parseLaunchFor(harness, []string{"--plugin=gitlab-service-account-broker", "--agent-identity", "host/hrx/agent"})
		if err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{
			{"--plugin=unknown", "--agent-identity=a"}, {"--plugin=gitlab-service-account-broker"}, {"--agent-identity=a"},
			{"--plugin=gitlab-service-account-broker", "--agent-identity=bad identity"},
			{"--", "--plugin=gitlab-service-account-broker"},
			{"--plugin=gitlab-service-account-broker", "--agent-identity=a", "--", "--fork"},
		} {
			if _, err := parseLaunchFor(harness, args); err == nil {
				t.Fatalf("accepted %v", args)
			}
		}
	}
}

func TestBrokerChildEnvironmentAndRedaction(t *testing.T) {
	_, o := brokerFixture(t)
	t.Setenv("GITLAB_TOKEN", "personal-token")
	t.Setenv("OAUTH_TOKEN", "personal-oauth")
	l := Launch{Executable: "claude"}
	prompt, err := prepareBroker(context.Background(), o, &l)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("GITLAB_TOKEN") != "personal-token" {
		t.Fatal("changed parent environment")
	}
	cmd := exec.Command("/bin/sh", "-c", `test "$GITLAB_TOKEN" = fixture-service-token && test "$GITLAB_HOST" = gitlab.example && test -z "$OAUTH_TOKEN" && test -z "$HRX_GITLAB_BROKER_TOKEN_FILE"`)
	cmd.Env = l.Env
	if err := cmd.Run(); err != nil {
		t.Fatal("child credential environment incorrect", err)
	}
	metadata, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{prompt, string(metadata), strings.Join(l.Args, " ")} {
		if strings.Contains(text, "fixture-service-token") || strings.Contains(text, "personal-token") {
			t.Fatal("credential leaked")
		}
	}
}

func TestBrokerDisabledAndDryRun(t *testing.T) {
	t.Setenv("HRX_GITLAB_BROKER_URL", "invalid")
	for _, o := range []LaunchOptions{{}, {Plugin: "gitlab-service-account-broker", AgentIdentity: "host/hrx/agent", DryRun: true}} {
		l := Launch{}
		prompt, err := prepareBroker(context.Background(), o, &l)
		if err != nil || prompt != "" || l.Env != nil {
			t.Fatal("disabled/dry-run had side effects", err)
		}
	}
}

func TestBrokerFailsClosed(t *testing.T) {
	_, o := brokerFixture(t)
	for _, body := range []string{"secret upstream error", `{"identity":"wrong","token":"secret"}`, strings.Repeat("x", 65537)} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		t.Setenv("HRX_GITLAB_BROKER_URL", s.URL)
		l := Launch{}
		_, err := prepareBroker(context.Background(), o, &l)
		s.Close()
		if err == nil || strings.Contains(err.Error(), "secret") || l.Env != nil {
			t.Fatal("invalid response not safely rejected")
		}
	}
	for _, address := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com/path", "https://example.com?secret"} {
		t.Setenv("HRX_GITLAB_BROKER_URL", address)
		if _, err := prepareBroker(context.Background(), o, &Launch{}); err == nil {
			t.Fatal("accepted", address)
		}
	}
}

func TestBrokerRedirectNotFollowed(t *testing.T) {
	_, o := brokerFixture(t)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed") }))
	defer target.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer s.Close()
	t.Setenv("HRX_GITLAB_BROKER_URL", s.URL)
	if _, err := prepareBroker(context.Background(), o, &Launch{}); err == nil {
		t.Fatal("accepted redirect")
	}
}

func TestBrokerCombinedPriming(t *testing.T) {
	_, o := brokerFixture(t)
	o.Pitput = true
	dir := t.TempDir()
	status, err := json.Marshal(map[string]any{"identity": o.AgentIdentity, "boards": []string{"test"}, "status": "ready"})
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif [ \"$1\" = agent ]; then printf '%s' " + shellWord(string(status)) + "; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "pitput"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, harness := range []string{"codex", "claude", "opencode"} {
		t.Run(harness, func(t *testing.T) {
			s := testSettings(t)
			s.GeneratedCatalogPath = filepath.Join(filepath.Dir(s.CachePath), "base.json")
			if err := os.WriteFile(s.GeneratedCatalogPath, []byte(`{"models":[{"base_instructions":"original"}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			l := Launch{Executable: harness, Args: []string{"-c", "model_catalog_json=base", "task"}, Env: []string{`OPENCODE_CONFIG_CONTENT={"model":"hrx/test"}`}}
			if err := prepareYesod(context.Background(), s, o, &l, io.Discard); err != nil {
				t.Fatal(err)
			}
			text := strings.Join(l.Args, " ")
			if harness == "codex" {
				paths, _ := filepath.Glob(filepath.Join(filepath.Dir(s.CachePath), "codex-yesod-*.json"))
				if len(paths) != 1 {
					t.Fatal("missing catalog")
				}
				data, err := os.ReadFile(paths[0])
				if err != nil {
					t.Fatal(err)
				}
				text = string(data)
			}
			if !strings.Contains(text, "GitLab service account") || !strings.Contains(text, "Pitput communications") || strings.Contains(text, "fixture-service-token") {
				t.Fatal("lost or leaked priming")
			}
		})
	}
}

func TestBrokerGlabCredentials(t *testing.T) {
	glab, err := exec.LookPath("glab")
	if err != nil {
		t.Skip("glab not installed")
	}
	_, o := brokerFixture(t)
	l := Launch{}
	if _, err := prepareBroker(context.Background(), o, &l); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/user" || r.Header.Get("PRIVATE-TOKEN") != "fixture-service-token" {
			w.WriteHeader(403)
			return
		}
		io.WriteString(w, `{"id":42,"username":"hrx_fixture"}`)
	}))
	defer api.Close()
	// Loopback transport is HTTP only in this fixture. Production pins HTTPS.
	host := strings.TrimPrefix(api.URL, "http://")
	l.Env = childEnvironment(l.Env, map[string]string{"GITLAB_HOST": host, "GITLAB_API_HOST": host, "GLAB_API_PROTOCOL": "http", "API_PROTOCOL": "http", "GLAB_CONFIG_DIR": t.TempDir(), "GLAB_CHECK_UPDATE": "false", "GLAB_SEND_TELEMETRY": "false"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, glab, "api", "user")
	cmd.Dir = t.TempDir()
	cmd.Env = l.Env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("glab fixture: %v: %s", err, output)
	}
	if !strings.Contains(string(output), `"username":"hrx_fixture"`) {
		t.Fatal("glab did not use service account")
	}
}
