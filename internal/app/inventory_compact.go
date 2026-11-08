package app

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// builtinCatalogLabels covers providers that have no account component.
var builtinCatalogLabels = map[string]string{
	"openai/":    "OAI",
	"anthropic/": "ANT",
	"chatgpt/":   "CGPT",
}

// configuredCatalogLabels reads [catalog.labels] ("provider/account" = "LABEL").
// A missing or unreadable configuration simply yields no custom labels.
func configuredCatalogLabels() map[string]string {
	config, _, err := loadUserConfig()
	if err != nil {
		return nil
	}
	return config.Catalog.Labels
}

func catalogProviderLabel(provider, account string, configured map[string]string) string {
	key := provider + "/" + account
	if label := strings.TrimSpace(configured[key]); label != "" {
		return label
	}
	if label, ok := builtinCatalogLabels[key]; ok {
		return label
	}
	if provider == "" && account == "" {
		return "?"
	}
	// Preserve unknown account names instead of assigning ambiguous initials.
	return provider + "/" + account
}

func sortedCatalogLabels(labels map[string]bool) string {
	values := make([]string, 0, len(labels))
	for label := range labels {
		values = append(values, label)
	}
	sort.Strings(values)
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ",")
}

func renderCompactCatalog(out io.Writer, rows []CatalogRow, harnesses []string) error {
	type modelSummary struct {
		newer     string
		providers map[string]bool
		ready     map[string]map[string]bool
	}
	models := map[string]*modelSummary{}
	legend := map[string]string{}
	configured := configuredCatalogLabels()
	for _, row := range rows {
		model := models[row.ID]
		if model == nil {
			model = &modelSummary{providers: map[string]bool{}, ready: map[string]map[string]bool{}}
			for _, harness := range harnesses {
				model.ready[harness] = map[string]bool{}
			}
			models[row.ID] = model
		}
		label := catalogProviderLabel(row.Provider, row.Account, configured)
		model.newer = row.SupersededBy
		model.providers[label] = true
		name := row.Provider
		if row.Account != "" {
			name += "/" + row.Account
		}
		if name == "" {
			name = "unknown provider/account"
		}
		legend[label] = name
		for _, harness := range harnesses {
			if row.Route != "" && row.Harnesses[harness].Status == "ready" {
				badge := evidenceBadge(row.Harnesses[harness].Verification)
				if row.Harnesses[harness].Cached {
					badge = "cached-" + badge
				}
				model.ready[harness][label+"["+badge+"]"] = true
			}
		}
	}
	ids := make([]string, 0, len(models))
	for id := range models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fmt.Fprintln(out, "SOURCES lists discovered providers, not authorization. Harness cells list ready providers with evidence badges (- = none); --details explains excluded routes.")
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "MODEL\tSOURCES\t"+strings.Join(harnesses, "\t")+"\tNEWER FAMILY MODEL")
	for _, id := range ids {
		model := models[id]
		values := []string{id, sortedCatalogLabels(model.providers)}
		for _, harness := range harnesses {
			values = append(values, sortedCatalogLabels(model.ready[harness]))
		}
		newer := model.newer
		if newer == "" {
			newer = "-"
		}
		values = append(values, newer)
		fmt.Fprintln(w, strings.Join(values, "\t"))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	labels := make([]string, 0, len(legend))
	for label := range legend {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	// Only abbreviations need a legend; fallback labels already show both names.
	for _, label := range labels {
		if label != legend[label] {
			if _, err := fmt.Fprintf(out, "%s = %s\n", label, legend[label]); err != nil {
				return err
			}
		}
	}
	return nil
}
