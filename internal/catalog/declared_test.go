package catalog

import "testing"

func TestTeamsOpusDeclaration(t *testing.T) {
	for _, variant := range []string{"ready", "unauthorized", "unavailable", "other-route", "other-provider", "other-upstream", "explicit-invalid"} {
		t.Run(variant, func(t *testing.T) {
			d, a, now := fixture(t)
			d.Models[0].ID = "claude-opus-5-5"
			source := &d.Models[0].Sources[0]
			source.Route = "claude-teams/claude-opus-5-5"
			source.Provider = "anthropic"
			source.Upstream = "anthropic/claude-teams-group/claude-opus-5-5"
			source.Details.Harnesses = nil
			a.Data[0].ID = source.Route
			switch variant {
			case "unauthorized":
				a.Data = nil
			case "unavailable":
				source.Available = false
			case "other-route":
				source.Route = "other/claude-opus-5-5"
				a.Data[0].ID = source.Route
			case "other-provider":
				source.Provider = "other"
			case "other-upstream":
				source.Upstream = "anthropic/other"
			case "explicit-invalid":
				source.Details.Harnesses = map[string]Capability{"claude": {WireAPI: "chat_completions"}}
			}
			s, err := Join(d, a, "binding", now)
			if variant == "explicit-invalid" {
				if err == nil {
					t.Fatal("invalid declaration overridden")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			m, err := s.Resolve(source.Route, "claude")
			if variant != "ready" {
				if err == nil {
					t.Fatal("unsupported route accepted")
				}
				return
			}
			if err != nil || m.Route != source.Route {
				t.Fatalf("route not preserved: %+v %v", m, err)
			}
			if m.Harnesses["claude"].Verification(now) != "not-tested" {
				t.Fatal("fabricated test evidence")
			}
			if len(s.Inventory) != 1 || s.Inventory[0].Status("claude") != "ready" {
				t.Fatal("inventory disagrees")
			}
			if source.Details.Harnesses != nil {
				t.Fatal("source mutated")
			}
		})
	}
}
