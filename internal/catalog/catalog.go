// Package catalog joins the shared dashboard catalog with live key authorization.
package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"time"
)

const MaxAge = 30 * time.Minute

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,239}$`)

type Capability struct {
	TestedAt         string   `json:"tested_at,omitempty"`
	Evidence         string   `json:"evidence,omitempty"`
	CatalogTemplate  string   `json:"catalog_template,omitempty"`
	WireAPI          string   `json:"wire_api"`
	Reasoning        []string `json:"reasoning_levels"`
	DefaultReasoning string   `json:"default_reasoning"`
}

type Source struct {
	Provider  string `json:"provider"`
	Account   string `json:"account"`
	Upstream  string `json:"raw_model_id"`
	Route     string `json:"gateway_route"`
	Available bool   `json:"available"`
	Details   struct {
		Harnesses     map[string]Capability `json:"harnesses"`
		Context       int                   `json:"max_input_tokens"`
		Output        int                   `json:"max_output_tokens"`
		ContextLength int                   `json:"context_length"`
		MaxCompletion int                   `json:"max_completion_tokens"`
	} `json:"details"`
}

type Identity struct {
	ID        string   `json:"canonical_id"`
	Available bool     `json:"available"`
	Sources   []Source `json:"sources"`
}

type Coverage struct {
	Providers []struct {
		Source string `json:"source"`
		OK     bool   `json:"ok"`
	} `json:"providers"`
}

type Dashboard struct {
	Version     int        `json:"schema_version"`
	GeneratedAt time.Time  `json:"generated_at"`
	Models      []Identity `json:"models"`
	Coverage    Coverage   `json:"coverage"`
}

type Authorization struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

type Model struct {
	ID           string                `json:"id"`
	Route        string                `json:"route"`
	Harnesses    map[string]Capability `json:"harnesses"`
	Context      int                   `json:"context,omitempty"`
	TotalContext int                   `json:"total_context,omitempty"`
	Output       int                   `json:"output,omitempty"`
}

type Snapshot struct {
	Coverage    Coverage  `json:"coverage"`
	Inventory   []Entry   `json:"inventory,omitempty"`
	Version     int       `json:"version"`
	Binding     string    `json:"binding"`
	RefreshedAt time.Time `json:"refreshed_at"`
	GeneratedAt time.Time `json:"generated_at"`
	Models      []Model   `json:"models"`
}

func Fresh(at, now time.Time) bool {
	return !at.IsZero() && !at.After(now.Add(time.Minute)) && now.Sub(at) <= MaxAge
}

func validCapabilityFor(harness string, c Capability) bool {
	if c.Evidence != "" && !slices.Contains([]string{"live-tested", "provider-declared", "gateway-declared"}, c.Evidence) {
		return false
	}
	if c.CatalogTemplate != "" && !identifier.MatchString(c.CatalogTemplate) {
		return false
	}
	wire, ok := map[string]string{"codex": "responses", "claude": "messages", "opencode": "chat_completions"}[harness]
	if !ok || c.WireAPI != wire || (harness == "codex" && len(c.Reasoning) == 0) {
		return false
	}
	if len(c.Reasoning) == 0 {
		return c.DefaultReasoning == ""
	}
	if !slices.Contains(c.Reasoning, c.DefaultReasoning) {
		return false
	}
	seen := map[string]bool{}
	for _, effort := range c.Reasoning {
		if harness == "claude" && !slices.Contains([]string{"low", "medium", "high", "xhigh", "max"}, effort) {
			return false
		}
		if seen[effort] || !slices.Contains([]string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}, effort) {
			return false
		}
		seen[effort] = true
	}
	return true
}

func Join(d Dashboard, a Authorization, binding string, now time.Time) (Snapshot, error) {
	s := Snapshot{Version: 1, Binding: binding, RefreshedAt: now, GeneratedAt: d.GeneratedAt, Models: []Model{}}
	if d.Version != 1 || !Fresh(d.GeneratedAt, now) {
		return s, errors.New("gateway catalog is stale or has an unsupported schema")
	}
	healthy := false
	for _, p := range d.Coverage.Providers {
		if p.Source == "gateway" {
			healthy = p.OK
			break
		}
	}
	if !healthy {
		return s, errors.New("gateway discovery did not succeed; refusing last-known routes")
	}
	s.Inventory = inventory(d, a)
	s.Coverage = d.Coverage
	byRoute := map[string]Model{}
	for _, m := range d.Models {
		if !m.Available || !identifier.MatchString(m.ID) {
			continue
		}
		for _, source := range m.Sources {
			if !source.Available || !Authorized(a, source.Route) {
				continue
			}
			caps := map[string]Capability{}
			for harness, cap := range sourceCapabilities(source) {
				if !slices.Contains([]string{"codex", "claude", "opencode"}, harness) {
					continue
				}
				if !validCapabilityFor(harness, cap) {
					return s, fmt.Errorf("invalid %s capabilities for %s", harness, source.Route)
				}
				caps[harness] = cap
			}
			if len(caps) == 0 {
				continue
			}
			model := Model{ID: m.ID, Route: source.Route, Harnesses: caps, Context: source.Details.Context, Output: source.Details.Output, TotalContext: source.Details.ContextLength}
			if previous, exists := byRoute[source.Route]; exists {
				old, _ := json.Marshal(previous.Harnesses)
				current, _ := json.Marshal(model.Harnesses)
				if previous.ID != model.ID || string(old) != string(current) {
					return s, fmt.Errorf("conflicting identity or capabilities for %s", source.Route)
				}
				// A failover alias may have several backends with different limits.
				// Advertise the smallest known bound; any unknown bound stays unknown.
				model.Context = min(previous.Context, model.Context)
				model.TotalContext = min(previous.TotalContext, model.TotalContext)
				model.Output = min(previous.Output, model.Output)
			}
			byRoute[source.Route] = model
		}
	}
	for _, model := range byRoute {
		s.Models = append(s.Models, model)
	}
	sort.Slice(s.Models, func(i, j int) bool {
		if s.Models[i].ID != s.Models[j].ID {
			return s.Models[i].ID < s.Models[j].ID
		}
		return s.Models[i].Route < s.Models[j].Route
	})
	return s, nil
}

func (s Snapshot) Validate(binding string, now time.Time, allowStale bool) error {
	if s.Version != 1 || s.Binding != binding {
		return errors.New("cache belongs to another configuration or schema; run hrx refresh")
	}
	if !allowStale && (!Fresh(s.RefreshedAt, now) || !Fresh(s.GeneratedAt, now)) {
		return errors.New("cache is stale; run hrx refresh")
	}
	seen := map[string]bool{}
	for _, m := range s.Models {
		if !identifier.MatchString(m.ID) || !identifier.MatchString(m.Route) || seen[m.Route] || len(m.Harnesses) == 0 || m.Context < 0 || m.TotalContext < 0 || m.Output < 0 {
			return errors.New("invalid cache; run hrx refresh")
		}
		for harness, cap := range m.Harnesses {
			if !validCapabilityFor(harness, cap) {
				return errors.New("invalid cache capabilities; run hrx refresh")
			}
		}
		seen[m.Route] = true
	}
	return nil
}

// Resolve prefers a public alias over a provider-specific route, preserving failover.
func (s Snapshot) Resolve(name, harness string) (Model, error) {
	var candidates []Model
	for _, m := range s.Models {
		if _, ok := m.Harnesses[harness]; !ok {
			continue
		}
		if m.Route == name {
			return m, nil
		}
		if m.ID == name {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	if len(candidates) > 1 {
		return Model{}, fmt.Errorf("ambiguous model %q; select an exact route from hrx models", name)
	}
	return Model{}, fmt.Errorf("model %q is unavailable, unauthorized, or lacks %s capabilities; run hrx models", name, harness)
}

func (s Snapshot) Names(harness string) []string {
	set := map[string]bool{}
	for _, m := range s.Models {
		if _, ok := m.Harnesses[harness]; !ok {
			continue
		}
		if resolved, err := s.Resolve(m.ID, harness); err == nil && resolved.Route == m.Route {
			set[m.ID] = true
		} else {
			set[m.Route] = true
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
