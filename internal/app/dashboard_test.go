package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/monitor"
	"github.com/meshcrawler/hrx/internal/session"
)

func TestDashboardHarnessHelper(t *testing.T) {
	if os.Getenv("HRX_DASHBOARD_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	if err := execHarness(Launch{Executable: "claude", Identity: "fixture-model", Passthrough: []string{"--name", "fixture-agent"}}); err != nil {
		t.Fatal(err)
	}
}

func TestDashboardProcessLifecycle(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nprintf '%s\\n' \"$HRX_MONITOR_RUN\"\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDashboardHarnessHelper$")
	cmd.Env = childEnvironment(os.Environ(), map[string]string{"HRX_DASHBOARD_CHILD": "1", "HRX_MONITOR": "on", "XDG_STATE_HOME": root, "PATH": bin + ":" + os.Getenv("PATH")}, nil)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	store := monitor.Store{Dir: filepath.Join(root, "hrx", "monitor")}
	rows, err := store.Snapshot(ctx)
	if err != nil || len(rows) != 1 || rows[0].RunID != strings.TrimSpace(line) || rows[0].Status != "running" || rows[0].Agent != "fixture-agent" {
		t.Fatalf("launch not tracked: %+v %v", rows, err)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	rows, err = store.Snapshot(ctx)
	if err != nil || rows[0].Status != "exited" {
		t.Fatal("exit not observed", err)
	}
}

func TestNativeClaudeDashboardActivity(t *testing.T) {
	if os.Getenv("HRX_NATIVE_SESSION_TESTS") != "1" {
		t.Skip("opt-in native CLI fixture")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	executable := filepath.Join(root, "hrx")
	if output, err := exec.Command("go", "build", "-o", executable, "../../cmd/hrx").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"offline fixture"}}`)
	}))
	defer server.Close()
	state := filepath.Join(root, "state")
	store := monitor.Store{Dir: filepath.Join(state, "hrx", "monitor")}
	record, err := store.Register(monitor.Record{Agent: "native-fixture", Harness: "claude", Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	l := Launch{Executable: "claude", Args: []string{"--model", "fixture", "--max-turns", "1", "--strict-mcp-config", "-p", "SECRET_PROMPT_MUST_NOT_BE_SAVED"}, Env: []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + root, "XDG_STATE_HOME=" + state, "CLAUDE_CONFIG_DIR=" + filepath.Join(root, "claude"),
		"ANTHROPIC_BASE_URL=" + server.URL, "ANTHROPIC_API_KEY=fixture", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", monitor.RunEnv + "=" + record.RunID,
	}}
	if err := attachSessionHook(&l, sessionHookCommand(executable), session.Record{Harness: "claude", Identity: "fixture", Route: "fixture", Project: root}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, l.Args...)
	cmd.Dir = root
	cmd.Env = l.Env
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	_ = cmd.Run()
	if ctx.Err() != nil {
		t.Fatal("native CLI fixture timed out")
	}
	rows, err := store.Snapshot(context.Background())
	if err != nil || len(rows) != 1 || rows[0].SessionID == "" || (rows[0].Activity != "user-message" && rows[0].Activity != "assistant-turn") {
		t.Fatalf("native activity hook not recorded: %+v %v", rows, err)
	}
	b, err := os.ReadFile(filepath.Join(store.Dir, record.RunID+".json"))
	if err != nil || bytes.Contains(b, []byte("SECRET_PROMPT")) {
		t.Fatal("prompt content persisted")
	}
}

func TestDashboardLaunchAndActivity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	detached := Launch{Executable: "claude", Passthrough: []string{"--bg"}}
	trackLaunch(&detached)
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_STATE_HOME"), "hrx", "monitor")); !os.IsNotExist(err) {
		t.Fatal("detached launcher should not claim native-process liveness")
	}
	l := Launch{Executable: "claude", Identity: "claude-opus-5-5", Broker: &BrokerLaunch{Identity: "pitput-dev-h1", Credential: monitor.Credential{Provider: "gitlab", Username: "hrx_test", Profile: "developer"}}}
	trackLaunch(&l)
	var id string
	for _, entry := range l.Env {
		if strings.HasPrefix(entry, monitor.RunEnv+"=") {
			id = strings.TrimPrefix(entry, monitor.RunEnv+"=")
		}
	}
	if id == "" {
		t.Fatal("no observation ID")
	}
	t.Setenv(monitor.RunEnv, id)
	if err := Run(context.Background(), []string{"_activity", "assistant-turn"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	store, _ := monitor.Default()
	rows, err := store.Snapshot(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Agent != "pitput-dev-h1" || rows[0].Activity != "assistant-turn" || rows[0].Credential.Profile != "developer" {
		t.Fatal("incorrect observation", err)
	}
	t.Setenv("HRX_MONITOR", "off")
	l.Env = append(l.Env, monitor.RunEnv+"=parent")
	trackLaunch(&l)
	for _, entry := range l.Env {
		if strings.HasPrefix(entry, monitor.RunEnv+"=") {
			t.Fatal("disabled launch inherited parent observation")
		}
	}
}

func TestDashboardClaudeHooksAndNoContent(t *testing.T) {
	l := Launch{Executable: "claude"}
	if err := attachSessionHook(&l, sessionHookCommand("/path with spaces/hrx"), session.Record{}); err != nil {
		t.Fatal(err)
	}
	var config struct {
		Hooks map[string][]struct{ Hooks []struct{ Command string } }
	}
	for i, arg := range l.Args {
		if arg == "--settings" {
			if err := json.Unmarshal([]byte(l.Args[i+1]), &config); err != nil {
				t.Fatal(err)
			}
		}
	}
	for event, kind := range map[string]string{"UserPromptSubmit": "user-message", "Stop": "assistant-turn", "PostToolUse": "tool-use"} {
		if len(config.Hooks[event]) != 1 || !strings.HasSuffix(config.Hooks[event][0].Hooks[0].Command, "_activity "+kind) {
			t.Fatal("missing hook", event)
		}
	}
	for _, args := range [][]string{{"--listen", "0.0.0.0:8787"}, {"--source", "local=http://127.0.0.1:8888"}, {"--source", "other=http://example.com"}} {
		if err := runDashboard(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Fatal("unsafe dashboard arguments accepted")
		}
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(monitor.RunEnv, "")
	if err := Run(context.Background(), []string{"_activity", "user-message"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_STATE_HOME"), "hrx", "monitor")); !os.IsNotExist(err) {
		t.Fatal("hook outside HRX wrote state")
	}
}
