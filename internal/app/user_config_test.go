package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestUserConfigAndCredentialPaths(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HRX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("HRX_GITLAB_BROKER_URL", "")
	t.Setenv("HRX_GITLAB_BROKER_TOKEN_FILE", "")
	t.Setenv("HRX_CODEX_PROVIDER", "")
	t.Setenv("CODEX_HOME", dir)
	config := `model_provider="unused"
[model_providers.gateway]
wire_api="responses"
base_url="https://gateway.example/v1"
env_key="FIXTURE_KEY"
[codex]
provider_name="gateway"
[plugins.gitlab-service-account-broker]
url="http://127.0.0.1:8743"
`
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings()
	if err != nil || s.ProviderName != "gateway" {
		t.Fatal("saved provider not selected", err)
	}
	origin, path, err := brokerConfiguration("laptop/dev/agent")
	if err != nil || origin != "http://127.0.0.1:8743" || path != filepath.Join(dir, "client-tokens", "laptop", "dev", "agent", "broker.token") {
		t.Fatal("incorrect config lookup", err)
	}
	for _, identity := range []string{"../dev", "laptop/../dev", "laptop//dev", "/laptop/dev", "laptop/./agent"} {
		if _, _, err := brokerConfiguration(identity); err == nil {
			t.Fatal("unsafe identity accepted", identity)
		}
	}
	t.Setenv("HRX_GITLAB_BROKER_URL", "http://127.0.0.1:9999")
	t.Setenv("HRX_GITLAB_BROKER_TOKEN_FILE", "/explicit/token")
	origin, path, err = brokerConfiguration("laptop/dev/agent")
	if err != nil || origin != "http://127.0.0.1:9999" || path != "/explicit/token" {
		t.Fatal("environment overrides ignored")
	}
}

func TestGitlabAlias(t *testing.T) {
	for _, h := range []string{"claude", "codex", "opencode"} {
		o, err := parseLaunchFor(h, []string{"--gitlab", "--agent-identity", "laptop/dev/agent"})
		if err != nil || o.Plugin != "gitlab-service-account-broker" {
			t.Fatal("alias failed", h, err)
		}
		for _, args := range [][]string{{"--gitlab=true"}, {"--gitlab"}, {"--gitlab", "--plugin=gitlab-service-account-broker"}, {"--plugin=gitlab-service-account-broker", "--gitlab"}, {"--", "--gitlab"}} {
			if _, err := parseLaunchFor(h, args); err == nil {
				t.Fatal("invalid alias usage accepted", args)
			}
		}
	}
}

func TestConfiguredYoloAndOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HRX_CONFIG", filepath.Join(dir, "config.toml"))
	if err := os.WriteFile(os.Getenv("HRX_CONFIG"), []byte("[permissions]\nyolo=true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"codex", "claude", "opencode"} {
		for _, tc := range []struct {
			args []string
			want bool
		}{{nil, true}, {[]string{"--no-yolo"}, false}, {[]string{"--yolo"}, true}} {
			o, err := parseLaunchFor(h, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			o, err = applyLaunchDefaults(o)
			if err != nil || o.Yolo != tc.want {
				t.Fatal("permission default/override failed", h, tc.args)
			}
		}
	}
	if err := os.WriteFile(os.Getenv("HRX_CONFIG"), []byte("[permissions]\nyolo=false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	o, err := applyLaunchDefaults(LaunchOptions{})
	if err != nil || o.Yolo {
		t.Fatal("configured approvals ignored")
	}
}

func TestEnrollmentRetryAndConcurrentReuse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HRX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("HRX_GITLAB_BROKER_URL", "")
	t.Setenv("HRX_GITLAB_BROKER_TOKEN_FILE", "")
	secret := strings.Repeat("h", 48)
	if err := os.WriteFile(filepath.Join(dir, "enrollment.token"), []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var received []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var body map[string]string
		if r.URL.Path != "/v1/enroll" || r.Header.Get("Authorization") != "Bearer "+secret || json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("bad enrollment request")
			w.WriteHeader(400)
			return
		}
		received = append(received, body["client_token"])
		if len(received) == 1 {
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"identity": body["identity"], "enrolled": true})
	}))
	defer server.Close()
	config := fmt.Sprintf("[plugins.gitlab-service-account-broker]\nurl=%q\nenrollment_token_file=\"enrollment.token\"\n", server.URL)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	origin, path, err := brokerConfiguration("laptop/dev/agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := enrollBrokerClient(context.Background(), origin, "laptop/dev/agent", path); err == nil {
		t.Fatal("failure not reported")
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := enrollBrokerClient(context.Background(), origin, "laptop/dev/agent", path); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(received) != 2 || received[0] != received[1] {
		t.Fatal("retry changed credential or enrolled more than once")
	}
	stored, err := readBrokerSecret(path)
	if err != nil || stored != received[0] {
		t.Fatal("credential not activated")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("credential not private")
	}
}
