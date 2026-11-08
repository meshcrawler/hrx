package codexlaunch

import (
	"encoding/json"
	"testing"
)

func TestStartResponseChecksExplicitNativeSelections(t *testing.T) {
	n := parse(t, response(nil))
	good, _ := json.Marshal(map[string]any{"model": n.Model, "modelProvider": n.ModelProvider, "cwd": n.Cwd})
	if err := n.ValidateStartResponse(good); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"model":"different"}`, `{"modelProvider":"different"}`, `{"cwd":"/different"}`,
		`{"permissions":"named"}`, `{"sandbox":"unknown"}`, `{"threadSource":"user"}`,
	} {
		if err := n.ValidateStartResponse(json.RawMessage(raw)); err == nil {
			t.Fatal("unchecked requested selection", raw)
		}
	}
	n.ActivePermissionProfile = Optional[json.RawMessage]{Present: true, Value: json.RawMessage(`{"id":"named","extends":null}`)}
	if err := n.ValidateStartResponse(json.RawMessage(`{"permissions":"named"}`)); err != nil {
		t.Fatal(err)
	}
	if err := n.ValidateStartResponse(json.RawMessage(`{"permissions":"other"}`)); err == nil {
		t.Fatal("wrong permission profile accepted")
	}
}
