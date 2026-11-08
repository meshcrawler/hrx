package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPitputPrimeCompatibility(t *testing.T) {
	for _, tc := range []struct{ name, script, want, calls string }{
		{"platform", "printf 'platform:%s' \"$3\"", "platform:claude", "prime --platform claude\n"},
		{"old", "if [ \"$#\" -gt 1 ]; then echo private-error >&2; exit 2; fi; printf legacy", "legacy", "prime --platform claude\nprime\n"},
		{"failed", "echo private-error >&2; exit 1", "", "prime --platform claude\nprime\n"},
		{"empty", "exit 0", "", "prime --platform claude\nprime\n"},
		{"oversized", "printf '%070000d' 0", "", "prime --platform claude\nprime\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "calls")
			t.Setenv("HRX_TEST_PRIME_LOG", log)
			script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HRX_TEST_PRIME_LOG\"\n" + tc.script + "\n"
			if err := os.WriteFile(filepath.Join(dir, "pitput"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			if got := pitputPrime(context.Background(), "claude"); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			calls, err := os.ReadFile(log)
			if err != nil || string(calls) != tc.calls {
				t.Fatalf("calls %q: %v", calls, err)
			}
		})
	}
}

func TestPitputCancelledPrimeDoesNotStartCLI(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	t.Setenv("HRX_TEST_PRIME_LOG", marker)
	if err := os.WriteFile(filepath.Join(dir, "pitput"), []byte("#!/bin/sh\nprintf x > \"$HRX_TEST_PRIME_LOG\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := pitputPrime(ctx, "codex"); got != "" {
		t.Fatalf("unexpected output %q", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("cancelled priming invoked CLI")
	}
}

func TestPitputPrimeBoundsRunningCommand(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pitput"), []byte("#!/bin/sh\nexec /bin/sleep 20\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	start := time.Now()
	if got := pitputPrime(context.Background(), "claude"); got != "" {
		t.Fatal("timed-out prime returned instructions")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("priming exceeded its bounded deadline: %s", elapsed)
	}
}

func TestPitputDisabledAndDryRunNeverInvokeInstalledCLI(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "called")
	t.Setenv("HRX_TEST_PRIME_LOG", marker)
	if err := os.WriteFile(filepath.Join(dir, "pitput"), []byte("#!/bin/sh\nprintf x > \"$HRX_TEST_PRIME_LOG\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, options := range []LaunchOptions{{}, {Pitput: true, DryRun: true}} {
		launch := Launch{Executable: "claude", Args: []string{"user-task"}}
		var diagnostics bytes.Buffer
		if err := prepareYesod(context.Background(), testSettings(t), options, &launch, &diagnostics); err != nil {
			t.Fatal(err)
		}
		if len(launch.Args) != 1 || launch.Args[0] != "user-task" || diagnostics.Len() != 0 {
			t.Fatal("disabled/dry-run modified launch")
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("disabled/dry-run invoked installed Pitput")
	}
}

func TestPitputMissingCLIAndPlatformBoundaries(t *testing.T) {
	t.Setenv("PATH", "")
	for platform, want := range map[string]string{"codex": "private Codex server", "claude": "explicit handle ACK", "opencode": "private OpenCode server", "other": "Verify runtime-specific delivery"} {
		p := pitputPrompt(context.Background(), platform, "host/hrx/agent")
		for _, fragment := range []string{want, "priming was unavailable", "host/hrx/agent", "untrusted data", "Do not ACK on receipt"} {
			if !strings.Contains(p, fragment) {
				t.Fatalf("%s missing %q", platform, fragment)
			}
		}
	}
}

func TestPitputLaunchInjectionAndDryRun(t *testing.T) {
	t.Setenv("PATH", "")
	config := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(config, []byte("[agents]\nworkspace_root = \"~/hrx-agents/\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRX_CONFIG", config)
	for _, harness := range []string{"codex", "claude", "opencode"} {
		t.Run(harness, func(t *testing.T) {
			s := testSettings(t)
			s.GeneratedCatalogPath = filepath.Join(filepath.Dir(s.CachePath), "base.json")
			base := `{"models":[{"base_instructions":"original instructions"}]}`
			if err := os.WriteFile(s.GeneratedCatalogPath, []byte(base), 0600); err != nil {
				t.Fatal(err)
			}
			l := Launch{Executable: harness, Args: []string{"user-task"}}
			if harness == "codex" {
				l.Args = []string{"-c", "model_catalog_json=\"base\"", "user-task"}
			}
			if harness == "opencode" {
				l.Env = []string{`OPENCODE_CONFIG_CONTENT={"model":"hrx/route"}`}
			}
			var out bytes.Buffer
			if err := prepareYesod(context.Background(), s, LaunchOptions{Pitput: true, DryRun: true}, &l, &out); err != nil {
				t.Fatal(err)
			}
			if !l.Pitput || l.Yesod != nil || strings.Contains(strings.Join(l.Args, " "), "Pitput communications") {
				t.Fatal("dry-run injected or registered")
			}
			if harness == "codex" || harness == "opencode" || harness == "claude" {
				dir := t.TempDir()
				os.WriteFile(filepath.Join(dir, "pitput"), []byte("#!/bin/sh\nif [ \"$1\" = agent ]; then printf '%s' '{\"identity\":\"host/test/agent\",\"boards\":[\"test\"],\"status\":\"ready\"}'; fi\n"), 0700)
				t.Setenv("PATH", dir)
			}
			if err := prepareYesod(context.Background(), s, LaunchOptions{Pitput: true, AgentIdentity: "host/test/agent"}, &l, &out); err != nil {
				t.Fatal(err)
			}
			if l.Yesod != nil || l.lease != nil || !strings.Contains(strings.Join(l.Args, " "), "user-task") {
				t.Fatal("lost task or allocated Yesod lease")
			}
			if harness == "codex" {
				paths, _ := filepath.Glob(filepath.Join(filepath.Dir(s.CachePath), "codex-yesod-*.json"))
				if len(paths) != 1 {
					t.Fatal("missing injected catalog")
				}
				data, err := os.ReadFile(paths[0])
				if err != nil || !bytes.Contains(data, []byte("Pitput communications")) || !bytes.Contains(data, []byte("original instructions")) || !bytes.Contains(data, []byte("~/hrx-agents/")) {
					t.Fatal("lost guidance")
				}
			} else if !strings.Contains(strings.Join(l.Args, " "), "Pitput communications") || !strings.Contains(strings.Join(l.Args, " "), "~/hrx-agents/<agent-name>/") {
				t.Fatal("missing guidance")
			}
		})
	}
}

func TestPitputOptions(t *testing.T) {
	for _, harness := range []string{"codex", "claude", "opencode"} {
		o, err := parseLaunchFor(harness, []string{"--pitput", "--yesod-comms"})
		if err != nil || !o.Pitput || !o.YesodMail || !o.YesodBBS {
			t.Fatalf("options: %+v %v", o, err)
		}
		for _, args := range [][]string{{"--pitput=false"}, {"--", "--pitput"}, {"--pitput", "--", "--fork"}} {
			if _, err := parseLaunchFor(harness, args); err == nil {
				t.Fatalf("accepted %v", args)
			}
		}
	}
}

func TestAgentWorkspaceGuidanceIsOptIn(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(config, []byte("[permissions]\nyolo = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRX_CONFIG", config)
	if got := agentWorkspacePrompt(); got != "" {
		t.Fatal("workspace guidance injected without [agents] workspace_root", got)
	}
	if err := os.WriteFile(config, []byte("[agents]\nworkspace_root = \"/srv/agents\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := agentWorkspacePrompt(); !strings.Contains(got, "/srv/agents/<agent-name>/") {
		t.Fatal("configured workspace root not used", got)
	}
}
