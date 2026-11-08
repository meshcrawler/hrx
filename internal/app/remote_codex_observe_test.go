package app

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCodexObservationRequiresExactBindingAndCompletedCommands(t *testing.T) {
	request := codexObserveRequest{Session: "native", Project: "/workspace"}
	data := map[string]any{"thread": map[string]any{"id": "native", "cwd": "/workspace", "turns": []any{map[string]any{"status": "completed", "items": []any{
		map[string]any{"type": "commandExecution", "status": "completed", "exitCode": 0, "command": "pitput attention read handle"},
		map[string]any{"type": "commandExecution", "status": "completed", "exitCode": 1, "command": "pitput post /b/test"},
		map[string]any{"type": "commandExecution", "status": "inProgress", "exitCode": 0, "command": "pitput attention ack handle"},
		map[string]any{"type": "agentMessage", "status": "completed", "exitCode": 0, "command": "pitput post fake"},
	}}}}}
	raw, _ := json.Marshal(data)
	result, err := codexReadEvidence(raw, request)
	if err != nil || !result.Binding || result.CompletedTurns != 1 || !reflect.DeepEqual(result.Actions, []string{"read"}) {
		t.Fatal("invalid evidence", result, err)
	}
	for _, changed := range []codexObserveRequest{{Session: "different", Project: "/workspace"}, {Session: "native", Project: "/elsewhere"}} {
		result, err := codexReadEvidence(raw, changed)
		if err != nil || result.Binding || len(result.Actions) != 0 {
			t.Fatal("accepted mismatched binding")
		}
	}
}
