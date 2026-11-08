//go:build darwin || linux

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
	"unsafe"
)

type presenceLease interface {
	renew(context.Context) error
	release() error
}

type childEvent struct {
	status syscall.WaitStatus
	err    error
}

// supervisorTrace appends each job-control step to HRX_SUPERVISOR_TRACE. It is a
// test-only diagnostic for intermittent stop/continue races and a no-op when unset.
func supervisorTrace() func(string, ...any) {
	path := os.Getenv("HRX_SUPERVISOR_TRACE")
	if path == "" {
		return func(string, ...any) {}
	}
	return func(format string, args ...any) {
		file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return
		}
		defer file.Close()
		fmt.Fprintf(file, "%s "+format+"\n", append([]any{time.Now().Format("15:04:05.000000")}, args...)...)
	}
}

func terminalGroup(fd uintptr, request uintptr, group *int32) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(unsafe.Pointer(group)))
	if errno != 0 {
		return errno
	}
	return nil
}

// superviseHarness owns the child process group and the lease. The caller must
// propagate the returned wait status only after this function finishes cleanup.
func superviseHarness(launch Launch, lease presenceLease) (syscall.WaitStatus, error) {
	if lease != nil {
		defer lease.release()
	}
	path, err := exec.LookPath(launch.Executable)
	if err != nil {
		return 0, errors.New("harness executable not found on PATH")
	}
	env := launch.Env
	if env == nil {
		env = os.Environ()
	}
	parentGroup := int32(syscall.Getpgrp())
	var foreground int32
	hasTerminal := terminalGroup(os.Stdin.Fd(), syscall.TIOCGPGRP, &foreground) == nil
	attributes := &syscall.SysProcAttr{Setpgid: true}
	if hasTerminal && foreground == parentGroup {
		attributes.Foreground = true
		attributes.Ctty = int(os.Stdin.Fd())
	}
	// Restoring the terminal while the child owns it must not stop the parent.
	ignoredTTOU := signal.Ignored(syscall.SIGTTOU)
	signal.Ignore(syscall.SIGTTOU)
	defer func() {
		if !ignoredTTOU {
			signal.Reset(syscall.SIGTTOU)
		}
	}()
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP,
		syscall.SIGQUIT, syscall.SIGTSTP, syscall.SIGCONT, syscall.SIGWINCH)
	defer signal.Stop(signals)
	var child *os.Process
	err = launch.startWithSession(func() error {
		var startErr error
		child, startErr = os.StartProcess(path, append([]string{path}, launch.Args...), &os.ProcAttr{
			Env: env, Files: []*os.File{os.Stdin, os.Stdout, os.Stderr}, Sys: attributes,
		})
		return startErr
	})
	if err != nil {
		return 0, errors.New("harness process failed to start")
	}
	defer child.Release()
	childGroup := int32(child.Pid)
	trace := supervisorTrace()
	reclaim := func() {
		var current int32
		if hasTerminal && terminalGroup(os.Stdin.Fd(), syscall.TIOCGPGRP, &current) == nil && current == childGroup {
			err := terminalGroup(os.Stdin.Fd(), syscall.TIOCSPGRP, &parentGroup)
			trace("reclaim foreground %d -> %d err=%v", current, parentGroup, err)
		}
	}
	defer reclaim()
	forward := func(sig syscall.Signal) {
		err := syscall.Kill(-child.Pid, sig)
		trace("forward %v to group %d err=%v", sig, child.Pid, err)
	}
	events := make(chan childEvent, 1)
	go func() {
		for {
			var status syscall.WaitStatus
			_, err := syscall.Wait4(child.Pid, &status, syscall.WUNTRACED, nil)
			if err == syscall.EINTR {
				continue
			}
			events <- childEvent{status, err}
			if err != nil || status.Exited() || status.Signaled() {
				return
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	renewed := make(chan error, 1)
	renewing := false
	defer func() {
		cancel()
		if renewing {
			<-renewed
		}
	}()
	ticker := time.NewTicker(yesodLeaseTTL / 4)
	defer ticker.Stop()
	var killTimer *time.Timer
	var killDeadline <-chan time.Time
	defer func() {
		if killTimer != nil {
			killTimer.Stop()
		}
	}()
	terminate := func(sig syscall.Signal) {
		forward(sig)
		forward(syscall.SIGCONT)
		if killTimer == nil {
			killTimer = time.NewTimer(3 * time.Second)
			killDeadline = killTimer.C
		}
	}
	for {
		select {
		case err := <-launch.sidecarErrors:
			fmt.Fprintln(os.Stderr, "hrx:", err)
			launch.sidecarErrors = nil
			terminate(syscall.SIGTERM)
		case event := <-events:
			if event.err != nil {
				forward(syscall.SIGKILL)
				return 0, errors.New("cannot wait for harness process")
			}
			if event.status.Exited() || event.status.Signaled() {
				// Do not leave background descendants using a released identity.
				if lease != nil || launch.hubClient != nil {
					forward(syscall.SIGKILL)
				}
				return event.status, nil
			}
			// Darwin's syscall.WaitStatus.Stopped excludes SIGSTOP because
			// its continued encoding overlaps that signal. We never request
			// WCONTINUED, so the POSIX stopped marker here is unambiguous.
			if event.status&0xff == 0x7f {
				trace("child stopped status=%#x", int(event.status))
				if killTimer != nil {
					forward(syscall.SIGCONT)
					continue
				}
				if launch.hubClient != nil {
					// A queued stop may arrive after the supervisor resumes. Never
					// suspend again while cluster ownership may already be lost.
					terminate(syscall.SIGTERM)
					continue
				}
				reclaim()
				err := syscall.Kill(os.Getpid(), syscall.SIGSTOP)
				trace("self SIGSTOP err=%v", err)
			}
		case incoming := <-signals:
			sig := incoming.(syscall.Signal)
			switch sig {
			case syscall.SIGCONT:
				// A suspended hub supervisor may have lost its ownership while
				// stopped. Require a fresh launch rather than resume a stale child.
				if launch.hubClient != nil {
					terminate(syscall.SIGKILL)
					continue
				}
				// A stopped supervisor cannot renew. Validate ownership before
				// letting its stopped child run again, even after a short stop.
				if lease != nil {
					if err := lease.renew(ctx); err != nil {
						terminate(syscall.SIGTERM)
						continue
					}
				}
				var current int32
				getErr := terminalGroup(os.Stdin.Fd(), syscall.TIOCGPGRP, &current)
				trace("SIGCONT received foreground=%d err=%v", current, getErr)
				if hasTerminal && getErr == nil && current == parentGroup {
					err := terminalGroup(os.Stdin.Fd(), syscall.TIOCSPGRP, &childGroup)
					trace("restore foreground %d -> %d err=%v", current, childGroup, err)
				}
				forward(syscall.SIGCONT)
			case syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT:
				terminate(sig)
			default:
				forward(sig)
			}
		case <-ticker.C:
			if lease != nil && !renewing && killTimer == nil {
				renewing = true
				go func() { renewed <- lease.renew(ctx) }()
			}
		case err := <-renewed:
			renewing = false
			if err != nil {
				terminate(syscall.SIGTERM)
			}
		case <-killDeadline:
			forward(syscall.SIGKILL)
			killDeadline = nil
		}
	}
}
