// Package matrix enumerates exhaustive harness tests without executing them.
package matrix

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"

	"github.com/meshcrawler/hrx/internal/catalog"
)

type Input struct {
	HRXVersion      string            `json:"hrx_version"`
	HarnessVersions map[string]string `json:"harness_versions"`
	// Authorization is harness-key-specific; never reuse one key's grants
	// when enumerating another harness's routes.
	Snapshots map[string]catalog.Snapshot `json:"snapshots"`
}

type Cell struct {
	ID              string `json:"id"`
	Harness         string `json:"harness"`
	Identity        string `json:"identity"`
	Route           string `json:"route"`
	Provider        string `json:"provider"`
	Account         string `json:"account"`
	Reasoning       string `json:"reasoning"` // Empty means omit the option.
	Communications  string `json:"communications"`
	Permissions     string `json:"permissions"`
	Session         string `json:"session"`
	Execution       string `json:"execution"`
	Expected        string `json:"expected"`
	Reason          string `json:"reason,omitempty"`
	Status          string `json:"status"`
	BackendCoverage string `json:"backend_coverage"`
}

type Manifest struct {
	Version         int               `json:"version"`
	HRXVersion      string            `json:"hrx_version"`
	HarnessVersions map[string]string `json:"harness_versions"`
	SnapshotSHA256  string            `json:"snapshot_sha256"`
	Profile         string            `json:"profile"`
	Cells           []Cell            `json:"cells"`
}

// Generate keeps provider/account distinctions but collapses repeated inventory
// rows for the same route/source tuple. Conflicting declarations fail closed.
func Generate(input Input) (Manifest, error) {
	result := Manifest{Version: 1, HRXVersion: input.HRXVersion, HarnessVersions: map[string]string{}, Profile: "exhaustive", Cells: []Cell{}}
	if input.HRXVersion == "" {
		return result, errors.New("record the hrx version before generating a matrix")
	}
	encoded, err := json.Marshal(input.Snapshots)
	if err != nil {
		return result, err
	}
	hash := sha256.Sum256(encoded)
	result.SnapshotSHA256 = hex.EncodeToString(hash[:])
	for _, harness := range []string{"codex", "claude", "opencode"} {
		version := input.HarnessVersions[harness]
		snapshot, ok := input.Snapshots[harness]
		if !ok || version == "" || snapshot.Version != 1 || snapshot.GeneratedAt.IsZero() || snapshot.RefreshedAt.IsZero() {
			return Manifest{}, errors.New("matrix requires versioned snapshots and harness versions for all three harnesses")
		}
		result.HarnessVersions[harness] = version
		if snapshot.Inventory == nil {
			return Manifest{}, errors.New("matrix requires full provider/account inventory, not only launchable models")
		}
		seen := map[string]string{}
		for _, entry := range snapshot.Inventory {
			key, _ := json.Marshal([]string{entry.ID, entry.Route, entry.Provider, entry.Account})
			status := entry.Status(harness)
			cap := entry.Harnesses[harness]
			levels := append([]string{""}, cap.Reasoning...)
			sort.Strings(levels)
			levels = unique(levels)
			declaration, _ := json.Marshal(struct {
				Status  string
				Levels  []string
				Default string
			}{status, levels, cap.DefaultReasoning})
			if old, found := seen[string(key)]; found {
				if old != string(declaration) {
					return Manifest{}, errors.New("conflicting inventory declarations for a matrix tuple")
				}
				continue
			}
			seen[string(key)] = string(declaration)
			for _, reasoning := range levels {
				for _, comms := range []string{"off", "mail", "bulletin", "both"} {
					for _, permission := range []string{"default", "yolo"} {
						for _, session := range []string{"fresh", "resume", "continue"} {
							for _, execution := range []string{"interactive", "one-shot"} {
								cell := Cell{Harness: harness, Identity: entry.ID, Route: entry.Route, Provider: entry.Provider, Account: entry.Account, Reasoning: reasoning, Communications: comms, Permissions: permission, Session: session, Execution: execution, Expected: "launch", Status: "not-run", BackendCoverage: "unknown"}
								if status != "ready" {
									cell.Expected, cell.Reason = "reject", status
								}
								// Identity hashes only the axes, not mutable test results.
								axes, _ := json.Marshal([]string{harness, entry.ID, entry.Route, entry.Provider, entry.Account, reasoning, comms, permission, session, execution})
								digest := sha256.Sum256(axes)
								cell.ID = hex.EncodeToString(digest[:])
								result.Cells = append(result.Cells, cell)
							}
						}
					}
				}
			}
		}
	}
	sort.Slice(result.Cells, func(i, j int) bool { return result.Cells[i].ID < result.Cells[j].ID })
	return result, nil
}

func unique(values []string) []string {
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}
