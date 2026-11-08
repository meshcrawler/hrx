package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/monitor"
)

func TestExitCleanupProcessHelper(t *testing.T) {
	if os.Getenv("HRX_EXIT_CLEANUP_TEST") == "" {
		t.Skip("subprocess helper")
	}
	if err := execHarness(Launch{Executable: "codex", Model: "fixture", Identity: "fixture"}); err != nil {
		os.Exit(79)
	}
	os.Exit(80)
}

func TestExitCleanupNormalSignalAndStartupFailure(t *testing.T) {
	for _, mode := range []string{"normal", "signal", "startup-failure"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			script := "#!/bin/sh\necho ready\nread value\nexit 37\n"
			if mode == "signal" {
				script = "#!/bin/sh\necho ready\nexec sleep 60\n"
			}
			if mode == "startup-failure" {
				script = "#!/nonexistent/hrx-fixture-interpreter\n"
			}
			if err := os.WriteFile(filepath.Join(root, "codex"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExitCleanupProcessHelper$")
			cmd.Env = childEnvironment(os.Environ(), map[string]string{"HRX_EXIT_CLEANUP_TEST": mode, "HRX_MONITOR": "on", "PATH": root + ":" + os.Getenv("PATH"), "XDG_STATE_HOME": filepath.Join(root, "state")}, nil)
			cmd.Stdin = strings.NewReader("exit\n")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if mode == "signal" {
				if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
					t.Fatal("harness did not start", err)
				}
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			err = cmd.Wait()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal("expected nonzero fixture exit", err)
			}
			if mode == "normal" && exit.ExitCode() != 37 || mode == "startup-failure" && exit.ExitCode() != 79 || mode == "signal" && !exit.Sys().(syscall.WaitStatus).Signaled() {
				t.Fatal("exit status changed", err)
			}
			files, _ := filepath.Glob(filepath.Join(root, "state/hrx/monitor/*.json"))
			var records []monitor.Record
			for _, path := range files {
				data, _ := os.ReadFile(path)
				var r monitor.Record
				if json.Unmarshal(data, &r) == nil && r.RunID != "" {
					records = append(records, r)
				}
			}
			if len(records) != 1 || records[0].EndedAt == nil || records[0].Status != "exited" {
				t.Fatalf("no explicit deregistration: %+v", records)
			}
		})
	}
}
