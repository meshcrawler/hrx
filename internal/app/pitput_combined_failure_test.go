package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func combinedFailureHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func combinedFailureState(value string, allowed ...string) string {
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return "other"
}

func combinedFailureError(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "none"
	}
	var failure struct{ CodexErrorInfo json.RawMessage }
	if json.Unmarshal(raw, &failure) != nil {
		return "other"
	}
	var name string
	if json.Unmarshal(failure.CodexErrorInfo, &name) != nil {
		var object map[string]json.RawMessage
		if json.Unmarshal(failure.CodexErrorInfo, &object) != nil || len(object) != 1 {
			return "other"
		}
		for key := range object {
			name = key
		}
	}
	return combinedFailureState(name, "contextWindowExceeded", "sessionBudgetExceeded",
		"usageLimitExceeded", "rateLimitExceeded", "flexUnavailable", "serverOverloaded",
		"cyberPolicy", "misalignmentPolicyViolation", "tooManyDenials", "httpConnectionFailed",
		"responseStreamConnectionFailed", "internalServerError", "unauthorized", "badRequest",
		"threadRollbackFailed", "sandboxError", "responseStreamDisconnected", "responseTooManyFailedAttempts")
}

// Fixed, bounded body-free failure evidence. Never emit native errors/messages,
// command text, output, paths, credentials or unclassified peer-provided labels.
func combinedFailureProjection(raw json.RawMessage, expectedThread string) (map[string]any, error) {
	var native struct {
		Thread struct {
			ID     string
			Status struct{ Type string }
			Turns  []struct {
				ID, Status, ItemsView string
				Error                 json.RawMessage
				Items                 []struct {
					ID, Type, Status, ClientID, Command, AggregatedOutput string
					ExitCode                                              *int
				}
			}
		}
	}
	if len(raw) > 4<<20 || json.Unmarshal(raw, &native) != nil || expectedThread == "" || native.Thread.ID != expectedThread || len(native.Thread.Turns) > 4096 {
		return nil, errors.New("failure projection unavailable")
	}
	turns := []map[string]any{}
	start := max(0, len(native.Thread.Turns)-8)
	for _, turn := range native.Thread.Turns[start:] {
		counts := map[string]int{}
		commands, clients := []map[string]any{}, []string{}
		for _, item := range turn.Items {
			kind := combinedFailureState(item.Type, "userMessage", "agentMessage", "reasoning", "commandExecution")
			counts[kind]++
			if item.Type == "userMessage" && item.ClientID != "" {
				clients = append(clients, combinedFailureHash(item.ClientID))
				clients = clients[max(0, len(clients)-8):]
			}
			if item.Type == "commandExecution" {
				commands = append(commands, map[string]any{
					"item_sha256":        combinedFailureHash(item.ID),
					"status":             combinedFailureState(item.Status, "inProgress", "completed", "failed", "declined"),
					"command_utf8_bytes": len(item.Command), "output_utf8_bytes": len(item.AggregatedOutput),
					"exit_code": item.ExitCode,
				})
				commands = commands[max(0, len(commands)-8):]
			}
		}
		turns = append(turns, map[string]any{"turn_sha256": combinedFailureHash(turn.ID),
			"status":     combinedFailureState(turn.Status, "inProgress", "completed", "interrupted", "failed"),
			"items_full": turn.ItemsView == "full", "error_kind": combinedFailureError(turn.Error),
			"item_counts": counts, "commands": commands, "client_sha256": clients})
	}
	return map[string]any{"status": "observed", "thread_status": combinedFailureState(native.Thread.Status.Type,
		"idle", "active", "notLoaded", "systemError"), "turn_count": len(native.Thread.Turns), "last_turns": turns}, nil
}

func TestCombinedFailureProjectionIsBoundedAndBodyFree(t *testing.T) {
	turns := []any{}
	for range 12 {
		items := []any{map[string]any{"type": "userMessage", "clientId": "private-delivery", "content": "private body"}}
		for range 12 {
			items = append(items, map[string]any{"id": "private-tool-call", "type": "commandExecution",
				"status": "inProgress", "command": "private credential command", "aggregatedOutput": "private output", "exitCode": 7})
		}
		turns = append(turns, map[string]any{"id": "private-turn", "status": "failed", "itemsView": "full", "items": items,
			"error": map[string]any{"message": "private error", "codexErrorInfo": "contextWindowExceeded"}})
	}
	raw, _ := json.Marshal(map[string]any{"thread": map[string]any{"id": "thread", "status": map[string]any{"type": "active"}, "turns": turns}})
	projection, err := combinedFailureProjection(raw, "thread")
	if err != nil {
		t.Fatal(err)
	}
	last := projection["last_turns"].([]map[string]any)
	if len(last) != 8 || len(last[0]["commands"].([]map[string]any)) != 8 || last[0]["error_kind"] != "contextWindowExceeded" {
		t.Fatal("failure evidence missing bound or classification")
	}
	encoded, _ := json.Marshal(projection)
	if strings.Contains(string(encoded), "private") || len(encoded) > 32768 {
		t.Fatal("failure projection exposed content or exceeded bound")
	}
	if _, err := combinedFailureProjection(raw, "foreign"); err == nil {
		t.Fatal("foreign native thread accepted")
	}
	if combinedFailureError(json.RawMessage(`{"message":"private","codexErrorInfo":"private-secret"}`)) != "other" {
		t.Fatal("unclassified native error exposed")
	}
}
