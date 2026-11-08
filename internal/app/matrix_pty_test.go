//go:build darwin || linux

package app

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/matrix"
)

const matrixPTYHarness = `import json, os, select, signal, sys
directory = os.environ['HRX_MATRIX_DIR']
assert sys.argv[1:] == json.load(open(directory + '/expected-argv.json'))
if os.environ['HRX_MATRIX_COMMS'] != 'off':
    for key in ('YESOD_AGENT_IDENTITY', 'YESOD_MAIL_IDENTITY', 'YESOD_MAIL_SELF'):
        assert os.environ[key] == 'fixture/hrx/matrix'
with open(directory + '/events', 'a') as f: f.write('launch\n')
signal.signal(signal.SIGINT, signal.SIG_DFL)
signal.signal(signal.SIGTSTP, signal.SIG_DFL)
# Unbuffered writes: a print can re-enter a stdout flush interrupted by the stop.
signal.signal(signal.SIGCONT, lambda *_: os.write(1, b'continued\n'))
signal.signal(signal.SIGWINCH, lambda *_: os.write(1, b'size:%dx%d\n' % tuple(os.get_terminal_size())))
assert os.isatty(0) and os.isatty(1) and os.isatty(2)
print('ready', flush=True)
# Poll before reading: a blocking read has no interpreter checkpoint, so a SIGCONT
# landing just before it would defer the 'continued' handler until input.
while not select.select([0], [], [], .05)[0]: pass
assert sys.stdin.readline().strip() == 'input-sentinel'
print('input-confirmed', flush=True)
while True: signal.pause()
`

const matrixPTYDriver = `import errno, fcntl, os, pty, select, signal, struct, subprocess, sys, termios, time
pid, terminal = pty.fork()
if pid == 0:
    for sig in (signal.SIGINT, signal.SIGQUIT, signal.SIGTSTP):
        signal.signal(sig, signal.SIG_DFL)
    os.execv(sys.argv[1], [sys.argv[1], '-test.run=^TestMatrixProcessHelper$'])
deadline = time.monotonic() + 10
data = b''
done = False
group = None
def read_terminal():
    # Linux reports a PTY whose child side has closed as EIO rather than EOF.
    try:
        return os.read(terminal, 4096)
    except OSError as error:
        if error.errno != errno.EIO:
            raise
        return b''
def child_status():
    try:
        found, status = os.waitpid(pid, os.WNOHANG)
    except ChildProcessError:
        return 'reaped'
    return 'running' if not found else 'status %d' % status
def processes():
    # Diagnose a stall: job-control state of this launch's process tree and the terminal owner.
    try:
        rows = subprocess.run(['ps', '-A', '-o', 'pid=,ppid=,pgid=,stat=,wchan=,comm='], capture_output=True, text=True, timeout=5).stdout.splitlines()
    except (OSError, subprocess.SubprocessError):
        return 'ps unavailable'
    tree, chosen = {pid}, []
    for _ in range(4):
        for row in rows:
            fields = row.split()
            if len(fields) >= 2 and fields[1].isdigit() and int(fields[1]) in tree:
                tree.add(int(fields[0]))
    for row in rows:
        fields = row.split()
        if fields and fields[0].isdigit() and int(fields[0]) in tree:
            chosen.append(' '.join(fields))
    try:
        owner = os.tcgetpgrp(terminal)
    except OSError:
        owner = None
    # Signal state separates "SIGCONT never sent" from "delivered but not handled".
    signals = {}
    for row in chosen:
        process = row.split()[0]
        try:
            with open('/proc/%s/status' % process) as source:
                signals[process] = {key: value.strip() for key, _, value in (line.partition(':') for line in source)
                                    if key in ('State', 'SigPnd', 'ShdPnd', 'SigBlk', 'SigIgn', 'SigCgt')}
        except OSError:
            signals[process] = 'unavailable'
    try:
        with open(os.environ['HRX_SUPERVISOR_TRACE']) as source:
            trace = source.read()[-4000:]
    except OSError:
        trace = 'unavailable'
    return {'terminal_pgrp': owner, 'tree': chosen, 'signals': signals, 'supervisor_trace': trace}
def until(marker):
    global data
    while marker not in data:
        if time.monotonic() >= deadline:
            evidence = processes()
            # Diagnostic only, after capturing state: a newline lets a harness blocked
            # in read() return. If 'continued' appears only now, its SIGCONT handler
            # was deferred by the interpreter, not lost by the supervisor.
            after = b''
            try:
                os.write(terminal, b'\n')
                probe_until = time.monotonic() + 1
                while time.monotonic() < probe_until:
                    if select.select([terminal], [], [], .05)[0]:
                        chunk = read_terminal()
                        if not chunk:
                            break
                        after += chunk
            except OSError:
                pass
            raise AssertionError(('terminal deadline', marker, data[-1000:], evidence, ('after newline', after[-1000:])))
        if select.select([terminal], [], [], .02)[0]:
            chunk = read_terminal()
            assert chunk, ('terminal closed', marker, child_status(), data[-1000:])
            data += chunk
            assert len(data) < 65536, 'output limit'
def wait(stopped=False):
    while True:
        found, status = os.waitpid(pid, os.WNOHANG | (os.WUNTRACED if stopped else 0))
        if found: return status
        assert time.monotonic() < deadline, 'process deadline'
        if not stopped and select.select([terminal], [], [], 0)[0]:
            try: os.read(terminal, 4096)
            except OSError: pass
        time.sleep(.01)
try:
    until(b'ready')
    group = os.tcgetpgrp(terminal)
    comms = os.environ['HRX_MATRIX_COMMS'] != 'off'
    assert group != pid, 'tracked launches must supervise foreground ownership'
    os.write(terminal, b'\x1a')
    assert os.WIFSTOPPED(wait(True)), 'did not preserve stop'
    assert os.tcgetpgrp(terminal) == pid, 'did not reclaim terminal'
    assert 'release' not in open(os.environ['HRX_MATRIX_DIR'] + '/events').read()
    os.kill(pid, signal.SIGCONT)
    until(b'continued')
    assert os.tcgetpgrp(terminal) == group, 'did not restore foreground'
    fcntl.ioctl(terminal, termios.TIOCSWINSZ, struct.pack('HHHH', 41, 113, 0, 0))
    until(b'size:113x41')
    os.write(terminal, b'input-sentinel\n')
    until(b'input-confirmed')
    os.write(terminal, b'\x03')
    status = wait()
    done = True
    assert os.WIFSIGNALED(status) and os.WTERMSIG(status) == signal.SIGINT, 'interrupt status changed'
    expected = 'allocate\nclaim\nlaunch\nrenew\nrelease\n' if comms else 'launch\n'
    assert open(os.environ['HRX_MATRIX_DIR'] + '/events').read() == expected, 'lease lifecycle mismatch'
    print('pty-matrix-ok')
finally:
    os.close(terminal)
    if not done:
        for target in set([pid, -(group or pid)]):
            try: os.kill(target, signal.SIGKILL)
            except (ProcessLookupError, PermissionError): pass
        cleanup_deadline = time.monotonic() + 1
        while time.monotonic() < cleanup_deadline:
            try:
                if os.waitpid(pid, os.WNOHANG)[0]: break
            except ChildProcessError: break
            time.sleep(.01)
`

