package coveragecontroller

import (
	"encoding/json"
	"errors"
	"github.com/meshcrawler/hrx/internal/codexlaunch"
	"testing"
)

func TestUnchangedSettingsNotificationRetainsAdmissionEvidence(t *testing.T) {
	native, err := codexlaunch.ParseThreadStart([]byte(bootstrapStart))
	if err != nil {
		t.Fatal(err)
	}
	native.CollaborationMode = codexlaunch.Optional[json.RawMessage]{Present: true, Value: json.RawMessage(`{"mode":"default","settings":{"model":"fixture","reasoning_effort":null,"developer_instructions":null}}`)}
	var base map[string]json.RawMessage
	_ = json.Unmarshal([]byte(bootstrapStart), &base)
	delete(base, "thread")
	delete(base, "runtimeWorkspaceRoots")
	base["sandboxPolicy"] = base["sandbox"]
	delete(base, "sandbox")
	base["effort"] = base["reasoningEffort"]
	delete(base, "reasoningEffort")
	base["collaborationMode"] = native.CollaborationMode.Value
	base["activePermissionProfile"] = json.RawMessage(`null`)
	base["disabledPluginIds"] = json.RawMessage(`[]`)
	base["summary"] = json.RawMessage(`null`)
	base["personality"] = json.RawMessage(`null`)
	base["multiAgentMode"] = json.RawMessage(`"explicitRequestOnly"`)
	for _, changed := range []string{"", "model", "approvalPolicy", "collaborationMode", "summary", "activePermissionProfile", "disabledPluginIds", "missing", "extra"} {
		t.Run(changed, func(t *testing.T) {
			fields := make(map[string]json.RawMessage)
			for k, v := range base {
				fields[k] = v
			}
			switch changed {
			case "":
			case "missing":
				delete(fields, "model")
			case "extra":
				fields["unknown"] = json.RawMessage(`null`)
			case "disabledPluginIds":
				fields[changed] = json.RawMessage(`["changed"]`)
			default:
				fields[changed] = json.RawMessage(`"changed"`)
			}
			raw, _ := json.Marshal(map[string]any{"method": "thread/settings/updated", "params": map[string]any{"threadId": "thread", "threadSettings": fields}})
			s, err := newStream(testConnection, "thread", 8, 8192, 4096, 8, 8)
			if err != nil {
				t.Fatal(err)
			}
			s.settings = &native
			err = s.accept(frame{testConnection, 1, notificationFrame, raw})
			if changed == "" {
				if err != nil || s.prefix.consumed != 1 || len(s.candidates) != 0 {
					t.Fatal("unchanged settings did not preserve exact prefix", err)
				}
			} else if !errors.Is(err, errThreadBoundary) {
				t.Fatal("changed or incomplete settings retained authority", err)
			}
		})
	}
}
