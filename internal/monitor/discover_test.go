package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRecoveredRecord(t *testing.T) {
	env := stringsFromEnvironment([]byte("HRX_SESSION_METADATA={\"harness\":\"claude\",\"model_identity\":\"opus\",\"native_id\":\"session-1\"}\x00HRX_AGENT_IDENTITY=pitput-dev-h1\x00HRX_GITLAB_CREDENTIAL_HOST=gitlab.example\x00HRX_GITLAB_CREDENTIAL_USER=hrx_test\x00GITLAB_TOKEN=SECRET\x00OTHER=SECRET\x00"))
	r, ok := recoveredRecord(42, "Tue Oct 20 12:00:00 2026", "claude", env)
	if !ok || r.Agent != "pitput-dev-h1" || r.SessionID != "session-1" || r.Credential.Username != "hrx_test" || r.Activity != "recovered-launch" {
		t.Fatal("legacy metadata lost")
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "SECRET") || strings.Contains(strings.Join(env, " "), "SECRET") {
		t.Fatal("unapproved environment escaped")
	}
	for _, extra := range []string{"HRX_MONITOR=off", RunEnv + "=already-tracked", "HRX_SESSION_METADATA=invalid"} {
		if _, ok := recoveredRecord(42, "Tue Oct 20 12:00:00 2026", "claude", append(append([]string{}, env...), extra)); ok {
			t.Fatal("unexpected recovery", extra)
		}
	}
	if _, ok := recoveredRecord(42, "Tue Oct 20 12:00:00 2026", "codex", env); ok {
		t.Fatal("mismatched harness accepted")
	}
	other, _ := recoveredRecord(42, "Tue Oct 20 13:00:00 2026", "claude", env)
	if other.RunID == r.RunID {
		t.Fatal("PID reuse shares identity")
	}
}

func TestRecoveryHonorsStateDirectory(t *testing.T) {
	s := Store{filepath.Join("/isolated", "hrx", "monitor")}
	if !s.ownsEnvironment([]string{"XDG_STATE_HOME=/isolated", "HOME=/elsewhere"}) {
		t.Fatal("explicit state directory ignored")
	}
	for _, env := range [][]string{{"XDG_STATE_HOME=/other"}, {"HOME=/other"}, {"XDG_STATE_HOME=relative"}, nil} {
		if s.ownsEnvironment(env) {
			t.Fatal("unrelated environment admitted")
		}
	}
}

func TestRecoveryProcessHelper(t *testing.T) {
	if os.Getenv("HRX_RECOVERY_TEST") != "1" {
		t.Skip("subprocess helper")
	}
	fmt.Println("ready")
	time.Sleep(30 * time.Second)
}

func TestRecoverExistingProcess(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("OS discovery unavailable")
	}
	root := t.TempDir()
	name := filepath.Join(root, "claude")
	// Copy rather than symlink so the OS executable name is the harness name.
	b, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, b, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(name, "-test.run=^TestRecoveryProcessHelper$")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "HRX_RECOVERY_TEST=1", "HRX_SESSION_METADATA={\"harness\":\"claude\",\"model_identity\":\"fixture\"}", "HRX_AGENT_IDENTITY=recovery-test", "GITLAB_TOKEN=DO_NOT_STORE"}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if _, err := bufio.NewReader(out).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	s := Store{filepath.Join(root, ".local", "state", "hrx", "monitor")}
	ctx := context.Background()
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found Record
	for _, r := range rows {
		if r.PID == cmd.Process.Pid {
			found = r
		}
	}
	if found.Agent != "recovery-test" || found.Status != "running" {
		t.Fatal("existing harness not discovered")
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	again, _ := s.Snapshot(ctx)
	count := 0
	for _, r := range again {
		if r.PID == found.PID {
			count++
			if !r.LastActivity.Equal(found.LastActivity) {
				t.Fatal("poll fabricated activity")
			}
		}
	}
	if count != 1 {
		t.Fatal("recovery duplicated run")
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, found.RunID+".json"))
	if err != nil || strings.Contains(string(data), "DO_NOT_STORE") {
		t.Fatal("secret persisted")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	rows, _ = s.Snapshot(ctx)
	for _, r := range rows {
		if r.RunID == found.RunID && r.Status != "exited" {
			t.Fatal("exit not observed")
		}
	}
}
