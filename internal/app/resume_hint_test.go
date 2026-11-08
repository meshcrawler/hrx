package app

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/session"
)

func TestResumeHintNeverChoosesAnotherSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s, _ := session.Default()
	r := session.Record{NativeID: "different-session", Harness: "codex", Identity: "model", Route: "route", Project: "/project"}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	printResumeHint(&b, Launch{Executable: "codex", resumeProject: "/project"}, "")
	if strings.Contains(b.String(), "--continue") || strings.Contains(b.String(), "--resume") {
		t.Fatal("hint chose another session")
	}
}

func TestResumeHintHookReceipt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	project := t.TempDir()
	project, _ = filepath.EvalSymlinks(project)
	l := Launch{Executable: "claude", resumeProject: project}
	path, cleanup := prepareResumeReceipt(&l)
	if cleanup == nil {
		t.Fatal("no receipt")
	}
	defer cleanup()
	t.Setenv(resumeReceiptEnv, path)
	s, _ := session.Default()
	meta := `{"harness":"claude","model_identity":"model","model_route":"route","project":"` + project + `"}`
	event := `{"hook_event_name":"SessionStart","source":"startup","session_id":"exact-id","cwd":"` + project + `"}`
	if err := receiveSessionStart(strings.NewReader(event), meta, s); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	printResumeHint(&b, l, path)
	if !strings.Contains(b.String(), "hrx claude --resume 'exact-id'") {
		t.Fatal("missing exact hint", b.String())
	}
	if shellWord("a'b") != "'a'\"'\"'b'" {
		t.Fatal("unsafe quoting")
	}
}

func TestResumeHintProcessHelper(t *testing.T) {
	mode := os.Getenv("HRX_HINT_TEST")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	s, _ := session.Default()
	project := os.Getenv("HRX_HINT_PROJECT")
	if err := s.Save(session.Record{NativeID: "exact-id", Harness: "codex", Identity: "fixture", Route: "fixture", Project: project}); err != nil {
		t.Fatal(err)
	}
	l := Launch{Executable: "codex", resumeProject: project}
	if mode == "oneshot" {
		l.Passthrough = []string{"exec", "task"}
	}
	if err := execHarness(l); err != nil {
		t.Fatal(err)
	}
}

func TestResumeHintInteractiveExitAndSignal(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python required for PTY")
	}
	for _, mode := range []string{"exit", "signal", "stop", "oneshot"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			script := "#!/bin/sh\nif [ -n \"$HRX_RESUME_RECEIPT\" ]; then printf exact-id > \"$HRX_RESUME_RECEIPT\"; fi\n"
			if mode == "signal" {
				script += "kill -TERM $$\n"
			} else if mode == "stop" {
				script += "kill -STOP $$\nexit 37\n"
			} else {
				script += "exit 37\n"
			}
			if err := os.WriteFile(filepath.Join(root, "codex"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			driver := `import os,pty,sys,errno,signal
pid,fd=pty.fork()
if pid==0: os.execv(sys.argv[1],[sys.argv[1],'-test.run=^TestResumeHintProcessHelper$'])
data=b''
if os.environ['HRX_HINT_TEST']=='stop':
 _,status=os.waitpid(pid,os.WUNTRACED)
 assert os.WIFSTOPPED(status),status
 assert os.tcgetpgrp(fd)==pid
 os.kill(pid,signal.SIGCONT)
try:
 while True:
  try: chunk=os.read(fd,4096)
  except OSError as e:
   if e.errno==errno.EIO: break
   raise
  if not chunk: break
  data+=chunk
finally: os.close(fd)
_,status=os.waitpid(pid,0)
mode=os.environ['HRX_HINT_TEST']
if mode=='signal': assert os.WIFSIGNALED(status) and os.WTERMSIG(status)==signal.SIGTERM,status
else: assert os.WIFEXITED(status) and os.WEXITSTATUS(status)==37,status
if mode=='oneshot': assert b'Resume this session' not in data,data
else: assert b"hrx codex --resume 'exact-id'" in data,data
print('ok')
`
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, python, "-I", "-c", driver, os.Args[0])
			cmd.Env = childEnvironment(os.Environ(), map[string]string{"PATH": root + ":" + os.Getenv("PATH"), "XDG_STATE_HOME": filepath.Join(root, "state"), "HRX_HINT_TEST": mode, "HRX_HINT_PROJECT": root, "HRX_MONITOR": "off"}, nil)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("PTY exit/hint failed: %v %s", err, out)
			}
		})
	}
}
