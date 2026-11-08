package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
)

func backendFailureDeadline(milliseconds int64, now time.Time) (time.Time, error) {
	deadline := time.UnixMilli(milliseconds)
	if !deadline.After(now) || deadline.After(now.Add(5*time.Second)) {
		return time.Time{}, errors.New("failure observation deadline rejected")
	}
	return deadline, nil
}

func backendFixedEnum(value any, allowed ...string) string {
	text, _ := value.(string)
	for _, candidate := range allowed {
		if text == candidate {
			return text
		}
	}
	return "other_or_absent"
}

func backendValueHash(value any) string {
	raw, _ := json.Marshal(value)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func backendNativeError(value any) map[string]any {
	present := value != nil && value != ""
	classification := "none"
	if present {
		classification = "unclassified"
		text, _ := value.(string)
		if item, ok := value.(map[string]any); ok {
			text, _ = item["message"].(string)
		}
		// These exact prefixes come from compact_tool_envelope.py, not model
		// prose. Their observation grants no authority and is never success.
		switch {
		case strings.HasPrefix(strings.TrimSpace(text), "BLOCKED: explicitly authorized disposable native fixture required"):
			classification = "fixture_authorization_missing"
		case strings.HasPrefix(strings.TrimSpace(text), "fixture_helper_wrapper_failed"):
			classification = "fixture_helper_wrapper_failed"
		}
	}
	return map[string]any{"present": present, "class": classification, "sha256": backendValueHash(value)}
}

func backendSessionPermissions(raw json.RawMessage) map[string]any {
	var session map[string]any
	_ = json.Unmarshal(raw, &session)
	value, present := session["permissions"]
	rules, valid := value.([]any)
	result := map[string]any{"present": present, "array_shape": valid, "rule_count": len(rules)}
	projected := []map[string]any{}
	for index, raw := range rules {
		if index >= 64 {
			result["truncated"] = true
			break
		}
		item, _ := raw.(map[string]any)
		projected = append(projected, map[string]any{
			"action":            backendFixedEnum(item["action"], "*", "shell", "bash", "read", "write", "edit"),
			"effect":            backendFixedEnum(item["effect"], "allow", "deny", "ask"),
			"resource_wildcard": item["resource"] == "*",
		})
	}
	result["rules"] = projected
	return result
}

func backendNativeTimeline(entries []any, emittedCallHash string) map[string]any {
	tools := []map[string]any{}
	timeline := []map[string]any{}
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		timeline = append(timeline, map[string]any{
			"type":    backendFixedEnum(entry["type"], "user", "assistant", "tool", "idle", "compaction"),
			"status":  backendFixedEnum(entry["status"], "pending", "running", "completed", "interrupted", "failed"),
			"outcome": backendFixedEnum(entry["outcome"], "succeeded", "failed", "interrupted", "cancelled"),
			"error":   backendNativeError(entry["error"]),
		})
	}
	visited, truncated := 0, false
	var inspect func(any)
	inspect = func(value any) {
		visited++
		if visited > 2048 || len(tools) >= 64 {
			truncated = true
			return
		}
		switch node := value.(type) {
		case map[string]any:
			match := false
			for _, key := range []string{"callID", "callId", "toolCallId", "tool_call_id", "id"} {
				if id, ok := node[key].(string); ok && emittedCallHash != "" {
					digest := sha256.Sum256([]byte(id))
					match = match || hex.EncodeToString(digest[:]) == emittedCallHash
				}
			}
			kind := backendFixedEnum(node["type"], "tool", "tool_use", "tool_call", "tool-call", "tool_result")
			if match || kind != "other_or_absent" {
				state, _ := node["state"].(map[string]any)
				status := node["status"]
				if status == nil {
					status = state["status"]
				}
				errorValue := node["error"]
				if errorValue == nil {
					errorValue = state["error"]
				}
				name := node["name"]
				if name == nil {
					name = node["tool"]
				}
				projected := map[string]any{"matches_emitted_call": match, "type": kind,
					"tool":              backendFixedEnum(name, "shell", "bash", "Bash", "read", "write", "edit", "exec_command"),
					"status":            backendFixedEnum(status, "pending", "queued", "running", "waiting", "completed", "error", "failed", "cancelled", "requires_approval", "permission_required"),
					"permission_marker": node["permission"] != nil || state["permission"] != nil,
					"error":             backendNativeError(errorValue), "exit_code": nil}
				if match {
					projected["fixture_output"] = backendCompactEnvelope(backendToolOutputStrings(node), emittedCallHash)
				}
				for _, parent := range []map[string]any{node, state} {
					for _, key := range []string{"exitCode", "exit_code"} {
						if number, ok := parent[key].(float64); ok && number == math.Trunc(number) && number >= -255 && number <= 255 {
							projected["exit_code"] = int(number)
						}
					}
				}
				tools = append(tools, projected)
			}
			for key, child := range node {
				// Traverse structural tool containers only; never inspect bodies,
				// commands, arguments, stdout/stderr or arbitrary error objects.
				switch key {
				case "content", "parts", "state", "tool_calls", "tools", "result", "info":
					inspect(child)
				}
			}
		case []any:
			for _, child := range node {
				inspect(child)
			}
		}
	}
	inspect(entries)
	return map[string]any{"timeline": timeline, "tool_states": tools, "truncated": truncated}
}
