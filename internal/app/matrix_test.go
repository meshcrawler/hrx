package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/matrix"
)

func TestMatrixCLIIsOfflineAndRejectsUnsafeInput(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("HOME", t.TempDir())
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"matrix", "--help"}, &out, &out); err != nil || !strings.Contains(out.String(), "Offline only") {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(path, []byte(`{"token":"secret-sentinel"}`), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err := Run(context.Background(), []string{"matrix", "--input", path}, &out, &out)
	if err == nil || strings.Contains(err.Error(), "secret-sentinel") || out.Len() != 0 {
		t.Fatal("invalid input leaked or accepted", err)
	}
}

func TestMatrixCLIExactCellSelection(t *testing.T) {
	t.Setenv("PATH", "")
	input := matrixAdapterInput()
	snapshot := input.Snapshots["codex"]
	snapshot.Inventory[0].Authorized = false
	input.Snapshots["codex"] = snapshot
	manifest, err := matrix.Generate(input)
	if err != nil {
		t.Fatal(err)
	}
	var selected []string
	for _, cell := range manifest.Cells {
		if cell.Expected == "reject" && len(selected) < 2 {
			selected = append(selected, cell.ID)
		}
	}
	if len(selected) != 2 {
		t.Fatal("missing negative fixture cells")
	}
	path := filepath.Join(t.TempDir(), "input.json")
	data, _ := json.Marshal(input)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	base := []string{"--input", path, "--offline", "--max-cells", "2"}
	var out bytes.Buffer
	err = runMatrix(context.Background(), append(base, "--cell", selected[0], "--cell", selected[1], "--cell", selected[0]), &out)
	var partial *exitError
	var report matrix.Report
	if !errors.As(err, &partial) || json.Unmarshal(out.Bytes(), &report) != nil || !report.Partial || report.Counts["pass"] != 2 || report.Counts["not-run"] != len(manifest.Cells)-2 {
		t.Fatalf("selection failed or omitted unselected cells: %v", err)
	}
	if report.Runner == nil || len(report.Runner.ExecutableSHA256) != 64 || report.Runner.HRXVersion != version || report.Runner.HarnessMode != "isolated-fake-processes" || report.Manifest.HRXVersion != input.HRXVersion {
		t.Fatal("missing actual runner evidence or overwritten planning versions")
	}
	for _, result := range report.Results {
		want := "not-run"
		if result.CellID == selected[0] || result.CellID == selected[1] {
			want = "pass"
		}
		if result.Status != want {
			t.Fatal("wrong cell executed", result.CellID)
		}
	}
	for _, args := range [][]string{
		append(base, "--cell", "unknown-cell"),
		append(base, "--cell", ""),
		{"--input", path, "--cell", selected[0]},
	} {
		out.Reset()
		if err := runMatrix(context.Background(), args, &out); err == nil || out.Len() != 0 {
			t.Fatal("invalid selection accepted")
		}
	}
}

func TestMatrixRunnerEvidenceFingerprintsArtifact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture-binary")
	data := []byte("nonsecret executable fixture")
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	evidence, err := matrixRunnerEvidence(path)
	hash := sha256.Sum256(data)
	if err != nil || evidence.ExecutableSHA256 != hex.EncodeToString(hash[:]) || evidence.GoVersion == "" || evidence.OS == "" || evidence.Arch == "" {
		t.Fatal("artifact fingerprint mismatch", err)
	}
	if _, err := matrixRunnerEvidence(filepath.Dir(path)); err == nil {
		t.Fatal("non-file executable accepted")
	}
}

func TestMatrixCLIRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runMatrix(context.Background(), []string{"--input", path}, &bytes.Buffer{}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO accepted")
		}
	case <-time.After(time.Second):
		// Unblock a regressed blocking open so the failed test leaves no goroutine.
		writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			writer.Close()
		}
		t.Fatal("input open blocked on FIFO")
	}
}
