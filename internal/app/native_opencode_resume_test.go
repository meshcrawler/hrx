package app

import (
	"context"
	"encoding/json"
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

func TestNativeOpenCodeResumeRetainsContext(t *testing.T) {
	if os.Getenv("HRX_NATIVE_SESSION_TESTS") != "1" {
		t.Skip("set HRX_NATIVE_SESSION_TESTS=1 for isolated native CLI checks")
	}
	binary, err := exec.LookPath("opencode")
	if err != nil {
		t.Fatal("opencode is required")
	}
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	project, _ = filepath.EvalSymlinks(project)
	state := filepath.Join(root, "state")
	t.Setenv("XDG_STATE_HOME", state)
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
			var payload struct {
				Model    string `json:"model"`
				Messages []struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			if json.NewDecoder(io.LimitReader(r.Body, 4*1024*1024)).Decode(&payload) != nil || payload.Model != "fixture" {
				t.Error("unexpected native OpenCode model/payload; payload suppressed")
			}
			var messages []string
			for _, message := range payload.Messages {
				if message.Role == "user" {
					messages = append(messages, string(message.Content))
				}
			}
			mu.Lock()
			requests = append(requests, strings.Join(messages, "\n"))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"offline fixture: inference disabled"}}`)
	}))
	defer server.Close()
	config, _ := json.Marshal(map[string]any{
		"model": "hrx/fixture", "small_model": "hrx/fixture", "enabled_providers": []string{"hrx"},
		"provider": map[string]any{"hrx": map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "fixture", "options": map[string]string{"baseURL": server.URL + "/v1", "apiKey": "nonsecret-test-placeholder"}, "models": map[string]any{"fixture": map[string]string{"name": "fixture"}}}},
	})
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root,
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_DATA_HOME=" + filepath.Join(root, "data"),
		"XDG_CACHE_HOME=" + filepath.Join(root, "cache"), "XDG_STATE_HOME=" + state,
		"OPENCODE_CONFIG_CONTENT=" + string(config)}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, _ := session.Default()
	var prior session.Record
	for _, mode := range []string{"fresh", "resume", "continue"} {
		o := LaunchOptions{Model: "fixture", project: project, Extra: []string{"run", "offline " + mode + " fixture"}}
		if mode != "fresh" {
			o.Model = ""
			if mode == "resume" {
				o.Resume = prior.NativeID
			} else {
				o.Continue = true
			}
			o, _, err = restoreSession(store, "opencode", project, o)
			if err != nil || o.Model != "fixture" || o.Resume != prior.NativeID || o.Yolo {
				t.Fatalf("%s restoration failed: %v", mode, err)
			}
		}
		args, err := sessionArguments("opencode", o)
		if err != nil {
			t.Fatal(err)
		}
		launch := Launch{Executable: "opencode", Identity: "fixture", Model: o.Model, Env: env, Args: insertOpenCodeFlags(args, "--model", "hrx/"+o.Model)}
		if err := prepareSessionCapture(ctx, o, &launch, io.Discard); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		before := len(requests)
		mu.Unlock()
		cmd := exec.CommandContext(ctx, binary, launch.Args...)
		cmd.Dir, cmd.Env = project, launch.Env
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		cmd.WaitDelay = time.Second
		if err := launch.startWithSession(cmd.Start); err != nil {
			t.Fatal("native OpenCode start failed", err)
		}
		_ = cmd.Wait()
		if ctx.Err() != nil {
			t.Fatalf("native OpenCode %s fixture timed out", mode)
		}
		saved, err := store.Resolve("opencode", project, "")
		if err != nil || saved.Route != "fixture" || (mode != "fresh" && (saved.NativeID != prior.NativeID || !saved.UpdatedAt.After(prior.UpdatedAt))) {
			t.Fatalf("%s did not index the same native session: %v", mode, err)
		}
		mu.Lock()
		observed := strings.Join(requests[before:], "\n")
		mu.Unlock()
		if !strings.Contains(observed, "offline fresh fixture") || !strings.Contains(observed, "offline "+mode+" fixture") || (mode == "continue" && !strings.Contains(observed, "offline resume fixture")) {
			t.Fatalf("%s did not retain prior/current user messages; payload suppressed", mode)
		}
		prior = saved
	}
}
