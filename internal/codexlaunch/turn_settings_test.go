package codexlaunch

import (
	_ "embed"
	"encoding/json"
	"testing"
)

func TestTurnSettingsPreserveBaselineAndNullSemantics(t *testing.T) {
	n := parse(t, response(nil))
	for _, tc := range []struct {
		key, raw string
		want     bool
	}{
		{"model", `"` + n.Model + `"`, true}, {"model", `"different"`, false},
		{"model", `null`, true}, {"cwd", `"relative"`, false},
		{"approvalPolicy", `null`, true}, {"approvalPolicy", `"invalid"`, false},
		{"permissions", `null`, true}, {"permissions", `"unverified-profile"`, false},
		{"collaborationMode", `{"mode":"plan"}`, false},
		{"turnTrigger", `"user"`, true}, {"turnTrigger", `"delegated"`, false},
		{"unknown", `null`, false},
	} {
		if got := n.SameTurnSetting(tc.key, json.RawMessage(tc.raw)); got != tc.want {
			t.Fatal(tc, got)
		}
	}
	n.ServiceTier = Optional[string]{Present: true, Value: "priority"}
	if n.SameTurnSetting("serviceTier", json.RawMessage(`null`)) {
		t.Fatal("null resets an existing sticky tier")
	}
	if !n.SameTurnSetting("serviceTierForTurn", json.RawMessage(`null`)) {
		t.Fatal("null per-turn tier should inherit")
	}
	if !n.SameTurnSetting("serviceTier", json.RawMessage(`"fast"`)) {
		t.Fatal("pinned alias not normalized")
	}
}

func TestTurnOptionalPayloadNullsPreserveNativeDefaults(t *testing.T) {
	n := parse(t, response(nil))
	// Pinned TurnStartParams serializes these Option fields as null. A null
	// carries no extra input, plugin mutation, ancestry, metadata or schema.
	for _, key := range []string{"disabledPluginIds", "turnTrigger", "parentTurnId", "rootTurnId", "toolOutput", "responsesapiClientMetadata", "additionalContext", "outputSchema"} {
		if !n.SameTurnSetting(key, json.RawMessage(`null`)) {
			t.Fatal("native empty option rejected", key)
		}
		for _, raw := range []string{`[]`, `{}`, `"changed"`, `false`} {
			if n.SameTurnSetting(key, json.RawMessage(raw)) {
				t.Fatal("unverified override accepted", key)
			}
		}
	}
}

func TestCollaborationModeMustMatchAdmittedSettings(t *testing.T) {
	n := parse(t, response(nil))
	raw := json.RawMessage(`{"mode":"plan","settings":{"model":"fixture","reasoning_effort":"low","developer_instructions":null}}`)
	if n.SameTurnSetting("collaborationMode", raw) {
		t.Fatal("unreported mode accepted")
	}
	n.CollaborationMode = Optional[json.RawMessage]{Present: true, Value: raw}
	if !n.SameTurnSetting("collaborationMode", raw) {
		t.Fatal("admitted mode rejected")
	}
	for _, candidate := range []string{
		`{"mode":"default","settings":{"model":"fixture","reasoning_effort":"low","developer_instructions":null}}`,
		`{"mode":"plan","settings":{"model":"other","reasoning_effort":"low","developer_instructions":null}}`,
		`{"mode":"plan","settings":{"model":"fixture","reasoning_effort":null,"developer_instructions":null}}`,
		`{"mode":"plan","settings":{"model":"fixture","reasoning_effort":"low","developer_instructions":"override"}}`,
		`{"mode":"plan","settings":{"model":"fixture","reasoning_effort":"low","developer_instructions":null},"extra":true}`,
	} {
		if n.SameTurnSetting("collaborationMode", json.RawMessage(candidate)) {
			t.Fatal("changed mode accepted")
		}
	}
}

// Apache-2.0 fixture from openai/codex, revision 83086188ae0614bd882818ff1aa23f3194962012,
// codex-rs/collaboration-mode-templates/templates/default.md.
//
//go:embed testdata/native_default_mode.txt
var nativeDefaultModeInstructions string

func TestBundledNativeModeNormalizationIsExact(t *testing.T) {
	n := parse(t, response(nil))
	n.CollaborationMode = Optional[json.RawMessage]{Present: true, Value: json.RawMessage(`{"mode":"default","settings":{"model":"fixture","reasoning_effort":null,"developer_instructions":null}}`)}
	for _, changed := range []bool{false, true} {
		instructions := nativeDefaultModeInstructions
		if changed {
			instructions += "\nchanged"
		}
		raw, _ := json.Marshal(map[string]any{"mode": "default", "settings": map[string]any{"model": "fixture", "reasoning_effort": nil, "developer_instructions": instructions}})
		if n.SameTurnSetting("collaborationMode", raw) == changed {
			t.Fatal("incorrect bundled-mode equivalence", changed)
		}
	}
}
