package codexlaunch

import (
	"bytes"
	"encoding/json"
	"errors"
)

// ValidateStartResponse compares explicit native start selections with the
// effective response. Preserving the request is not proof the server applied it.
// Inherited values are left to the same native config environment; this method
// does not certify that the launcher actually supplied that environment.
func (n Native) ValidateStartResponse(raw json.RawMessage) error {
	fields, err := decodeObject(raw)
	if err != nil {
		return err
	}
	mismatch := errors.New("native start selection differs from effective response")
	for key, value := range fields {
		null := bytes.Equal(bytes.TrimSpace(value), []byte("null"))
		switch key {
		case "cwd", "model", "approvalsReviewer", "approvalPolicy", "runtimeWorkspaceRoots", "serviceTier":
			if !n.SameTurnSetting(key, value) {
				return mismatch
			}
		case "modelProvider":
			if null {
				continue
			}
			var v string
			if json.Unmarshal(value, &v) != nil || v != n.ModelProvider {
				return mismatch
			}
		case "sandbox":
			if null {
				continue
			}
			var v string
			if json.Unmarshal(value, &v) != nil || nativeSandboxType[v] == "" || nativeSandboxType[v] != n.Sandbox.Type {
				return mismatch
			}
		case "permissions":
			if null {
				continue
			}
			var v string
			if json.Unmarshal(value, &v) != nil || v == "" || !n.ActivePermissionProfile.Present {
				return mismatch
			}
			profile, err := decodeObject(n.ActivePermissionProfile.Value)
			if err != nil {
				return mismatch
			}
			id, err := nonEmpty(profile, "id")
			if err != nil || id != v {
				return mismatch
			}
		case "threadSource":
			if null {
				continue
			}
			var v string
			if json.Unmarshal(value, &v) != nil || !n.ThreadSource.Present || v != n.ThreadSource.Value {
				return mismatch
			}
		}
	}
	return nil
}
