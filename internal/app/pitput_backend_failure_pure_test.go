package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackendFailureDeadlineCannotExtendFiveSecondsOrAcceptExpiredRequest(t *testing.T) {
	now := time.Now()
	deadline, err := backendFailureDeadline(now.Add(4*time.Second).UnixMilli(), now)
	if err != nil || !deadline.After(now) || deadline.After(now.Add(5*time.Second)) {
		t.Fatal("failure observer deadline changed", err)
	}
	for _, candidate := range []time.Time{now.Add(-time.Second), now.Add(6 * time.Second)} {
		if _, err := backendFailureDeadline(candidate.UnixMilli(), now); err == nil {
			t.Fatal("invalid failure deadline accepted")
		}
	}
}

func TestBackendFailureProjectionPreservesBindingAndExportsNoPrivateFields(t *testing.T) {
	project, _ := filepath.EvalSymlinks(t.TempDir())
	id, call := "ses_exact", "call_private_native_id"
	digest := sha256.Sum256([]byte(call))
	callHash := hex.EncodeToString(digest[:])
	paths := []string{}
	read := func(path string, result any) error {
		paths = append(paths, path)
		var value any
		if path == "/api/session/"+id {
			value = map[string]any{"data": map[string]any{"id": id, "location": map[string]any{"directory": project},
				"permissions": []any{map[string]any{"action": "*", "resource": "*", "effect": "allow"},
					map[string]any{"action": "private", "resource": "/private/token", "effect": "private"}}}}
		} else {
			value = map[string]any{"data": []any{map[string]any{"type": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": call, "name": "shell", "state": map[string]any{
					"status": "permission_required", "permission": map[string]any{"request": "private permission"},
					"error": "fixture_helper_wrapper_failed private suffix", "exitCode": 2, "output": "private body"}}}}}}
		}
		raw, _ := json.Marshal(value)
		return json.Unmarshal(raw, result)
	}
	observed, err := backendOpenCodeFailureObservation(id, project, "msg_original", callHash, read)
	if err != nil || len(paths) != 3 {
		t.Fatal("read-only failure projection failed", err)
	}
	raw, _ := json.Marshal(observed)
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), call) {
		t.Fatal("failure projection exported private fields")
	}
	permissions := observed["persisted_permissions"].(map[string]any)["rules"].([]map[string]any)
	if permissions[0]["effect"] != "allow" || permissions[0]["resource_wildcard"] != true || permissions[1]["effect"] != "other_or_absent" {
		t.Fatal("persisted permission projection changed")
	}
	tools := observed["history_native_state"].(map[string]any)["tool_states"].([]map[string]any)
	if len(tools) != 1 || tools[0]["matches_emitted_call"] != true || tools[0]["status"] != "permission_required" || tools[0]["exit_code"] != 2 || tools[0]["error"].(map[string]any)["class"] != "fixture_helper_wrapper_failed" {
		t.Fatal("exact native tool state was lost")
	}
	paths = nil
	if _, err := backendOpenCodeFailureObservation("ses_foreign", project, "msg_original", callHash, read); err == nil || len(paths) != 1 {
		t.Fatal("foreign session reached history")
	}
	paths = nil
	if _, err := backendOpenCodeFailureObservation(id, project, "msg_original", "invalid", read); err == nil || len(paths) != 0 {
		t.Fatal("invalid emitted call identity reached native API")
	}
}

func TestBackendFailureProjectionStopsAfterReadDeadlineWithoutAnotherRequest(t *testing.T) {
	project, _ := filepath.EvalSymlinks(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	_, err := backendOpenCodeFailureObservation("ses_exact", project, "", "", func(path string, result any) error {
		calls++
		if ctx.Err() != nil {
			return context.DeadlineExceeded
		}
		raw, _ := json.Marshal(map[string]any{"data": map[string]any{"id": "ses_exact", "location": map[string]any{"directory": project}}})
		cancel()
		return json.Unmarshal(raw, result)
	})
	if err == nil || calls != 2 {
		t.Fatal("deadline failure continued native reads")
	}
}
