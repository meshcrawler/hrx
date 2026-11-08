package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/matrix"
)

func TestOfflineMatrixCatalogProcesses(t *testing.T) {
	testOfflineMatrixCatalogProcesses(t, "one-shot")
}

func TestOfflineMatrixCatalogPTY(t *testing.T) {
	testOfflineMatrixCatalogProcesses(t, "interactive")
}

// offlineCatalogInput is the catalog the offline matrix tests run. Different source
// names and routes ensure this is catalog driven, not a fixed fixture-model test
// disguised as an arbitrary-catalog executor.
func offlineCatalogInput() matrix.Input {
	input := matrixAdapterInput()
	for harness, snapshot := range input.Snapshots {
		snapshot.Inventory[0].ID = "other-model"
		snapshot.Inventory[0].Route = "other-account/other-model"
		snapshot.Inventory[0].Account = "other-account"
		input.Snapshots[harness] = snapshot
	}
	return input
}

// TestExportMatrixPTYInventory writes the exact interactive cell IDs each PTY suite
// runs, so the stress job certifies those cells rather than any same-sized set. It
// only runs when HRX_MATRIX_INVENTORY_OUT names an output file.
func TestExportMatrixPTYInventory(t *testing.T) {
	out := os.Getenv("HRX_MATRIX_INVENTORY_OUT")
	if out == "" {
		t.Skip("set HRX_MATRIX_INVENTORY_OUT to export the PTY matrix inventory")
	}
	inventory := map[string][]string{}
	for suite, input := range map[string]matrix.Input{
		"TestMatrixInteractivePTY":    matrixAdapterInput(),
		"TestOfflineMatrixCatalogPTY": offlineCatalogInput(),
	} {
		manifest, err := matrix.Generate(input)
		if err != nil {
			t.Fatal(err)
		}
		for _, cell := range manifest.Cells {
			if cell.Execution == "interactive" {
				inventory[suite] = append(inventory[suite], cell.ID)
			}
		}
	}
	data, _ := json.Marshal(inventory)
	if err := os.WriteFile(out, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func testOfflineMatrixCatalogProcesses(t *testing.T, execution string) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("requires Python stdlib")
	}
	binary := filepath.Join(t.TempDir(), "hrx")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/hrx")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	input := offlineCatalogInput()
	manifest, err := matrix.Generate(input)
	if err != nil {
		t.Fatal(err)
	}
	executor := offlineMatrixExecutor{input: input, executable: binary, python: python}
	count := 0
	for _, cell := range manifest.Cells {
		if cell.Execution != execution {
			continue
		}
		t.Run(cell.ID, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			executor := executor
			executor.diagnostics = func(detail string) { t.Log(detail) }
			result := executor.ExecuteOffline(ctx, cell)
			if result.Status != "pass" {
				t.Fatalf("%s/%s/%s: %+v", cell.Harness, cell.Communications, cell.Session, result)
			}
		})
		count++
	}
	if count != 216 {
		t.Fatal("incomplete catalog test", count)
	}
	path := filepath.Join(t.TempDir(), "input.json")
	data, _ := json.Marshal(input)
	if os.WriteFile(path, data, 0600) != nil {
		t.Fatal("write input")
	}
	cmd := exec.Command(binary, "matrix", "--input", path, "--offline", "--max-cells", "2")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("partial matrix claimed completion")
	}
	var report matrix.Report
	if json.Unmarshal(stdout.Bytes(), &report) != nil || !report.Partial || len(report.Results) != 432 || report.Counts["not-run"] != 430 {
		t.Fatal("CLI omitted cells or did not emit report")
	}
	if strings.Contains(stdout.String(), "offline-key-sentinel") {
		t.Fatal("report exposed fixture credential")
	}
}

