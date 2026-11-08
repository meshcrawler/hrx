package coveragecontroller

import (
	"context"
	"encoding/json"

	"github.com/meshcrawler/hrx/internal/codexlaunch"
)

// This read uses the same bootstrap owner and bounded exchange as every other
// native call. Its response fills the consumed prefix before thread creation.
func (h *gateway) initializedProfileStart(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	if !h.profileDeveloperInstructions.Present {
		return initializedStart(params, h.initialDeveloperInstructions, h.router.maxFrame)
	}
	if _, err := preservedStart(params, h.router.maxFrame); err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(params, &fields) != nil {
		return nil, errBootstrap
	}
	// Explicit per-thread config overrides outrank the selected user profile.
	if raw, ok := fields["config"]; ok {
		var config map[string]json.RawMessage
		if json.Unmarshal(raw, &config) != nil {
			return nil, errBootstrap
		}
		if _, explicit := config["developer_instructions"]; explicit {
			return initializedStart(params, h.initialDeveloperInstructions, h.router.maxFrame)
		}
	}
	read := map[string]json.RawMessage{}
	if cwd, ok := fields["cwd"]; ok {
		read["cwd"] = cwd
	}
	request, err := json.Marshal(read)
	if err != nil {
		return nil, errBootstrap
	}
	response, err := h.bootstrapCall(ctx, "config/read", request)
	if err != nil {
		return nil, err
	}
	selected, err := codexlaunch.ProfileInstructions(h.profileDeveloperInstructions, response.Result)
	if err != nil {
		return nil, err
	}
	instructions := h.initialDeveloperInstructions
	if selected.Present && selected.Value != "" {
		if instructions != "" {
			instructions += "\n\n"
		}
		instructions += selected.Value
	}
	return initializedStartValue(params, instructions, selected.Present || instructions != "", h.router.maxFrame)
}
