package codexlaunch

import (
	"encoding/json"
	"testing"
)

func TestProfileInstructionNativePrecedence(t *testing.T) {
	for _, kind := range []string{"packagedDefaults", "mdm", "system", "enterpriseManaged", "user", "project", "sessionFlags", "legacyManagedConfigTomlFromFile", "legacyManagedConfigTomlFromMdm", "futureLayer"} {
		for _, value := range []string{"profile guidance", ""} {
			raw, _ := json.Marshal(map[string]any{"config": map[string]any{"developer_instructions": "server guidance"}, "origins": map[string]any{"developer_instructions": map[string]any{"name": map[string]any{"type": kind}}}})
			got, err := ProfileInstructions(Optional[string]{Present: true, Value: value}, raw)
			switch kind {
			case "futureLayer":
				if err == nil {
					t.Fatal("unknown origin accepted")
				}
			case "project", "sessionFlags", "legacyManagedConfigTomlFromFile", "legacyManagedConfigTomlFromMdm":
				if err != nil || got.Present {
					t.Fatal("profile replaced higher priority", kind, got, err)
				}
			default:
				if err != nil || !got.Present || got.Value != value {
					t.Fatal("profile did not win", kind, got, err)
				}
			}
		}
	}
}
func TestProfileInstructionRequiresOriginEvidence(t *testing.T) {
	profile := Optional[string]{Present: true, Value: "profile"}
	for _, raw := range []string{`{"config":{},"config":{},"origins":{}}`, `{"config":{},"origins":{"developer_instructions":{"name":{"type":"project","type":"user"}}}}`, `{}`, `{"config":{},"origins":null}`, `{"config":{"developer_instructions":"base"},"origins":{}}`, `{"config":{"developer_instructions":4},"origins":{}}`, `{"config":{},"origins":{"developer_instructions":{"name":{"type":"user","profile":"different"}}}}`} {
		if _, err := ProfileInstructions(profile, json.RawMessage(raw)); err == nil {
			t.Fatal("accepted incomplete origin", raw)
		}
	}
	for _, raw := range []string{`{"config":{},"origins":{}}`, `{"config":{"developer_instructions":null},"origins":{}}`} {
		got, err := ProfileInstructions(profile, json.RawMessage(raw))
		if err != nil || got != profile {
			t.Fatal("absent base", got, err)
		}
	}
	if got, err := ProfileInstructions(Optional[string]{}, nil); err != nil || got.Present {
		t.Fatal("absent profile changed inheritance", got, err)
	}
}
