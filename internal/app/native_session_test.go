package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/session"
)

func TestSessionHookProcessHelper(t *testing.T) {
	if os.Getenv("HRX_HOOK_PROCESS_TEST") != "1" {
		t.Skip("native hook subprocess helper")
	}
	if err := runSessionStartHook(os.Stdin); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestNativeCodexDoesNotBypassHookTrust(t *testing.T) {
	if os.Getenv("HRX_NATIVE_SESSION_TESTS") != "1" {
		t.Skip("set HRX_NATIVE_SESSION_TESTS=1 for isolated native CLI checks")
	}
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal("codex is required")
	}
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"offline fixture: inference disabled"}}`)
	}))
	defer server.Close()
	state := filepath.Join(root, "state")
	launch := Launch{Executable: "codex", Args: []string{
		"-c", `model_provider="fixture"`, "-c", `model_providers.fixture.name="fixture"`,
		"-c", fmt.Sprintf("model_providers.fixture.base_url=%q", server.URL+"/v1"),
		"-c", `model_providers.fixture.wire_api="responses"`, "-c", `model_providers.fixture.env_key="HRX_FIXTURE_KEY"`,
		"-c", `model_providers.fixture.request_max_retries=0`, "-c", `model_providers.fixture.stream_max_retries=0`,
		"exec", "--skip-git-repo-check", "--model", "fixture", "offline startup fixture",
	}, Env: []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + root, "CODEX_HOME=" + filepath.Join(root, "codex"),
		"XDG_STATE_HOME=" + state, "HRX_FIXTURE_KEY=nonsecret-test-placeholder", "HRX_HOOK_PROCESS_TEST=1",
	}}
	command := strings.TrimSuffix(sessionHookCommand(os.Args[0]), " _session-start") + " -test.run=^TestSessionHookProcessHelper$"
	if err := attachSessionHook(&launch, command, session.Record{Harness: "codex", Identity: "fixture", Route: "fixture", Project: project}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, codex, launch.Args...)
	cmd.Dir, cmd.Env = project, launch.Env
	cmd.WaitDelay = time.Second
	output, _ := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("native Codex fixture timed out")
	}
	text := strings.ToLower(string(output))
	if !strings.Contains(text, "hook") || (!strings.Contains(text, "trust") && !strings.Contains(text, "review")) {
		t.Fatal("native Codex did not report hook trust requirement; output suppressed")
	}
	if _, err := os.Stat(filepath.Join(state, "hrx", "sessions", "index.json")); !os.IsNotExist(err) {
		t.Fatal("untrusted native hook wrote metadata")
	}
}

func TestNativeClaudeSessionStartCapture(t *testing.T) {
	if os.Getenv("HRX_NATIVE_SESSION_TESTS") != "1" {
		t.Skip("set HRX_NATIVE_SESSION_TESTS=1 for isolated native CLI checks")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal("claude is required")
	}
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	project, _ = filepath.EvalSymlinks(project)
	var requestMu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/messages" {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
			var payload struct {
				Model    string `json:"model"`
				Messages []struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			if json.Unmarshal(body, &payload) != nil || payload.Model != "fixture" {
				t.Error("native Claude request used an unexpected model or invalid payload; payload suppressed")
			}
			var userMessages []string
			for _, message := range payload.Messages {
				if message.Role == "user" {
					userMessages = append(userMessages, string(message.Content))
				}
			}
			requestMu.Lock()
			requests = append(requests, strings.Join(userMessages, "\n"))
			requestMu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"offline fixture: inference disabled"}}`)
	}))
	defer server.Close()
	state := filepath.Join(root, "state")
	launch := Launch{Executable: "claude", Identity: "fixture", Model: "fixture", Args: []string{"--model", "fixture", "--max-turns", "1", "-p", "offline startup fixture"}, Env: []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + root, "XDG_STATE_HOME=" + state,
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_DATA_HOME=" + filepath.Join(root, "data"),
		"XDG_CACHE_HOME=" + filepath.Join(root, "cache"), "CLAUDE_CONFIG_DIR=" + filepath.Join(root, "claude"),
		"ANTHROPIC_BASE_URL=" + server.URL, "ANTHROPIC_API_KEY=nonsecret-test-placeholder",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "HRX_HOOK_PROCESS_TEST=1",
	}}
	command := strings.TrimSuffix(sessionHookCommand(os.Args[0]), " _session-start") + " -test.run=^TestSessionHookProcessHelper$"
	if err := attachSessionHook(&launch, command, session.Record{Harness: "claude", Identity: "fixture", Route: "fixture", Project: project}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, claude, launch.Args...)
	cmd.Dir, cmd.Env = project, launch.Env
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.WaitDelay = time.Second
	_ = cmd.Run() // The fixture intentionally rejects the inference request.
	if ctx.Err() != nil {
		t.Fatal("native Claude fixture timed out")
	}
	store := session.Store{Dir: filepath.Join(state, "hrx", "sessions")}
	r, err := store.Resolve("claude", project, "")
	if err != nil || r.NativeID == "" || r.Route != "fixture" {
		t.Fatal("native SessionStart failed to capture metadata", err)
	}
	for _, mode := range []string{"resume", "continue"} {
		o := LaunchOptions{Extra: []string{"--max-turns", "1", "-p", "offline " + mode + " fixture"}}
		if mode == "resume" {
			o.Resume = r.NativeID
		} else {
			o.Continue = true
		}
		o, _, err = restoreSession(store, "claude", project, o)
		if err != nil || o.Model != "fixture" || o.Resume != r.NativeID || o.Yolo {
			t.Fatalf("%s metadata restoration failed: %v", mode, err)
		}
		args, err := sessionArguments("claude", o)
		if err != nil {
			t.Fatal(err)
		}
		next := Launch{Executable: "claude", Args: append([]string{"--model", o.Model}, args...), Env: launch.Env}
		if err := attachSessionHook(&next, command, session.Record{NativeID: o.Resume, Harness: "claude", Identity: "fixture", Route: o.Model, Project: project}); err != nil {
			t.Fatal(err)
		}
		requestMu.Lock()
		before := len(requests)
		requestMu.Unlock()
		cmd := exec.CommandContext(ctx, claude, next.Args...)
		cmd.Dir, cmd.Env = project, next.Env
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		cmd.WaitDelay = time.Second
		_ = cmd.Run() // Reject locally after observing the restored request context.
		if ctx.Err() != nil {
			t.Fatalf("native Claude %s fixture timed out", mode)
		}
		updated, err := store.Resolve("claude", project, "")
		if err != nil || updated.NativeID != r.NativeID || !updated.UpdatedAt.After(r.UpdatedAt) {
			t.Fatalf("%s did not recapture the original native session: %v", mode, err)
		}
		requestMu.Lock()
		observed := strings.Join(requests[before:], "\n")
		requestMu.Unlock()
		if !strings.Contains(observed, "offline startup fixture") || !strings.Contains(observed, "offline "+mode+" fixture") {
			t.Fatalf("%s request did not retain prior and current user messages; payload suppressed", mode)
		}
		if mode == "continue" && !strings.Contains(observed, "offline resume fixture") {
			t.Fatal("continue lost the intervening resumed turn; payload suppressed")
		}
		r = updated
	}
}
