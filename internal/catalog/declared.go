package catalog

import "maps"

// sourceCapabilities supplies the known messages contract for the Teams Opus
// route while gateway discovery lacks its declaration. This is not live-test
// evidence, and does not bypass availability or per-key authorization checks.
// Never replace the route with the unprefixed model: its failover differs.
func sourceCapabilities(source Source) map[string]Capability {
	caps := source.Details.Harnesses
	if _, declared := caps["claude"]; declared {
		return caps
	}
	if source.Route != "claude-teams/claude-opus-5-5" || source.Provider != "anthropic" {
		return caps
	}
	if source.Upstream != "anthropic/claude-opus-5-5" && source.Upstream != "anthropic/claude-teams-group/claude-opus-5-5" {
		return caps
	}
	result := maps.Clone(caps)
	if result == nil {
		result = map[string]Capability{}
	}
	result["claude"] = Capability{WireAPI: "messages", Evidence: "provider-declared"}
	return result
}
