package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/meshcrawler/hrx/internal/catalog"
)

type CatalogAccess struct {
	Authorized       bool     `json:"authorized"`
	Compatibility    string   `json:"compatibility"`
	Verification     string   `json:"verification"`
	TestedAt         string   `json:"tested_at,omitempty"`
	Cached           bool     `json:"stale_cache,omitempty"`
	WireAPI          string   `json:"wire_api,omitempty"`
	Reasoning        []string `json:"reasoning_levels,omitempty"`
	DefaultReasoning string   `json:"default_reasoning,omitempty"`
	Status           string   `json:"status"`
	Evidence         string   `json:"evidence,omitempty"`
}
type CatalogRow struct {
	SupersededBy string                   `json:"superseded_by,omitempty"`
	ID           string                   `json:"id"`
	Route        string                   `json:"route,omitempty"`
	Provider     string                   `json:"provider,omitempty"`
	Account      string                   `json:"account,omitempty"`
	Upstream     string                   `json:"upstream,omitempty"`
	Context      int                      `json:"context,omitempty"`
	Output       int                      `json:"output,omitempty"`
	Harnesses    map[string]CatalogAccess `json:"harnesses"`
}
type CatalogReport struct {
	AuthorizationCheckedAt map[string]time.Time        `json:"authorization_checked_at"`
	Stale                  map[string]bool             `json:"stale"`
	LatestOnly             bool                        `json:"latest_only,omitempty"`
	Coverage               map[string]catalog.Coverage `json:"coverage"`
	SchemaVersion          int                         `json:"schema_version"`
	VerifiedAt             map[string]time.Time        `json:"verified_at"`
	SourceGeneratedAt      map[string]time.Time        `json:"source_generated_at"`
	IdentityCount          int                         `json:"identity_count"`
	RouteCount             int                         `json:"route_count"`
	SourceCount            int                         `json:"source_count"`
	ReadyRoutes            map[string]int              `json:"ready_routes"`
	Models                 []CatalogRow                `json:"models"`
}

