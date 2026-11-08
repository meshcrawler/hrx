package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/session"
)

func TestHarnessProcessHelper(t *testing.T) {
	mode := os.Getenv("HRX_TEST_PROCESS_HELPER")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	script := "read value; printf '%s' \"$value\"; printf stderr-ok >&2; exit 37"
	if mode == "signal" || mode == "session" {
		script = "echo $$; exec sleep 30"
	}
	var environment []string
	if mode == "environment" {
		script = `test "$HRX_CHILD_TEST" = expected && test -z "$HRX_REMOVED_TEST"`
		environment = childEnvironment(os.Environ(), map[string]string{"HRX_CHILD_TEST": "expected"}, []string{"HRX_REMOVED_TEST"})
	}
	launch := Launch{Executable: "sh", Args: []string{"-c", script}, Env: environment}
	if mode == "session" {
		launch.pendingSession = &session.Record{NativeID: "fixture-id", Harness: "opencode", Identity: "fixture", Route: "fixture/route", Project: "/project"}
	}
	if err := execHarness(launch); err != nil {
		t.Fatal(err)
	}
	t.Fatal("successful exec returned")
}

func TestHarnessUsesComputedEnvironment(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHarnessProcessHelper$")
	cmd.Env = append(os.Environ(), "HRX_TEST_PROCESS_HELPER=environment", "HRX_REMOVED_TEST=remove-me")
	if err := cmd.Run(); err != nil {
		t.Fatal("computed child environment lost", err)
	}
}

func TestHarnessPreservesStreamsAndExitStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHarnessProcessHelper$")
	cmd.Env = append(os.Environ(), "HRX_TEST_PROCESS_HELPER=exit")
	cmd.Stdin = strings.NewReader("stdin-ok\n")
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 37 {
		t.Fatalf("wrong exit status %v", err)
	}
	if out.String() != "stdin-ok" || errOut.String() != "stderr-ok" {
		t.Fatalf("streams changed: %q %q", out.String(), errOut.String())
	}
}

func TestHarnessPreservesPIDAndSignal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHarnessProcessHelper$")
	cmd.Env = append(os.Environ(), "HRX_TEST_PROCESS_HELPER=signal")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != strconv.Itoa(cmd.Process.Pid) {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatal("launcher did not preserve PID")
	}
	if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("missing signal exit: %v", err)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Fatalf("signal not preserved: %v", exit.Sys())
	}
}