func TestOfflineMatrixExplicitNegativeAndMissingPrerequisites(t *testing.T) {
	input := matrixAdapterInput()
	snapshot := input.Snapshots["codex"]
	snapshot.Inventory[0].Authorized = false
	input.Snapshots["codex"] = snapshot
	manifest, _ := matrix.Generate(input)
	executor := offlineMatrixExecutor{input: input}
	for _, cell := range manifest.Cells {
		result := executor.ExecuteOffline(context.Background(), cell)
		if cell.Expected == "reject" {
			if result.Status != "pass" || result.Category != "expected-rejection" {
				t.Fatal("negative case not exercised", result)
			}
		} else if result.Status != "blocked" {
			t.Fatal("missing prerequisites counted as pass", result)
		}
	}
}

// A hung interactive worker must time out inside the PTY driver, before the
// context, so the result names the phase it stalled in instead of losing it.
func TestOfflineMatrixPTYTimeoutReportsPhase(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("requires Python stdlib")
	}
	input := matrixAdapterInput()
	manifest, _ := matrix.Generate(input)
	worker := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(worker, []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, cell := range manifest.Cells {
		if cell.Execution != "interactive" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		var detail string
		executor := offlineMatrixExecutor{input: input, executable: worker, python: python,
			diagnostics: func(text string) { detail = text }}
		started := time.Now()
		outcome := executor.ExecuteOffline(ctx, cell)
		if outcome.Status != "blocked" || outcome.Category != "timeout-or-cancelled" || time.Since(started) > 8*time.Second {
			t.Fatal("hung worker was not bounded", outcome, time.Since(started))
		}
		if ctx.Err() != nil || !strings.Contains(detail, `offline PTY deadline waiting for b'ready'`) {
			t.Fatalf("timeout did not report its phase before cancellation: ctx %v detail %s", ctx.Err(), detail)
		}
		return
	}
	t.Fatal("no interactive cell")
}

func TestOfflineMatrixCancellationStopsWorker(t *testing.T) {
	input := matrixAdapterInput()
	manifest, _ := matrix.Generate(input)
	worker := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(worker, []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, cell := range manifest.Cells {
		if cell.Execution != "one-shot" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		started := time.Now()
		outcome := (offlineMatrixExecutor{input: input, executable: worker, python: "/unused-for-cancelled-worker"}).ExecuteOffline(ctx, cell)
		if outcome.Status != "blocked" || outcome.Category != "timeout-or-cancelled" || time.Since(started) > 5*time.Second {
			t.Fatal("worker did not stop within cancellation bound", outcome)
		}
		return
	}
	t.Fatal("no one-shot cell")
}

func TestOfflineMatrixPTYCancellationReapsWorker(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("requires Python stdlib")
	}
	input := matrixAdapterInput()
	manifest, _ := matrix.Generate(input)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	worker := filepath.Join(dir, "worker")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	if err := os.WriteFile(worker, []byte("#!/bin/sh\nprintf '%s' \"$$\" > "+quote(pidFile)+"\nexec /bin/sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, cell := range manifest.Cells {
		if cell.Execution != "interactive" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan matrix.Outcome, 1)
		go func() {
			done <- (offlineMatrixExecutor{input: input, executable: worker, python: python}).ExecuteOffline(ctx, cell)
		}()
		var pid int
		for pid == 0 && ctx.Err() == nil {
			data, _ := os.ReadFile(pidFile)
			pid, _ = strconv.Atoi(string(data))
			if pid == 0 {
				time.Sleep(10 * time.Millisecond)
			}
		}
		cancel()
		reaped := false
		if pid > 0 {
			defer func() {
				if !reaped {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}()
		}
		select {
		case outcome := <-done:
			if pid <= 0 || outcome.Status != "blocked" || outcome.Category != "timeout-or-cancelled" {
				t.Fatal("PTY cancellation failed", outcome)
			}
			if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
				t.Fatal("PTY worker survived cancellation or was not reaped", err)
			}
			reaped = true
		case <-time.After(6 * time.Second):
			t.Fatal("PTY cancellation did not honor bound")
		}
		return
	}
	t.Fatal("no interactive cell")
}
