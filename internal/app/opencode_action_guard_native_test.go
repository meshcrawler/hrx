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
)

// No paid calls: real OpenCode, isolated home, deterministic local model endpoint.
func TestNativeOpenCodeActionGuard(t *testing.T) {
	if os.Getenv("HRX_NATIVE_SESSION_TESTS") != "1" {
		t.Skip("set HRX_NATIVE_SESSION_TESTS=1 for installed OpenCode 2.1.2")
	}
	var mu sync.Mutex
	var choices []string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Stream bool              `json:"stream"`
			Tools  []json.RawMessage `json:"tools"`
			Choice string            `json:"tool_choice"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&b); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		t.Logf("request stream=%v tools=%d choice=%s", b.Stream, len(b.Tools), b.Choice)
		if len(b.Tools) > 0 {
			mu.Lock()
			choices = append(choices, b.Choice)
			mu.Unlock()
		}
		message := map[string]any{"role": "assistant", "content": "done"}
		finish := "stop"
		if b.Choice == "required" {
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"index": 0, "id": "call_guard", "type": "function", "function": map[string]any{"name": "shell", "arguments": `{"command":"printf HRX_NATIVE_GUARD_OK"}`}}}}
			finish = "tool_calls"
		}
		if b.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			raw, _ := json.Marshal(map[string]any{"id": "fixture", "object": "chat.completion.chunk", "created": 1, "model": "fixture", "choices": []any{map[string]any{"index": 0, "delta": message, "finish_reason": nil}}})
			io.WriteString(w, "data: "+string(raw)+"\n\n")
			raw, _ = json.Marshal(map[string]any{"id": "fixture", "object": "chat.completion.chunk", "created": 1, "model": "fixture", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}})
			io.WriteString(w, "data: "+string(raw)+"\n\ndata: [DONE]\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}})
		}
	}))
	defer gateway.Close()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"model": "hrx/fixture", "small_model": "hrx/fixture", "enabled_providers": []string{"hrx"}, "provider": map[string]any{"hrx": map[string]any{"npm": "@ai-sdk/openai-compatible", "options": map[string]string{"baseURL": gateway.URL + "/v1", "apiKey": "fixture"}, "models": map[string]any{"fixture": map[string]string{"name": "fixture"}}}}, "agent": map[string]any{"build": map[string]any{"steps": 4, "permission": map[string]string{"*": "deny", "shell": "allow"}}}}
	data, _ := json.Marshal(config)
	l := Launch{Executable: "opencode", Model: "fixture", Env: childEnvironment(os.Environ(), map[string]string{"HOME": root, "XDG_CONFIG_HOME": filepath.Join(root, "config"), "XDG_DATA_HOME": filepath.Join(root, "data"), "XDG_STATE_HOME": filepath.Join(root, "state"), "XDG_CACHE_HOME": filepath.Join(root, "cache"), "OPENCODE_CONFIG_CONTENT": string(data)}, []string{"OPENCODE_CONFIG", "OPENCODE_CONFIG_DIR", "OPENCODE_SERVER_PASSWORD", "OPENCODE_PASSWORD", "HRX_OPENCODE_ACTION_GUARD"})}
	if err := installOpenCodeActionGuard(&l, root); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	id, err := openCodeNativeSession(ctx, l, project, "")
	if err != nil {
		t.Fatal(err)
	}
	binding, _ := json.Marshal(map[string]any{"version": 1, "identity": "test/native/agent", "session": id, "project": project, "model": "fixture", "states": []string{}, "initial_action": true})
	l.Env = childEnvironment(l.Env, map[string]string{"HRX_OPENCODE_ACTION_GUARD": string(binding)}, nil)
	cmd := exec.CommandContext(ctx, "opencode", "run", "--standalone", "--agent", "build", "--auto", "--session", id, "--model", "hrx/fixture", "Run the authorized native shell probe once.")
	cmd.Dir = project
	cmd.Env = l.Env
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("native guard probe: %v\n%s", err, output)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(choices) < 2 || choices[0] != "required" || choices[len(choices)-1] == "required" {
		t.Fatalf("guard did not enforce then release: %v\n%s", choices, output)
	}
	if !strings.Contains(string(output), "HRX_NATIVE_GUARD_OK") {
		t.Fatalf("no native tool execution evidence: %s", output)
	}
}
