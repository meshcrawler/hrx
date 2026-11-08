package codexlaunch

import (
	"encoding/json"
	"errors"
)

var ErrProfileInstructionOrigin = errors.New("cannot establish native developer-instruction precedence")

// ProfileInstructions selects an explicit profile value only when its native
// user-profile layer would win. configRead must be the native config/read result
// for the impending thread's cwd and CLI overrides. It must not come from a
// cached configuration for another directory. Missing profile values inherit;
// an explicitly empty winning value remains present so it can clear base text.
//
// This implements ConfigLayerSource precedence in pinned Codex 83086188. Unknown
// origins fail rather than silently promote profile text above a newer layer.
func ProfileInstructions(profile Optional[string], configRead json.RawMessage) (Optional[string], error) {
	if !profile.Present {
		return Optional[string]{}, nil
	}
	// Reject ambiguous evidence at each object used for the decision.
	root, err := decodeObject(configRead)
	if err != nil {
		return Optional[string]{}, ErrProfileInstructionOrigin
	}
	if _, err := decodeObject(root["config"]); err != nil {
		return Optional[string]{}, ErrProfileInstructionOrigin
	}
	origins, err := decodeObject(root["origins"])
	if err != nil {
		return Optional[string]{}, ErrProfileInstructionOrigin
	}
	if raw, ok := origins["developer_instructions"]; ok {
		metadata, err := decodeObject(raw)
		if err != nil {
			return Optional[string]{}, ErrProfileInstructionOrigin
		}
		if _, err := decodeObject(metadata["name"]); err != nil {
			return Optional[string]{}, ErrProfileInstructionOrigin
		}
	}
	var response struct {
		Config  map[string]json.RawMessage `json:"config"`
		Origins map[string]struct {
			Name struct {
				Type    string  `json:"type"`
				Profile *string `json:"profile"`
			} `json:"name"`
		} `json:"origins"`
	}
	if json.Unmarshal(configRead, &response) != nil || response.Config == nil || response.Origins == nil {
		return Optional[string]{}, ErrProfileInstructionOrigin
	}
	value, present := response.Config["developer_instructions"]
	var text *string
	if present && json.Unmarshal(value, &text) != nil {
		return Optional[string]{}, ErrProfileInstructionOrigin
	}
	origin, hasOrigin := response.Origins["developer_instructions"]
	if !hasOrigin {
		if text != nil {
			return Optional[string]{}, ErrProfileInstructionOrigin
		}
		return profile, nil
	}
	switch origin.Name.Type {
	case "packagedDefaults", "mdm", "system", "enterpriseManaged":
		return profile, nil
	case "user":
		if origin.Name.Profile != nil {
			return Optional[string]{}, ErrProfileInstructionOrigin
		}
		return profile, nil
	case "project", "sessionFlags", "legacyManagedConfigTomlFromFile", "legacyManagedConfigTomlFromMdm":
		return Optional[string]{}, nil
	default:
		return Optional[string]{}, ErrProfileInstructionOrigin
	}
}
