package app

import (
	"context"
	"encoding/json"
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

func fakeOpenCodeSessionAPI(t *testing.T, response any, status string) (Launch, string, string) {
	t.Helper()
	dir := t.TempDir()
	project, _ := filepath.EvalSymlinks(t.TempDir())
	marker := filepath.Join(dir, "calls")
	data, _ := json.Marshal(response)
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$HRX_TEST_CALLS\"\nprintf '%s' \"$HRX_TEST_RESPONSE\"\nexit " + status + "\n"
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	launch := Launch{Model: "route", Reasoning: "high", Env: append(os.Environ(), "HRX_TEST_CALLS="+marker, "HRX_TEST_RESPONSE="+string(data))}
	return launch, project, marker
}

// This opt-in native test creates only an empty local session in disposable
// state. It sends no prompt and has neither provider credentials nor a remote
// model endpoint. It is separate from the paid inference matrix.
func TestNativeOpenCodeEmptySessionContract(t *testing.T) {
	if os.Getenv("HRX_NATIVE_SESSION_TESTS") != "1" {
		t.Skip("set HRX_NATIVE_SESSION_TESTS=1 for isolated native CLI checks")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Fatal("opencode is required")
	}
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "inference forbidden", http.StatusForbidden)
	}))
	defer server.Close()
	config, _ := json.Marshal(map[string]any{
		"model": "hrx/fixture", "enabled_providers": []string{"hrx"},
		"provider": map[string]any{"hrx": map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "fixture", "options": map[string]string{"baseURL": server.URL + "/v1", "apiKey": "nonsecret-test-placeholder"}, "models": map[string]any{"fixture": map[string]any{"name": "fixture", "variants": map[string]any{"high": map[string]string{"reasoningEffort": "high"}}}}}},
	})
	launch := Launch{Executable: "opencode", Model: "fixture", Reasoning: "high", Env: []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + root,
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_DATA_HOME=" + filepath.Join(root, "data"),
		"XDG_CACHE_HOME=" + filepath.Join(root, "cache"), "XDG_STATE_HOME=" + filepath.Join(root, "state"),
		"OPENCODE_CONFIG_CONTENT=" + string(config),
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	id, err := openCodeNativeSession(ctx, launch, project, "")
	if err != nil {
		t.Fatal("native create failed", err)
	}
	got, err := openCodeNativeSession(ctx, launch, project, id)
	if err != nil || got != id {
		t.Fatal("native get failed", err)
	}
	if _, err := openCodeNativeSession(ctx, launch, project, "ses_hrx_does_not_exist"); err == nil {
		t.Fatal("missing native session accepted")
	}
	if requests.Load() != 0 {
		t.Fatalf("session operation contacted model endpoint %d times", requests.Load())
	}
}

func TestOpenCodeSessionCreateAndResumeContract(t *testing.T) {
	for _, existing := range []string{"", "native-id"} {
		launch, project, marker := fakeOpenCodeSessionAPI(t, nil, "0")
		response, _ := json.Marshal(map[string]any{"data": map[string]any{"id": "native-id", "location": map[string]string{"directory": project}}})
		launch.Env = childEnvironment(launch.Env, map[string]string{"HRX_TEST_RESPONSE": string(response)}, nil)
		id, err := openCodeNativeSession(context.Background(), launch, project, existing)
		if err != nil || id != "native-id" {
			t.Fatalf("session contract: %s %v", id, err)
		}
		data, _ := os.ReadFile(marker)
		args := string(data)
		if !strings.HasPrefix(args, "api\n--standalone\n") {
			t.Fatal("shared server used")
		}
		if existing == "" {
			if !strings.Contains(args, "session.create\n--data\n") || !strings.Contains(args, `"providerID":"hrx"`) || !strings.Contains(args, `"variant":"high"`) {
				t.Fatal("creation arguments lost", args)
			}
		} else if !strings.Contains(args, "session.get\n--param\nsessionID=native-id\n") || strings.Contains(args, "session.create") {
			t.Fatal("resume created session", args)
		}
	}
}

func TestOpenCodeSessionRejectsOversizedValidResponse(t *testing.T) {
	launch, project, _ := fakeOpenCodeSessionAPI(t, nil, "0")
	response, _ := json.Marshal(map[string]any{"data": map[string]any{"id": "native-id", "location": map[string]string{"directory": project}}, "padding": strings.Repeat("x", 70000)})
	launch.Env = childEnvironment(launch.Env, map[string]string{"HRX_TEST_RESPONSE": string(response)}, nil)
	if _, err := openCodeNativeSession(context.Background(), launch, project, "native-id"); err == nil {
		t.Fatal("oversized valid session response bypassed output limit")
	}
}

func TestOpenCodeSessionRejectsMissingWrongOrCrossProject(t *testing.T) {
	for _, response := range []any{
		map[string]any{"data": map[string]string{"id": "other"}},
		map[string]any{"data": map[string]any{"id": "native-id", "location": map[string]string{"directory": "/elsewhere"}}},
		map[string]string{"error": "private-sentinel"},
	} {
		launch, project, _ := fakeOpenCodeSessionAPI(t, response, "0")
		_, err := openCodeNativeSession(context.Background(), launch, project, "native-id")
		if err == nil || strings.Contains(err.Error(), "private-sentinel") {
			t.Fatal("bad response accepted or leaked", err)
		}
	}
	launch, project, _ := fakeOpenCodeSessionAPI(t, map[string]string{"error": "private-sentinel"}, "1")
	if _, err := openCodeNativeSession(context.Background(), launch, project, "native-id"); err == nil || strings.Contains(err.Error(), "private-sentinel") {
		t.Fatal("failed API accepted or leaked", err)
	}
}
