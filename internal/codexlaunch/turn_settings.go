package codexlaunch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"reflect"
)

// SameTurnSetting accepts only settings the pinned native baseline reported.
// It does not authorize a new configuration. In particular serviceTier:null
// resets the sticky tier, unlike ordinary nullable Option fields which inherit.
func (n Native) SameTurnSetting(key string, raw json.RawMessage) bool {
	null := bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
	var expected string
	switch key {
	case "collaborationMode":
		if null {
			return true
		}
		if !n.CollaborationMode.Present {
			return false
		}
		expected, err := decodeObject(n.CollaborationMode.Value)
		if err != nil {
			return false
		}
		actual, err := decodeObject(raw)
		if err != nil {
			return false
		}
		normalizeBundledDefaultMode(expected)
		normalizeBundledDefaultMode(actual)
		var left, right any
		a, _ := json.Marshal(expected)
		b, _ := json.Marshal(actual)
		return json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && reflect.DeepEqual(left, right)
	case "turnTrigger":
		if null {
			return true
		}
		var value string
		return json.Unmarshal(raw, &value) == nil && value == "user"
	case "model":
		expected = n.Model
	case "cwd":
		if null {
			return true
		}
		var value string
		return json.Unmarshal(raw, &value) == nil && filepath.IsAbs(value) && filepath.Clean(value) == filepath.Clean(n.Cwd)
	case "approvalsReviewer":
		expected = n.ApprovalsReviewer
	case "approvalPolicy":
		if null {
			return true
		}
		v, err := parseApproval(raw)
		return err == nil && reflect.DeepEqual(v, n.Approval)
	case "sandboxPolicy":
		if null {
			return true
		}
		v, err := parseSandbox(raw)
		return err == nil && reflect.DeepEqual(v, n.Sandbox)
	case "runtimeWorkspaceRoots":
		if null {
			return true
		}
		var v []string
		return json.Unmarshal(raw, &v) == nil && v != nil && reflect.DeepEqual(v, n.RuntimeWorkspaceRoots)
	case "effort":
		if null {
			return true
		}
		if !n.ReasoningEffort.Present {
			return false
		}
		expected = n.ReasoningEffort.Value
	case "serviceTier", "serviceTierForTurn":
		if null {
			return key == "serviceTierForTurn" || !n.ServiceTier.Present
		}
		if !n.ServiceTier.Present {
			return false
		}
		var v string
		if json.Unmarshal(raw, &v) != nil {
			return false
		}
		if v == "fast" {
			v = "priority"
		}
		return v == n.ServiceTier.Value
	// These are not reported sufficiently to compare. Only native no-op nulls
	// are accepted; a real override requires explicit new admission.
	case "permissions", "environments", "summary", "personality", "multiAgentMode", "cyberAccessProgram",
		"disabledPluginIds", "parentTurnId", "rootTurnId", "toolOutput",
		"responsesapiClientMetadata", "additionalContext", "outputSchema":
		return null
	default:
		return false
	}
	if null {
		return true
	}
	var value string
	return expected != "" && json.Unmarshal(raw, &value) == nil && value == expected
}

// SameThreadSettings validates a complete pinned settings notification as an
// unchanged baseline, never as permission to mutate or publish new settings.
func (n Native) SameThreadSettings(raw json.RawMessage) bool {
	return n.ThreadSettingsMismatch(raw) == ""
}

// ThreadSettingsMismatch returns a fixed field classification, never its value.
func (n Native) ThreadSettingsMismatch(raw json.RawMessage) string {
	fields, err := decodeObject(raw)
	if err != nil {
		return "object"
	}
	required := []string{"disabledPluginIds", "cwd", "approvalPolicy", "approvalsReviewer", "sandboxPolicy", "activePermissionProfile", "model", "modelProvider", "serviceTier", "effort", "summary", "collaborationMode", "multiAgentMode", "personality"}
	if len(fields) != len(required) {
		return "field-count"
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			return "missing-" + key
		}
	}
	for _, key := range []string{"cwd", "approvalPolicy", "approvalsReviewer", "sandboxPolicy", "model", "serviceTier", "effort", "collaborationMode"} {
		if string(fields[key]) == "null" {
			if key == "serviceTier" && !n.ServiceTier.Present || key == "effort" && !n.ReasoningEffort.Present {
				continue
			}
			return key
		}
		if !n.SameTurnSetting(key, fields[key]) {
			return key
		}
	}
	var provider, mode string
	if json.Unmarshal(fields["modelProvider"], &provider) != nil || provider != n.ModelProvider {
		return "modelProvider"
	}
	if json.Unmarshal(fields["multiAgentMode"], &mode) != nil || mode != "explicitRequestOnly" {
		return "multiAgentMode"
	}
	for _, key := range []string{"summary", "personality"} {
		if string(fields[key]) != "null" {
			return key
		}
	}
	var plugins []string
	if json.Unmarshal(fields["disabledPluginIds"], &plugins) != nil || plugins == nil || !reflect.DeepEqual(plugins, n.DisabledPluginIDs) {
		return "disabledPluginIds"
	}
	expected := json.RawMessage("null")
	if n.ActivePermissionProfile.Present {
		expected = n.ActivePermissionProfile.Value
	}
	var before, after any
	if json.Unmarshal(expected, &before) != nil || json.Unmarshal(fields["activePermissionProfile"], &after) != nil || !reflect.DeepEqual(before, after) {
		return "activePermissionProfile"
	}
	return ""
}

// Pinned Codex 83086188: TurnRequestProcessor::normalize_collaboration_mode
// replaces null mode instructions with models-manager's bundled DEFAULT text.
// Match its source hash, not arbitrary server-provided instructions. This keeps
// same-mode TUI reassertion equivalent while refusing user overrides.
func normalizeBundledDefaultMode(mode map[string]json.RawMessage) {
	var kind string
	if json.Unmarshal(mode["mode"], &kind) != nil || kind != "default" {
		return
	}
	settings, err := decodeObject(mode["settings"])
	if err != nil {
		return
	}
	var instructions string
	if json.Unmarshal(settings["developer_instructions"], &instructions) != nil {
		return
	}
	digest := sha256.Sum256([]byte(instructions))
	// codex-rs/collaboration-mode-templates/templates/default.md, exact bytes.
	if hex.EncodeToString(digest[:]) != "1042cc643eb0147ca1039b19287c7462ceb297502f7f310d9664ac323a12feca" {
		return
	}
	settings["developer_instructions"] = json.RawMessage("null")
	mode["settings"], _ = json.Marshal(settings)
}
