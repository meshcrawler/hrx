package catalog

import "testing"

func TestLatestGenerationsNumericAndVariantSafe(t *testing.T) {
	ids := []string{"gpt-5.5-sol", "gpt-5.6-sol", "gpt-5.9-sol", "gpt-5.10-sol", "gpt-6-astra", "gpt-5.5-terra", "gpt-5.5-sol-preview", "gpt-5.5-sol-2026-01-01", "claude-opus-5", "claude-opus-5-5", "claude-sonnet-5", "glm-5p2-flash", "glm-5p3-flash", "glm-5p2", "unknown-1", "unknown-2"}
	hidden := Superseded(ids)
	for _, id := range []string{"gpt-5.5-sol", "gpt-5.6-sol", "gpt-5.9-sol", "claude-opus-5", "glm-5p2-flash"} {
		if !hidden[id] {
			t.Errorf("older generation kept: %s", id)
		}
	}
	for _, id := range []string{"gpt-5.10-sol", "gpt-6-astra", "gpt-5.5-terra", "gpt-5.5-sol-preview", "gpt-5.5-sol-2026-01-01", "claude-opus-5-5", "claude-sonnet-5", "glm-5p3-flash", "glm-5p2", "unknown-1", "unknown-2"} {
		if hidden[id] {
			t.Errorf("distinct variant hidden: %s", id)
		}
	}
}

func TestLatestKeepsAllRoutesForNewestIdentity(t *testing.T) {
	caps := map[string]Capability{"opencode": {WireAPI: "chat_completions"}}
	s := Snapshot{Models: []Model{{ID: "gpt-5.5-sol", Route: "old", Harnesses: caps}, {ID: "gpt-5.6-sol", Route: "a/new", Harnesses: caps}, {ID: "gpt-5.6-sol", Route: "b/new", Harnesses: caps}}}
	filtered := s.Latest("opencode")
	if len(filtered.Models) != 2 || len(s.Models) != 3 {
		t.Fatal("filter lost routes or mutated source")
	}
}
