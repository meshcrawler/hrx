package catalog

import (
	"sort"
	"strings"
	"unicode"
)

// Entry includes nonlaunchable sources: absence from completion must be explainable.
type Entry struct {
	ID         string                `json:"id"`
	Route      string                `json:"route,omitempty"`
	Provider   string                `json:"provider,omitempty"`
	Account    string                `json:"account,omitempty"`
	Upstream   string                `json:"upstream,omitempty"`
	Available  bool                  `json:"available"`
	Authorized bool                  `json:"authorized"`
	Harnesses  map[string]Capability `json:"harnesses"`
	Context    int                   `json:"context,omitempty"`
	Output     int                   `json:"output,omitempty"`
}

func Authorized(a Authorization, route string) bool {
	if !identifier.MatchString(route) {
		return false
	}
	for _, grant := range a.Data {
		if grant.ID == route {
			return true
		}
		// Only namespace wildcards returned by this exact key are accepted.
		prefix := strings.TrimSuffix(grant.ID, "*")
		if (strings.HasSuffix(grant.ID, "/*") || strings.HasSuffix(grant.ID, "-*")) && identifier.MatchString(prefix[:len(prefix)-1]) && strings.HasPrefix(route, prefix) {
			return true
		}
	}
	return false
}

func clean(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, value)
}

func (e Entry) Key() string {
	return strings.Join([]string{e.ID, e.Route, e.Provider, e.Account, e.Upstream}, "\x00")
}

func (e Entry) Status(harness string) string {
	if !e.Available {
		return "unavailable"
	}
	if e.Route == "" {
		return "no-gateway-route"
	}
	if !identifier.MatchString(e.Route) || !identifier.MatchString(e.ID) {
		return "invalid-identifier"
	}
	if !e.Authorized {
		return "not-authorized"
	}
	cap, ok := e.Harnesses[harness]
	if !ok {
		return "compatibility-unverified"
	}
	if !validCapabilityFor(harness, cap) {
		return "invalid-capability"
	}
	return "ready"
}

func inventory(d Dashboard, a Authorization) []Entry {
	bySource := map[string]Entry{}
	for _, m := range d.Models {
		sources := m.Sources
		if len(sources) == 0 {
			sources = []Source{{}}
		}
		for _, source := range sources {
			caps := map[string]Capability{}
			for _, harness := range []string{"codex", "claude", "opencode"} {
				if cap, ok := sourceCapabilities(source)[harness]; ok && validCapabilityFor(harness, cap) {
					caps[harness] = cap
				}
			}
			e := Entry{ID: clean(m.ID), Route: clean(source.Route), Provider: clean(source.Provider), Account: clean(source.Account), Upstream: clean(source.Upstream), Available: m.Available && source.Available, Authorized: Authorized(a, source.Route), Harnesses: caps, Context: source.Details.Context, Output: source.Details.Output}
			if e.Context == 0 {
				e.Context = source.Details.ContextLength
			}
			if e.Output == 0 {
				e.Output = source.Details.MaxCompletion
			}
			if old, ok := bySource[e.Key()]; ok {
				e.Available = e.Available || old.Available
				e.Context = min(e.Context, old.Context)
				e.Output = min(e.Output, old.Output)
			}
			bySource[e.Key()] = e
		}
	}
	entries := make([]Entry, 0, len(bySource))
	for _, e := range bySource {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key() < entries[j].Key() })
	return entries
}
