package app

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/meshcrawler/hrx/internal/catalog"
)

// GenerateCodexCatalog supplies the harness-specific schema, not a second model
// registry. Only gateway-authorized routes are emitted and their efforts always
// come from the gateway. Templates supply Codex's prompts and tool definitions.
// Routes whose context capacity is unknown are skipped and reported, never
// padded with a fabricated window.
func GenerateCodexCatalog(s Settings, snapshot catalog.Snapshot) (string, []string, error) {
	output, skipped, err := codexCatalogContent(s, snapshot)
	if err != nil {
		return "", nil, err
	}
	encoded, _ := json.Marshal(output)
	hash := sha256.Sum256(encoded)
	path := filepath.Join(filepath.Dir(s.CachePath), fmt.Sprintf("codex-%x.json", hash[:16]))
	if err = catalog.WriteJSON(path, output); err != nil {
		return "", nil, errors.New("cannot write generated Codex model catalog")
	}
	return path, skipped, nil
}

// Doctor uses the exact launch-template validation without writing a catalog.
func codexCatalogContent(s Settings, snapshot catalog.Snapshot) (map[string]any, []string, error) {
	f, err := os.Open(s.TemplatePath)
	if err != nil {
		return nil, nil, errors.New("cannot read Codex model catalog template; configure model_catalog_json in Codex")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, catalog.MaxJSONBytes+1))
	if err != nil || len(data) > catalog.MaxJSONBytes {
		return nil, nil, errors.New("Codex model template is unreadable or too large")
	}
	var root map[string]json.RawMessage
	if err = json.Unmarshal(data, &root); err != nil {
		return nil, nil, errors.New("invalid Codex model template JSON")
	}
	var templates []map[string]json.RawMessage
	if err := json.Unmarshal(root["models"], &templates); err != nil {
		return nil, nil, errors.New("Codex model template must contain models")
	}
	byName := map[string]map[string]json.RawMessage{}
	for _, entry := range templates {
		var slug string
		if json.Unmarshal(entry["slug"], &slug) == nil && slug != "" {
			byName[slug] = entry
		}
	}
	models := make([]map[string]json.RawMessage, 0, len(snapshot.Models))
	skipped := []string{}
	set := func(m map[string]json.RawMessage, key string, value any) { m[key], _ = json.Marshal(value) }
	for i, model := range snapshot.Models {
		cap, ok := model.Harnesses["codex"]
		if !ok {
			continue
		}
		if cap.CatalogTemplate != "" && model.Context <= 0 {
			skipped = append(skipped, model.Route)
			continue
		}
		name := model.ID
		if cap.CatalogTemplate != "" {
			name = cap.CatalogTemplate
		}
		template, ok := byName[name]
		if !ok {
			return nil, nil, fmt.Errorf("no Codex schema template %q for %s; refresh the installed Codex catalog", name, model.ID)
		}
		entry := make(map[string]json.RawMessage, len(template))
		for key, value := range template {
			entry[key] = value
		}
		set(entry, "slug", model.Route)
		set(entry, "display_name", model.ID)
		set(entry, "description", "LiteLLM gateway: "+model.Route)
		set(entry, "visibility", "list")
		set(entry, "supported_in_api", true)
		set(entry, "priority", i)
		set(entry, "default_reasoning_level", cap.DefaultReasoning)
		levels := make([]map[string]string, 0, len(cap.Reasoning))
		for _, level := range cap.Reasoning {
			levels = append(levels, map[string]string{"effort": level, "description": level + " reasoning (gateway capability policy)"})
		}
		set(entry, "supported_reasoning_levels", levels)
		set(entry, "upgrade", nil)
		if cap.CatalogTemplate != "" {
			// Borrow instructions/tools, not another model's limits or vendor features.
			delete(entry, "tool_mode")
			delete(entry, "multi_agent_version")
			context := model.Context
			// Bound prompt history by the route's input allowance, not its
			// larger total context (which also includes generated output).
			if model.TotalContext > 0 {
				context = min(context, model.TotalContext)
			}
			set(entry, "context_window", context)
			set(entry, "max_context_window", context)
			set(entry, "input_modalities", []string{"text"})
			set(entry, "supports_image_detail_original", false)
			set(entry, "supports_search_tool", false)
			set(entry, "support_verbosity", false)
			set(entry, "use_responses_lite", false)
			set(entry, "additional_speed_tiers", []string{})
			set(entry, "service_tiers", []any{})
		}
		models = append(models, entry)
	}
	// Whitelist root fields: never copy arbitrary template metadata into output.
	return map[string]any{"models": models}, skipped, nil
}
