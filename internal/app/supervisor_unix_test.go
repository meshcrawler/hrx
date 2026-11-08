//go:build darwin || linux

package app

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/hub"
)

type recordedLease struct{ path string }

const supervisorResizeHarness = `import os, select, signal, sys
signal.signal(signal.SIGWINCH, lambda *_: os.write(1, b'resized:%dx%d\n' % tuple(os.get_terminal_size())))
print('ready', flush=True)
# Keep interpreter checkpoints available while the driver waits for resize output.
while not select.select([0], [], [], .05)[0]: pass
sys.stdin.readline()
sys.exit(37)
`

func (l recordedLease) renew(context.Context) error {
	if _, err := os.Stat(l.path + ".deny"); err == nil {
		return errYesodLeaseLost
	}
	return nil
}
func (l recordedLease) release() error { return os.WriteFile(l.path, []byte("released"), 0600) }

func TestSupervisorHelper(t *testing.T) {
	mode := os.Getenv("HRX_SUPERVISOR_TEST")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	l := Launch{Executable: "sh", Args: []string{"-c", "read value; printf '%s' \"$value\"; exit 37"}}
	if mode == "signal" {
		l.Args = []string{"-c", "echo ready; exec sleep 30"}
	}
	if mode == "hub-stop" {
		l.hubClient = &hub.Client{}
		l.Args = []string{"-c", "echo $$; kill -STOP $$; exit 37"}
	}
	if mode == "pty" {
		l.Args = []string{"-c", "echo ready; kill -STOP $$; echo resumed; read value; echo received:$value; exit 37"}
	}
	if mode == "resize" {
		l.Executable = "python3"
		l.Args = []string{"-I", "-c", supervisorResizeHarness}
	}
	if mode == "interrupt" {
		l.Executable = "python3"
		l.Args = []string{"-I", "-c", "import signal, time; signal.signal(signal.SIGINT, signal.SIG_DFL); print('ready', flush=True); time.sleep(30)"}
	}
	status, err := superviseHarness(l, recordedLease{os.Getenv("HRX_RELEASE_PATH")})
	if err != nil {
		os.Exit(99)
	}
	if status.Signaled() {
		os.Exit(128 + int(status.Signal()))
	}
	os.Exit(status.ExitStatus())
}

func TestSupervisorPreservesOutputStatusAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSupervisorHelper$")
	cmd.Env = append(os.Environ(), "HRX_SUPERVISOR_TEST=exit", "HRX_RELEASE_PATH="+path)
	cmd.Stdin = strings.NewReader("input-ok\n")
	output, err := cmd.Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 37 || string(output) != "input-ok" {
		t.Fatalf("wrong outcome %q %v", output, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "released" {
		t.Fatal("lease not released", err)
	}
}

func TestSupervisorHubChildStopDoesNotResuspend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSupervisorHelper$")
	cmd.Env = append(os.Environ(), "HRX_SUPERVISOR_TEST=hub-stop", "HRX_RELEASE_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	pid, parseErr := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || parseErr != nil || pid <= 0 {
		t.Fatalf("child PID unavailable: %v %v", err, parseErr)
	}
	defer syscall.Kill(-pid, syscall.SIGKILL)
	err = cmd.Wait()
	var exit *exec.ExitError
	if ctx.Err() != nil || !errors.As(err, &exit) || exit.ExitCode() != 143 {
		t.Fatalf("hub stop did not terminate promptly: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("lease not released", err)
	}
}

func TestSupervisorForwardsTerminationAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSupervisorHelper$")
	cmd.Env = append(os.Environ(), "HRX_SUPERVISOR_TEST=signal", "HRX_RELEASE_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatal("child not ready", err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 143 {
		t.Fatal("termination lost", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("lease not released", err)
	}
}

func TestSupervisorLaunchFailureReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release")
	if _, err := superviseHarness(Launch{Executable: "/nonexistent/hrx-test-harness"}, recordedLease{path}); err == nil {
		t.Fatal("missing executable accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("lease not released", err)
	}
}

func TestSupervisorPTYStopResume(t *testing.T) {
	for _, mode := range []string{"resume", "lost-lease", "interrupt", "resize"} {
		t.Run(mode, func(t *testing.T) { supervisorPTYStopResume(t, mode) })
	}
}

func supervisorPTYStopResume(t *testing.T, mode string) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY driver requires Python stdlib")
	}
	path := filepath.Join(t.TempDir(), "release")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "-c", `
import fcntl, os, pty, select, signal, struct, subprocess, sys, termios, time
pid, master = pty.fork()
if pid == 0:
    # Test runners may inherit ignored interactive signals from automation.
    # A foreground job launched by an interactive shell starts with defaults.
    for sig in (signal.SIGINT, signal.SIGQUIT, signal.SIGTSTP):
        signal.signal(sig, signal.SIG_DFL)
    os.environ['HRX_SUPERVISOR_TEST'] = {'interrupt':'interrupt', 'resize':'resize'}.get(sys.argv[3], 'pty')
    os.environ['HRX_RELEASE_PATH'] = sys.argv[2]
    os.execv(sys.argv[1], [sys.argv[1], '-test.run=^TestSupervisorHelper$'])
deadline = time.monotonic() + 10
data = b''
done = False
def read_until(marker):
    global data
    while marker not in data:
        assert time.monotonic() < deadline, ('terminal timeout', data)
        if select.select([master], [], [], .05)[0]:
            data += os.read(master, 4096)
try:
    read_until(b'ready')
    if sys.argv[3] in ('interrupt', 'resize'):
        assert os.tcgetpgrp(master) != pid, 'harness is not foreground'
        if sys.argv[3] == 'interrupt':
            attrs = termios.tcgetattr(master)
            assert attrs[3] & termios.ISIG, ('ISIG disabled', attrs)
            assert attrs[6][termios.VINTR] == b'\x03', ('unexpected VINTR', attrs)
            os.write(master, b'\x03')
            expected = 130
        else:
            # No input arrives until repeated resize callbacks have completed.
            for round in range(25):
                rows, columns = 41 + round, 113 + round
                fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack('HHHH', rows, columns, 0, 0))
                read_until(b'resized:%dx%d' % (columns, rows))
            os.write(master, b'finish\n')
            expected = 37
        while True:
            found, status = os.waitpid(pid, os.WNOHANG)
            if found: break
            if time.monotonic() >= deadline:
                rows = subprocess.check_output(['ps', '-axo', 'pid,ppid,pgid,state,comm'], text=True)
                raise AssertionError(('exit timeout', '\n'.join(r for r in rows.splitlines() if str(pid) in r.split()[:3])))
            time.sleep(.01)
        done = True
        assert os.WIFEXITED(status) and os.WEXITSTATUS(status) == expected, status
        assert open(sys.argv[2]).read() == 'released', 'missing terminal-event cleanup'
        sys.exit(0)
    while True:
        found, status = os.waitpid(pid, os.WNOHANG | os.WUNTRACED)
        if found:
            assert os.WIFSTOPPED(status), ('supervisor did not stop', status, data)
            break
        if time.monotonic() >= deadline:
            rows = subprocess.check_output(['ps', '-axo', 'pid,ppid,pgid,state,comm'], text=True)
            print('\n'.join(r for r in rows.splitlines() if str(pid) in r.split()[:3]))
            raise AssertionError(('supervisor stop timeout', pid, data))
        time.sleep(.01)
    assert os.tcgetpgrp(master) == pid, 'terminal not reclaimed on stop'
    assert not os.path.exists(sys.argv[2]), 'released while stopped'
    if sys.argv[3] == 'lost-lease':
        open(sys.argv[2] + '.deny', 'w').close()
    os.kill(pid, signal.SIGCONT)
    if sys.argv[3] == 'lost-lease':
        found, status = os.waitpid(pid, 0)
        done = True
        assert os.WIFEXITED(status) and os.WEXITSTATUS(status) == 143, status
        assert open(sys.argv[2]).read() == 'released', 'missing lost-lease cleanup'
        sys.exit(0)
    read_until(b'resumed')
    assert os.tcgetpgrp(master) != pid, 'child did not regain foreground'
    os.write(master, b'pty-input\n')
    read_until(b'received:pty-input')
    found, status = os.waitpid(pid, 0)
    done = True
    assert os.WIFEXITED(status) and os.WEXITSTATUS(status) == 37, status
    assert open(sys.argv[2]).read() == 'released', 'missing cleanup'
finally:
    if not done:
        try: os.kill(pid, signal.SIGKILL)
        except ProcessLookupError: pass
    os.close(master)
`, os.Args[0], path, mode)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("PTY stop/resume: %v\n%s", err, output)
	}
}