func TestMatrixInteractivePTY(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY driver requires Python stdlib")
	}
	manifest, err := matrix.Generate(matrixAdapterInput())
	if err != nil {
		t.Fatal(err)
	}
	executed := 0
	for _, cell := range manifest.Cells {
		if cell.Execution != "interactive" {
			continue
		}
		t.Run(cell.ID, func(t *testing.T) {
			t.Log(cell.Harness, cell.Communications, cell.Session, cell.Permissions)
			dir := t.TempDir()
			ps, err := exec.LookPath("ps")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(ps, filepath.Join(dir, "ps")); err != nil {
				t.Fatal(err)
			}
			// Use the known interpreter by absolute path; PATH only exposes fakes.
			quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
			harness := "#!/bin/sh\nexec " + quote(python) + " -I -c " + quote(matrixPTYHarness) + " \"$@\"\n"
			for name, script := range map[string]string{
				cell.Harness: harness,
				"python3":    "#!/bin/sh\nprintf 'allocate\\n' >> \"$HRX_MATRIX_DIR/events\"\nprintf '%s' '{\"agent\":\"fixture/hrx/matrix\"}'\n",
				"yesod":      "#!/bin/sh\nread -r token\n[ ${#token} = 64 ] || exit 9\nprintf '%s\\n' \"$2\" >> \"$HRX_MATRIX_DIR/events\"\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			encoded, _ := json.Marshal(cell)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, python, "-I", "-c", matrixPTYDriver, os.Args[0])
			cmd.WaitDelay = time.Second
			cmd.Env = []string{"PATH=" + dir, "HOME=" + dir, "XDG_STATE_HOME=" + filepath.Join(dir, "state"), "GORACE=atexit_sleep_ms=0",
				"HRX_MATRIX_DIR=" + dir, "HRX_MATRIX_CELL=" + string(encoded), "HRX_MATRIX_COMMS=" + cell.Communications,
				"HRX_SUPERVISOR_TRACE=" + filepath.Join(dir, "supervisor.trace"),
				"YESOD_AGENT_IDENTITY=parent/agent/old", "YESOD_MAIL_IDENTITY=parent/agent/old", "YESOD_MAIL_SELF=parent/agent/old"}
			output, err := cmd.CombinedOutput()
			if err != nil || string(output) != "pty-matrix-ok\n" {
				t.Fatalf("PTY fixture failed: %v; %s", err, output)
			}
			// The diagnostic must stay wired: a silent trace would hide the next stall.
			trace, _ := os.ReadFile(filepath.Join(dir, "supervisor.trace"))
			for _, step := range []string{"child stopped", "self SIGSTOP", "SIGCONT received", "forward continued"} {
				if !strings.Contains(string(trace), step) {
					t.Fatalf("supervisor trace lacks %q: %s", step, trace)
				}
			}
		})
		executed++
	}
	if executed != 216 {
		t.Fatalf("incomplete interactive matrix: %d", executed)
	}
}

