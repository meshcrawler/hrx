package matrix

import (
	"context"
	"errors"
	"slices"
	"time"
)

type Limits struct {
	MaxCells int
	WallTime time.Duration
	CellTime time.Duration
	// An optional exact-ID selection remains visibly partial in the report.
	CellIDs []string
}

type Outcome struct {
	Status   string `json:"status"`
	Category string `json:"category,omitempty"`
	// Coverage names the checks actually performed, not intended future tests.
	Coverage []string `json:"coverage"`
}

type Result struct {
	CellID string `json:"cell_id"`
	Outcome
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	LatencyMS  int64      `json:"latency_ms,omitempty"`
}

type Report struct {
	Runner     *RunnerEvidence `json:"runner,omitempty"`
	Version    int             `json:"version"`
	Manifest   Manifest        `json:"manifest"`
	Mode       string          `json:"mode"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt time.Time       `json:"finished_at"`
	Partial    bool            `json:"partial"`
	StopReason string          `json:"stop_reason,omitempty"`
	Counts     map[string]int  `json:"counts"`
	Results    []Result        `json:"results"`
}

// RunnerEvidence describes the actual offline runner, independently of the
// caller-supplied versions in its frozen planning manifest.
type RunnerEvidence struct {
	HRXVersion       string `json:"hrx_version"`
	ExecutableSHA256 string `json:"executable_sha256"`
	GoVersion        string `json:"go_version"`
	OS               string `json:"os"`
	Arch             string `json:"arch"`
	VCSRevision      string `json:"vcs_revision,omitempty"`
	VCSModified      *bool  `json:"vcs_modified,omitempty"`
	HarnessMode      string `json:"harness_mode"`
}

// OfflineExecutor must not contact providers or read real credentials. It must
// honor cancellation and reap its subprocesses before returning. Live execution
// intentionally has no path through this interface: spend/request reservation
// and explicit approval need a separate contract before live runs are enabled.
type OfflineExecutor interface {
	ExecuteOffline(context.Context, Cell) Outcome
}

// RunOffline always retains one result for every manifest cell. Budget stops
// and selections leave not-run records; they never shrink the claimed matrix.
func RunOffline(ctx context.Context, manifest Manifest, limits Limits, executor OfflineExecutor) (Report, error) {
	report := Report{Version: 1, Manifest: manifest, Mode: "offline", StartedAt: time.Now().UTC(), Results: make([]Result, len(manifest.Cells))}
	if executor == nil || limits.MaxCells <= 0 || limits.WallTime <= 0 || limits.CellTime <= 0 || manifest.Version != 1 || manifest.SnapshotSHA256 == "" {
		return report, errors.New("offline runner requires a manifest, executor and positive cell/time limits")
	}
	known := map[string]bool{}
	for i, cell := range manifest.Cells {
		if cell.ID == "" || known[cell.ID] {
			return report, errors.New("manifest cell IDs must be nonempty and unique")
		}
		known[cell.ID] = true
		report.Results[i] = Result{CellID: cell.ID, Outcome: Outcome{Status: "not-run", Coverage: []string{}}}
	}
	selected := map[string]bool{}
	for _, id := range limits.CellIDs {
		if !known[id] {
			return report, errors.New("selected cell does not exist in manifest")
		}
		selected[id] = true
	}
	ctx, cancel := context.WithTimeout(ctx, limits.WallTime)
	defer cancel()
	executed := 0
	for i, cell := range manifest.Cells {
		if len(selected) > 0 && !selected[cell.ID] {
			continue
		}
		if ctx.Err() != nil {
			report.StopReason = "cancelled-or-wall-time-limit"
			break
		}
		if executed >= limits.MaxCells {
			report.StopReason = "cell-limit"
			break
		}
		started := time.Now().UTC()
		cellCtx, cellCancel := context.WithTimeout(ctx, limits.CellTime)
		outcome := executor.ExecuteOffline(cellCtx, cell)
		deadline := cellCtx.Err()
		cellCancel()
		finished := time.Now().UTC()
		if deadline != nil {
			outcome = Outcome{Status: "blocked", Category: "timeout-or-cancelled", Coverage: []string{}}
		}
		if !validOutcome(outcome) {
			outcome = Outcome{Status: "fail", Category: "invalid-executor-result", Coverage: []string{}}
		}
		if outcome.Status == "pass" {
			for _, check := range requiredChecks(cell) {
				if !slices.Contains(outcome.Coverage, check) {
					outcome.Status, outcome.Category = "blocked", "incomplete-coverage"
					break
				}
			}
		}
		report.Results[i] = Result{CellID: cell.ID, Outcome: outcome, StartedAt: &started, FinishedAt: &finished, LatencyMS: finished.Sub(started).Milliseconds()}
		executed++
	}
	report.FinishedAt = time.Now().UTC()
	report.Counts = map[string]int{"pass": 0, "fail": 0, "blocked": 0, "unsupported": 0, "not-run": 0}
	for _, result := range report.Results {
		report.Counts[result.Status]++
	}
	report.Partial = report.Counts["not-run"] > 0 || report.Counts["blocked"] > 0
	return report, nil
}

func validOutcome(outcome Outcome) bool {
	switch outcome.Status {
	case "pass", "fail", "blocked", "unsupported":
	default:
		return false
	}
	// Error diagnostics from child processes must not become arbitrary report
	// strings: these fixed categories cannot contain credentials or transcripts.
	switch outcome.Category {
	case "", "assertion", "expected-rejection", "missing-binary", "invalid-fixture", "timeout-or-cancelled", "registry-unavailable", "active-owner", "unsupported-combination", "unexpected-exit", "invalid-executor-result", "incomplete-coverage":
	default:
		return false
	}
	if outcome.Status == "pass" && len(outcome.Coverage) == 0 {
		return false
	}
	if outcome.Status != "pass" && outcome.Category == "" {
		return false
	}
	for _, check := range outcome.Coverage {
		switch check {
		case "argv", "environment", "configuration", "reasoning", "authorization-rejection", "secret-redaction", "identity-isolation", "prompt-injection", "permissions", "session-restoration", "lease-cleanup", "pty-signals", "pty-resize", "pty-job-control", "dry-run":
		default:
			return false
		}
	}
	return true
}

func requiredChecks(cell Cell) []string {
	if cell.Expected == "reject" {
		return []string{"authorization-rejection", "secret-redaction"}
	}
	checks := []string{"argv", "environment", "configuration", "reasoning", "secret-redaction", "permissions"}
	if cell.Communications != "off" {
		checks = append(checks, "identity-isolation", "prompt-injection", "lease-cleanup")
	}
	if cell.Session != "fresh" {
		checks = append(checks, "session-restoration")
	}
	if cell.Execution == "interactive" {
		checks = append(checks, "pty-signals", "pty-resize", "pty-job-control")
	}
	return checks
}
