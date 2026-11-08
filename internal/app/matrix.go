package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/meshcrawler/hrx/internal/matrix"
)

const matrixHelp = `hrx matrix --input FILE
hrx matrix --input FILE --offline --max-cells N [--cell ID ...] [--wall-time 5m] [--cell-time 15s]
Emit an exhaustive JSON test manifest from a frozen matrix input.
Input contains hrx_version, harness_versions, and snapshots keyed by codex,
claude, and opencode. Snapshots must retain full provider/account inventory.
Offline only: no refresh, auth helpers, Yesod registration, or inference.
Without --offline, all generated cells are not-run.
Offline execution uses isolated fake processes and requires Python 3.
Interactive cells run under a PTY; one-shot cells preserve output and exit status.
Every manifest cell remains in the JSON report, including budget skips.
Repeat --cell to select exact manifest IDs; unselected cells remain not-run.
Exit 1 means failed, blocked or not-run cells remain. No live mode exists.
`

func runMatrix(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 1 && args[0] == "--help" {
		_, err := fmt.Fprint(out, matrixHelp)
		return err
	}
	flags := flag.NewFlagSet("matrix", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("input", "", "")
	offline := flags.Bool("offline", false, "")
	maxCells := flags.Int("max-cells", 0, "")
	wallTime := flags.Duration("wall-time", 5*time.Minute, "")
	cellTime := flags.Duration("cell-time", 15*time.Second, "")
	var cellIDs []string
	flags.Func("cell", "exact manifest cell ID (repeatable)", func(id string) error {
		if id == "" {
			return errors.New("cell ID must not be empty")
		}
		cellIDs = append(cellIDs, id)
		return nil
	})
	if flags.Parse(args) != nil || flags.NArg() != 0 || *path == "" || (*offline && (*maxCells <= 0 || *wallTime <= 0 || *cellTime <= 0)) {
		return errors.New("invalid matrix options; run hrx matrix --help")
	}
	if !*offline && (len(cellIDs) > 0 || *maxCells != 0 || *wallTime != 5*time.Minute || *cellTime != 15*time.Second) {
		return errors.New("matrix execution limits require --offline")
	}
	// A FIFO must not block before the regular-file check can reject it.
	f, err := os.OpenFile(*path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("cannot open matrix input")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16*1024*1024 {
		return errors.New("matrix input must be a regular file of at most 16 MiB")
	}
	var input matrix.Input
	decoder := json.NewDecoder(io.LimitReader(f, 16*1024*1024+1))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&input) != nil || decoder.Decode(&extra) != io.EOF {
		return errors.New("invalid matrix input JSON")
	}
	manifest, err := matrix.Generate(input)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if *offline {
		executable, err := os.Executable()
		if err != nil {
			return errors.New("cannot resolve offline worker executable")
		}
		runner, err := matrixRunnerEvidence(executable)
		if err != nil {
			return err
		}
		python, _ := exec.LookPath("python3")
		report, err := matrix.RunOffline(ctx, manifest, matrix.Limits{MaxCells: *maxCells, WallTime: *wallTime, CellTime: *cellTime, CellIDs: cellIDs}, offlineMatrixExecutor{input: input, executable: executable, python: python})
		if err != nil {
			return err
		}
		report.Runner = &runner
		if err = encoder.Encode(report); err != nil {
			return err
		}
		if report.Partial || report.Counts["fail"] > 0 {
			return &exitError{1, "offline matrix has failed or unfinished cells"}
		}
		return nil
	}
	return encoder.Encode(manifest)
}

func matrixRunnerEvidence(executable string) (matrix.RunnerEvidence, error) {
	evidence := matrix.RunnerEvidence{HRXVersion: version, GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, HarnessMode: "isolated-fake-processes"}
	f, err := os.OpenFile(executable, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return evidence, errors.New("cannot fingerprint offline runner executable")
	}
	defer f.Close()
	info, err := f.Stat()
	const maxBinarySize = 1024 * 1024 * 1024
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBinarySize {
		return evidence, errors.New("invalid offline runner executable")
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(f, maxBinarySize+1))
	if err != nil || count != info.Size() {
		return evidence, errors.New("cannot fingerprint offline runner executable")
	}
	evidence.ExecutableSHA256 = hex.EncodeToString(hash.Sum(nil))
	if build, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				evidence.VCSRevision = setting.Value
			case "vcs.modified":
				modified := setting.Value == "true"
				evidence.VCSModified = &modified
			}
		}
	}
	return evidence, nil
}
