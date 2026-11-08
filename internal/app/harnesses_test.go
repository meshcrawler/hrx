package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/catalog"
)

func otherSnapshot(harness string) catalog.Snapshot {
	s := snapshot()
	wire := map[string]string{"claude": "messages", "opencode": "chat_completions"}[harness]
	s.Models[0].Harnesses = map[string]catalog.Capability{harness: {WireAPI: wire}}
	s.Models[0].Context, s.Models[0].Output = 100000, 10000
	return s
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		m[k] = v
	}
	return m
}

func TestOtherHarnessRoutingAndSecretIsolation(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "wrong-provider-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "wrong-token")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "subscription-token")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	for _, harness := range []string{"claude", "opencode"} {
		t.Run(harness, func(t *testing.T) {
			s := testSettings(t)
			s.Harness = harness
			l, err := buildOtherLaunch(s, otherSnapshot(harness), LaunchOptions{Extra: []string{"a prompt with spaces"}}, "test-secret")
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(l)
			if strings.Contains(string(encoded), "secret") || strings.Contains(strings.Join(l.Args, " "), "secret") {
				t.Fatal("credential leaked")
			}
			e := envMap(l.Env)
			if harness == "claude" {
				if e["ANTHROPIC_API_KEY"] != "" || e["ANTHROPIC_AUTH_TOKEN"] != "" || e["CLAUDE_CODE_USE_BEDROCK"] != "" {
					t.Fatal("auth override inherited")
				}
				if e["CLAUDE_CODE_OAUTH_TOKEN"] != "subscription-token" || e["ANTHROPIC_CUSTOM_HEADERS"] != "x-litellm-api-key: Bearer test-secret" {
					t.Fatal("dual auth lost")
				}
				if e["ANTHROPIC_BASE_URL"] != "https://gateway.example" || e["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != l.Model {
					t.Fatal("route not pinned")
				}
			} else {
				if e["HRX_GATEWAY_KEY"] != "test-secret" || strings.Contains(e["OPENCODE_CONFIG_CONTENT"], "test-secret") {
					t.Fatal("unsafe OpenCode runtime config")
				}
				var config map[string]any
				if json.Unmarshal([]byte(e["OPENCODE_CONFIG_CONTENT"]), &config) != nil || config["model"] != "hrx/provider/example" {
					t.Fatal("invalid runtime config")
				}
			}
			if _, err = buildOtherLaunch(s, otherSnapshot(harness), LaunchOptions{Reasoning: "high"}, "key"); err == nil {
				t.Fatal("invented reasoning accepted")
			}
		})
	}
}

func TestClaudeGatewayAuthWithoutPersonalLogin(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "unrelated-vendor-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "unrelated-auth-token")
	s := testSettings(t)
	s.Harness = "claude"
	launch, err := buildOtherLaunch(s, otherSnapshot("claude"), LaunchOptions{}, "selected-gateway-key")
	if err != nil {
		t.Fatal(err)
	}
	env := envMap(launch.Env)
	if env["ANTHROPIC_AUTH_TOKEN"] != "selected-gateway-key" || env["ANTHROPIC_API_KEY"] != "" {
		t.Fatal("clean native home must authenticate with selected gateway credential")
	}
	encoded, _ := json.Marshal(launch)
	if strings.Contains(string(encoded), "selected-gateway-key") || strings.Contains(strings.Join(launch.Args, " "), "selected-gateway-key") {
		t.Fatal("gateway credential escaped private child environment")
	}
}

func TestOpenCodeModelFlagBelongsToRun(t *testing.T) {
	s := testSettings(t)
	s.Harness = "opencode"
	interactive, err := buildOtherLaunch(s, otherSnapshot("opencode"), LaunchOptions{}, "key")
	if err != nil || len(interactive.Args) != 0 {
		t.Fatalf("interactive model must come from config: %v %v", interactive.Args, err)
	}
	run, err := buildOtherLaunch(s, otherSnapshot("opencode"), LaunchOptions{Extra: []string{"run", "task"}}, "key")
	if err != nil || strings.Join(run.Args, " ") != "run --model hrx/provider/example task" {
		t.Fatalf("%v %v", run.Args, err)
	}
}

func TestHarnessSpecificPassthrough(t *testing.T) {
	for _, harness := range []string{"claude", "opencode"} {
		for _, flag := range []string{"-p", "-c"} {
			if _, err := parseLaunchFor(harness, []string{"--", flag, "hello"}); err != nil {
				t.Fatal(err)
			}
		}
		for _, flag := range []string{"--model=x", "-mx", "--effort", "--variant=high", "--settings", "--fallback-model", "--attach", "--agents"} {
			if _, err := parseLaunchFor(harness, []string{"--", flag}); err == nil {
				t.Fatalf("accepted %s %s", harness, flag)
			}
		}
	}
}

func TestHarnessConfigAndOfflineCompletion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(dir, "hrx.toml")
	t.Setenv("HRX_CONFIG", path)
	data := `[claude]
model = "example"
[claude.provider]
base_url = "https://gateway.example/v1"
env_key = "HRX_MISSING_KEY"
[opencode.provider]
base_url = "https://gateway.example/v1"
env_key = "HRX_MISSING_KEY"
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := loadHarnessSettings("claude")
	if err != nil {
		t.Fatal(err)
	}
	other, err := loadHarnessSettings("opencode")
	if err != nil || other.Binding == s.Binding {
		t.Fatal("harness binding collision", err)
	}
	snap := otherSnapshot("claude")
	snap.Binding = s.Binding
	if err := catalog.Write(s.CachePath, snap); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := Run(context.Background(), []string{"_complete", "models", "claude"}, &out, &errOut); err != nil || out.String() != "example\n" {
		t.Fatal("completion attempted auth or lost models", err, out.String())
	}
	snap.GeneratedAt = time.Now().Add(-time.Hour)
	catalog.Write(s.CachePath, snap)
	out.Reset()
	Run(context.Background(), []string{"_complete", "models", "claude"}, &out, &errOut)
	if out.Len() != 0 {
		t.Fatal("stale completion")
	}
	if !slices.Contains(strings.Split(ZshCompletion, "\n"), "    codex|claude|opencode)") {
		t.Fatal("completion missing adapters")
	}
}

func TestOpenCodeCredentialHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.json")
	for _, key := range []string{"test-key", "{env:HRX_TEST_OPEN_KEY}"} {
		t.Setenv("HRX_TEST_OPEN_KEY", "test-key")
		data, _ := json.Marshal(map[string]any{"provider": map[string]any{"litellm": map[string]any{"options": map[string]string{"baseURL": "https://gateway.example/v1", "apiKey": key}}}})
		os.WriteFile(path, data, 0600)
		got, err := credential(context.Background(), Provider{CredentialFile: path})
		if err != nil || got != "test-key" {
			t.Fatal("credential reference failed", err)
		}
	}
}

func TestClaudeRequiresConfiguredProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hrx.toml")
	t.Setenv("HRX_CONFIG", path)
	if err := os.WriteFile(path, []byte("[permissions]\nyolo = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := loadHarnessSettings("claude")
	if err == nil || !strings.Contains(err.Error(), "[claude.provider]") {
		t.Fatal("Claude launched without a configured gateway provider", err)
	}
}
