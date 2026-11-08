package app

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/meshcrawler/hrx/internal/session"
)

const resumeReceiptEnv = "HRX_RESUME_RECEIPT"

var resumeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,255}$`)

func interactiveResumeHint(l Launch) bool {
	if l.resumeProject == "" {
		return false
	}
	var group int32
	if terminalGroup(os.Stdin.Fd(), syscall.TIOCGPGRP, &group) != nil || terminalGroup(os.Stderr.Fd(), syscall.TIOCGPGRP, &group) != nil {
		return false
	}
	command := nativeCommand(l.Executable, l.Passthrough)
	if l.Executable == "codex" && (command == "exec" || command == "e") || l.Executable == "opencode" && command == "run" {
		return false
	}
	for _, token := range nativeArguments(l.Executable, l.Passthrough) {
		if !token.option {
			continue
		}
		name, _, _ := strings.Cut(l.Passthrough[token.index], "=")
		if name == "--bg" || name == "--background" || name == "--tmux" || l.Executable == "claude" && (name == "-p" || name == "--print") {
			return false
		}
	}
	return l.Executable == "codex" || l.Executable == "claude" || l.Executable == "opencode"
}

func prepareResumeReceipt(l *Launch) (string, func()) {
	env := l.Env
	if env == nil {
		env = os.Environ()
	}
	l.Env = childEnvironment(env, nil, []string{resumeReceiptEnv})
	dir, err := os.MkdirTemp("", "hrx-resume-")
	if err != nil {
		return "", nil
	}
	path := filepath.Join(dir, "session-id")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil
	}
	l.Env = childEnvironment(l.Env, map[string]string{resumeReceiptEnv: path}, nil)
	return path, func() { _ = os.RemoveAll(dir) }
}

func writeResumeReceipt(id string) {
	path := os.Getenv(resumeReceiptEnv)
	if path == "" || !resumeID.MatchString(id) {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return
	}
	if f.Truncate(0) == nil {
		_, _ = f.WriteString(id)
	}
}

func shellWord(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func printResumeHint(out io.Writer, l Launch, receipt string) {
	id := ""
	if receipt != "" {
		f, err := os.OpenFile(receipt, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err == nil {
			info, e := f.Stat()
			if e == nil && info.Mode().IsRegular() && info.Mode().Perm()&0077 == 0 {
				b, _ := io.ReadAll(io.LimitReader(f, 257))
				id = string(b)
			}
			_ = f.Close()
		}
	}
	if id == "" && l.pendingSession != nil {
		id = l.pendingSession.NativeID
	}
	if id == "" {
		id = l.Resume
	}
	if resumeID.MatchString(id) {
		store, err := session.Default()
		if err == nil {
			_, err = store.Resolve(l.Executable, l.resumeProject, id)
			if err == nil {
				fmt.Fprintf(out, "\nhrx: Session ended. Resume this session:\n  cd %s && hrx %s --resume %s\n", shellWord(l.resumeProject), l.Executable, shellWord(id))
				return
			}
		}
	}
	fmt.Fprintln(out, "\nhrx: Session ended; no resumable session ID was captured. Use the harness's native session ID with explicit HRX launch settings. For Codex, trust the HRX SessionStart hook in /hooks to enable exact resume hints.")
}