func runCatalog(ctx context.Context, args []string, out io.Writer) error {
	harnesses := []string{"codex", "claude", "opencode"}
	refresh, asJSON := false, false
	latest := false
	compact := false
	stale, details := false, false
	search := ""
	outputPath := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--stale":
			stale = true
		case "--details":
			details = true
		case "--compact":
			compact = true
		case "--latest":
			latest = true
		case "--refresh":
			refresh = true
		case "--json":
			asJSON = true
		case "--harness", "--search", "--output":
			flag := args[i]
			i++
			if i == len(args) {
				return errors.New("missing catalog option value")
			}
			if flag == "--search" {
				search = strings.ToLower(args[i])
				continue
			}
			if flag == "--output" {
				outputPath = args[i]
				if outputPath == "" {
					return errors.New("empty output path")
				}
				continue
			}
			if args[i] != "all" {
				if !slices.Contains(harnesses, args[i]) {
					return errors.New("unknown harness")
				}
				harnesses = []string{args[i]}
			}
		case "--help":
			_, err := fmt.Fprintln(out, "hrx catalog [--harness all|codex|claude|opencode] [--compact | --details] [--latest] [--refresh] [--stale] [--json] [--search TEXT] [--output FILE]")
			return err
		default:
			return errors.New("unknown catalog option")
		}
	}
	report := CatalogReport{SchemaVersion: 1, Coverage: map[string]catalog.Coverage{}, VerifiedAt: map[string]time.Time{}, SourceGeneratedAt: map[string]time.Time{}, ReadyRoutes: map[string]int{}}
	report.AuthorizationCheckedAt = map[string]time.Time{}
	report.Stale = map[string]bool{}
	rows := map[string]CatalogRow{}
	for _, harness := range harnesses {
		s, err := loadHarnessSettings(harness)
		if err != nil {
			return fmt.Errorf("%s: %w", harness, err)
		}
		var snapshot catalog.Snapshot
		if refresh {
			snapshot, err = Refresh(ctx, s)
		} else {
			snapshot, err = catalog.Read(s.CachePath)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", harness, err)
		}
		if err = snapshot.Validate(s.Binding, time.Now(), stale); err != nil {
			return fmt.Errorf("%s: %w", harness, err)
		}
		if snapshot.Inventory == nil {
			return errors.New("cache predates the full catalog; run hrx catalog --refresh")
		}
		report.VerifiedAt[harness] = snapshot.RefreshedAt
		report.AuthorizationCheckedAt[harness] = snapshot.RefreshedAt
		report.Stale[harness] = !catalog.Fresh(snapshot.RefreshedAt, time.Now()) || !catalog.Fresh(snapshot.GeneratedAt, time.Now())
		report.SourceGeneratedAt[harness] = snapshot.GeneratedAt
		report.Coverage[harness] = snapshot.Coverage
		ready := map[string]bool{}
		for _, entry := range snapshot.Inventory {
			row, ok := rows[entry.Key()]
			if !ok {
				row = CatalogRow{ID: entry.ID, Route: entry.Route, Provider: entry.Provider, Account: entry.Account, Upstream: entry.Upstream, Context: entry.Context, Output: entry.Output, Harnesses: map[string]CatalogAccess{}}
			}
			status := entry.Status(harness)
			evidence := ""
			if cap, ok := entry.Harnesses[harness]; ok {
				evidence = cap.Evidence
				if evidence == "" {
					evidence = "gateway-declared"
				}
			}
			cap := entry.Harnesses[harness]
			compatibility := "unknown"
			if _, ok := entry.Harnesses[harness]; ok {
				compatibility = "declared"
			}
			row.Harnesses[harness] = CatalogAccess{Authorized: entry.Authorized, Compatibility: compatibility, Verification: cap.Verification(time.Now()), TestedAt: safeTestDate(cap.TestedAt), Cached: report.Stale[harness], Status: status, Evidence: evidence, WireAPI: cap.WireAPI, Reasoning: cap.Reasoning, DefaultReasoning: cap.DefaultReasoning}
			rows[entry.Key()] = row
			if status == "ready" {
				ready[entry.Route] = true
			}
		}
		report.ReadyRoutes[harness] = len(ready)
	}
	ids := []string{}
	for _, row := range rows {
		for _, access := range row.Harnesses {
			if access.Status != "unavailable" && access.Status != "not-in-snapshot" {
				ids = append(ids, row.ID)
				break
			}
		}
	}
	successors := catalog.Successors(ids)
	for key, row := range rows {
		row.SupersededBy = successors[row.ID]
		rows[key] = row
	}
	if latest {
		rows = latestCatalogRows(rows, harnesses)
		report.LatestOnly = true
	}
	readyByHarness := map[string]map[string]bool{}
	for _, h := range harnesses {
		readyByHarness[h] = map[string]bool{}
	}
	identities, routes := map[string]bool{}, map[string]bool{}
	for _, row := range rows {
		if search != "" && !strings.Contains(strings.ToLower(strings.Join([]string{row.ID, row.Route, row.Provider, row.Account, row.Upstream}, " ")), search) {
			continue
		}
		for _, h := range harnesses {
			if _, ok := row.Harnesses[h]; !ok {
				row.Harnesses[h] = CatalogAccess{Status: "not-in-snapshot"}
			}
			if row.Harnesses[h].Status == "ready" {
				readyByHarness[h][row.Route] = true
			}
		}
		report.Models = append(report.Models, row)
		identities[row.ID] = true
		if row.Route != "" {
			routes[row.Route] = true
		}
	}
	sort.Slice(report.Models, func(i, j int) bool {
		a, b := report.Models[i], report.Models[j]
		return strings.Join([]string{a.ID, a.Route, a.Provider, a.Account, a.Upstream}, "\x00") < strings.Join([]string{b.ID, b.Route, b.Provider, b.Account, b.Upstream}, "\x00")
	})
	for _, h := range harnesses {
		report.ReadyRoutes[h] = len(readyByHarness[h])
	}
	report.IdentityCount, report.RouteCount, report.SourceCount = len(identities), len(routes), len(report.Models)
	if outputPath != "" {
		if err := catalog.WriteJSON(outputPath, report); err != nil {
			return errors.New("cannot write catalog export")
		}
		_, err := fmt.Fprintf(out, "Wrote %d identities, %d routes and %d sources to %s\n", report.IdentityCount, report.RouteCount, report.SourceCount, outputPath)
		return err
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	fmt.Fprintf(out, "%d identities; %d gateway routes; %d provider sources\n", report.IdentityCount, report.RouteCount, report.SourceCount)
	for _, h := range harnesses {
		fmt.Fprintf(out, "%s: %d ready routes (authorization checked %s; stale=%t)\n", h, report.ReadyRoutes[h], report.AuthorizationCheckedAt[h].Format(time.RFC3339), report.Stale[h])
	}
	fmt.Fprintln(out, "ready = authorized + declared compatible, NOT a live test or uptime guarantee. Stale rows are historical only.")
	fmt.Fprintln(out, "V = dated live test within 7d; D = declared only; U = undated/invalid test; S = stale test. Use --details for evidence and limits.")
	if latest {
		fmt.Fprintln(out, "Latest uses numeric same-family names, not deprecation; older harness-usable fallbacks remain.")
	}
	if compact && !details {
		return renderCompactCatalog(out, report.Models, harnesses)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	header := "MODEL\tROUTE\tPROVIDER/ACCOUNT\t" + strings.Join(harnesses, "\t") + "\tNEWER FAMILY MODEL"
	if details {
		header += "\tUPSTREAM\tCONTEXT/OUTPUT"
	}
	fmt.Fprintln(w, header)
	seen := map[string]bool{}
	for _, row := range report.Models {
		values := []string{row.ID, row.Route, row.Provider + "/" + row.Account}
		for _, h := range harnesses {
			access := row.Harnesses[h]
			value := accessLabel(access)
			if details {
				value += fmt.Sprintf(" auth=%t compatibility=%s evidence=%s test=%s tested_at=%s wire=%s reasoning=%s default=%s", access.Authorized, access.Compatibility, access.Evidence, access.Verification, access.TestedAt, access.WireAPI, strings.Join(access.Reasoning, ","), access.DefaultReasoning)
			}
			values = append(values, value)
		}
		newer := row.SupersededBy
		if newer == "" {
			newer = "-"
		}
		values = append(values, newer)
		if details {
			values = append(values, row.Upstream, fmt.Sprintf("%d/%d", row.Context, row.Output))
		}
		// Several upstream IDs can describe the same visible row. Keep their
		// evidence in JSON, but print each distinct table row only once.
		line := strings.Join(values, "\t")
		if !seen[line] {
			fmt.Fprintln(w, line)
			seen[line] = true
		}
	}
	return w.Flush()
}

func latestCatalogRows(rows map[string]CatalogRow, harnesses []string) map[string]CatalogRow {
	available := []string{}
	for _, row := range rows {
		for _, access := range row.Harnesses {
			if access.Status != "unavailable" && access.Status != "not-in-snapshot" {
				available = append(available, row.ID)
				break
			}
		}
	}
	hidden := catalog.Superseded(available)
	for _, h := range harnesses {
		ready := []string{}
		for _, row := range rows {
			if row.Harnesses[h].Status == "ready" {
				ready = append(ready, row.ID)
			}
		}
		superseded := catalog.Superseded(ready)
		// Preserve an older generation if it is still this harness's newest
		// usable option, even when a newer generation exists elsewhere.
		for _, id := range ready {
			if !superseded[id] {
				delete(hidden, id)
			}
		}
	}
	filtered := map[string]CatalogRow{}
	for key, row := range rows {
		if !hidden[row.ID] {
			filtered[key] = row
		}
	}
	return filtered
}