// Regression for Linux CI EIO failures: a fixture signal handler that prints re-enters
// the stdout flush interrupted by the job-control stop, raises RuntimeError and closes
// the PTY, which Linux reports to the driver as EIO.
func TestMatrixFixtureSignalHandlersAreReentrantSafe(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("fixture handlers require Python stdlib")
	}
	for name, source := range map[string]string{"pty harness": matrixPTYHarness, "offline probe": matrixProbe} {
		var resume string
		for _, line := range strings.Split(source, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "signal.signal(signal.SIGCONT") || strings.HasPrefix(line, "signal.signal(signal.SIGWINCH") {
				if strings.Contains(line, "print(") {
					t.Fatalf("%s: signal handler uses buffered print: %s", name, line)
				}
				if strings.Contains(line, "SIGCONT") {
					resume = line
				}
			}
		}
		if resume == "" {
			t.Fatalf("%s: SIGCONT handler not found", name)
		}
		// Block the main thread inside a buffered stdout write, then deliver SIGCONT.
		script := "import os, signal, sys\n" + resume + "\nfor _ in range(64): print('x' * 65536, flush=True)\n"
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		t.Cleanup(cancel)
		cmd := exec.CommandContext(ctx, python, "-I", "-c", script)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
		for range 20 {
			cmd.Process.Signal(syscall.SIGCONT)
			time.Sleep(5 * time.Millisecond)
		}
		data, _ := io.ReadAll(stdout)
		if err := cmd.Wait(); err != nil || !strings.Contains(string(data), "continued") {
			t.Fatalf("%s: handler broke a blocked write: %v %s", name, err, stderr.String())
		}
	}
}

// matrixSignalDriver sends SIGWINCH (via resize) and SIGCONT to an idle fixture harness
// that has no input ready, and requires each handler's output well within a cell deadline.
const matrixSignalDriver = `import fcntl, os, pty, select, signal, struct, sys, termios, time
pid, terminal = pty.fork()
if pid == 0:
    os.execv(sys.executable, [sys.executable, '-I', '-c', sys.argv[1]])
data = b''
def until(marker, limit):
    global data
    deadline = time.monotonic() + limit
    while marker not in data:
        if time.monotonic() >= deadline:
            raise AssertionError(('handler deferred without input', marker, data[-300:]))
        if select.select([terminal], [], [], .02)[0]:
            data += os.read(terminal, 4096)
try:
    until(b'ready', 10)
    for round in range(int(sys.argv[2])):
        rows, columns = 20 + round % 7, 80 + round
        fcntl.ioctl(terminal, termios.TIOCSWINSZ, struct.pack('HHHH', rows, columns, 0, 0))
        until(b'size:%dx%d' % (columns, rows), 2)
        before = data.count(b'continued')
        os.kill(pid, signal.SIGCONT)
        deadline = time.monotonic() + 2
        while data.count(b'continued') == before:
            assert time.monotonic() < deadline, ('SIGCONT handler deferred without input', data[-300:])
            if select.select([terminal], [], [], .02)[0]:
                data += os.read(terminal, 4096)
    print('signals-ok')
finally:
    os.kill(pid, signal.SIGKILL)
    os.waitpid(pid, 0)
`

// Regression for CI stalls (jobs 3447/3503/3530): CPython runs Python signal handlers only
// at interpreter checkpoints, so a SIGCONT or SIGWINCH landing just before a blocking
// read() was deferred until input that the driver only sends after the handler's output.
// Fixtures must install their handlers before 'ready' and poll before reading.
func TestMatrixFixturesHandleSignalsWithoutInput(t *testing.T) {
	for name, source := range map[string]string{"matrixPTYHarness": matrixPTYHarness, "matrix_probe.py": matrixProbe,
		"supervisorResizeHarness": supervisorResizeHarness} {
		ready := strings.Index(source, "'ready'")
		if ready < 0 {
			ready = strings.Index(source, `"ready"`)
		}
		read := strings.Index(source, "sys.stdin.readline()")
		poll := strings.LastIndex(source[:max(read, 0)], "select.select([0]")
		for _, handler := range []string{"signal.SIGCONT,", "signal.SIGWINCH,"} {
			if name == "supervisorResizeHarness" && handler == "signal.SIGCONT," {
				continue // This fixture only waits for resize output.
			}
			if at := strings.Index(source, "signal.signal("+handler); at < 0 || at > ready {
				t.Fatalf("%s: %s handler must be installed before ready", name, handler)
			}
		}
		if ready < 0 || read < 0 || poll < ready {
			t.Fatalf("%s: must poll with select between ready and its blocking readline", name)
		}
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY driver requires Python stdlib")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "expected-argv.json"), []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "-c", matrixSignalDriver, matrixPTYHarness, "25")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "HRX_MATRIX_DIR=" + dir, "HRX_MATRIX_COMMS=off"}
	output, err := cmd.CombinedOutput()
	if err != nil || string(output) != "signals-ok\n" {
		t.Fatalf("fixture signal handling: %v; %s", err, output)
	}
}
