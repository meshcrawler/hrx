package matrix

import (
	"context"
	"strings"
	"testing"
	"time"
)

type offlineFunc func(context.Context, Cell) Outcome

func (f offlineFunc) ExecuteOffline(ctx context.Context, cell Cell) Outcome { return f(ctx, cell) }

func TestOfflineRunnerKeepsUnexecutedCells(t *testing.T) {
	m, _ := Generate(fixture())
	calls := 0
	r, err := RunOffline(context.Background(), m, Limits{MaxCells: 2, WallTime: time.Second, CellTime: time.Second}, offlineFunc(func(_ context.Context, cell Cell) Outcome {
		calls++
		return Outcome{Status: "pass", Coverage: requiredChecks(cell)}
	}))
	if err != nil || calls != 2 || r.Counts["pass"] != 2 || r.Counts["not-run"] != len(m.Cells)-2 || !r.Partial || r.StopReason != "cell-limit" {
		t.Fatalf("bad budget report: %+v %v", r.Counts, err)
	}
	if len(r.Results) != len(m.Cells) {
		t.Fatal("runner dropped unexecuted cells")
	}
}

func TestOfflineRunnerCannotClaimCompleteCellFromArgvOnly(t *testing.T) {
	m, _ := Generate(fixture())
	r, err := RunOffline(context.Background(), m, Limits{MaxCells: 1, WallTime: time.Second, CellTime: time.Second}, offlineFunc(func(context.Context, Cell) Outcome { return Outcome{Status: "pass", Coverage: []string{"argv"}} }))
	if err != nil || r.Results[0].Status != "blocked" || r.Results[0].Category != "incomplete-coverage" || !r.Partial {
		t.Fatal("partial assertions misreported as pass", err)
	}
}

func TestOfflineRunnerSelectionAndCancellation(t *testing.T) {
	m, _ := Generate(fixture())
	selected := m.Cells[3].ID
	r, err := RunOffline(context.Background(), m, Limits{MaxCells: 10, WallTime: time.Second, CellTime: 5 * time.Millisecond, CellIDs: []string{selected}}, offlineFunc(func(ctx context.Context, c Cell) Outcome {
		if c.ID != selected {
			t.Error("executed unselected cell")
		}
		<-ctx.Done()
		return Outcome{Status: "pass", Coverage: []string{"argv"}}
	}))
	if err != nil || r.Counts["blocked"] != 1 || r.Counts["pass"] != 0 || !r.Partial {
		t.Fatal("timeout passed or scope misreported", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err = RunOffline(ctx, m, Limits{MaxCells: 1, WallTime: time.Second, CellTime: time.Second}, offlineFunc(func(context.Context, Cell) Outcome { t.Error("executed cancelled run"); return Outcome{} }))
	if err != nil || r.Counts["not-run"] != len(m.Cells) {
		t.Fatal("cancelled report lost cells", err)
	}
}

func TestOfflineRunnerRejectsUnboundedOrUnknownSelection(t *testing.T) {
	m, _ := Generate(fixture())
	f := offlineFunc(func(context.Context, Cell) Outcome { t.Fatal("invalid run executed"); return Outcome{} })
	for _, limits := range []Limits{{}, {MaxCells: 1, WallTime: time.Second, CellTime: time.Second, CellIDs: []string{"unknown"}}} {
		if _, err := RunOffline(context.Background(), m, limits, f); err == nil {
			t.Fatal("invalid run accepted")
		}
	}
}

func TestOfflineRunnerDoesNotRelayArbitraryDiagnostics(t *testing.T) {
	m, _ := Generate(fixture())
	for _, bad := range []Outcome{{Status: "pass"}, {Status: "pass", Coverage: []string{"private-token"}}, {Status: "fail", Category: "private-token"}} {
		r, err := RunOffline(context.Background(), m, Limits{MaxCells: 1, WallTime: time.Second, CellTime: time.Second}, offlineFunc(func(context.Context, Cell) Outcome { return bad }))
		if err != nil || r.Results[0].Category != "invalid-executor-result" || strings.Contains(r.Results[0].Category, "private-token") {
			t.Fatal("unsafe result accepted", err)
		}
	}
}
