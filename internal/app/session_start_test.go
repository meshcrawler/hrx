package app

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/session"
)

func TestSessionSuccessfulExecReleasesIndexLock(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHarnessProcessHelper$")
	cmd.Env = append(os.Environ(), "HRX_TEST_PROCESS_HELPER=session")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatal("child did not start", err)
	}
	done := make(chan error, 1)
	go func() {
		store, err := session.Default()
		if err == nil {
			var record session.Record
			record, err = store.Resolve("opencode", "/project", "fixture-id")
			if err == nil && record.Route != "fixture/route" {
				err = errors.New("incorrect successful-launch metadata")
			}
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("exec retained the index lock in its running child")
	}
}

func TestSessionSuccessfulSupervisedLaunchIsIndexed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	record := session.Record{NativeID: "fixture-id", Harness: "opencode", Identity: "fixture", Route: "fixture/route", Project: "/project"}
	status, err := superviseHarness(Launch{Executable: "sh", Args: []string{"-c", "exit 7"}, pendingSession: &record}, recordedLease{filepath.Join(t.TempDir(), "released")})
	if err != nil || status.ExitStatus() != 7 {
		t.Fatal("supervised launch failed", err)
	}
	store, _ := session.Default()
	saved, err := store.Resolve("opencode", "/project", "fixture-id")
	if err != nil || saved.Route != record.Route {
		t.Fatal("started session not indexed", err)
	}
}

func TestSessionStartupFailureDoesNotChangeLatest(t *testing.T) {
	for _, supervised := range []bool{false, true} {
		for _, resume := range []bool{false, true} {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			store, _ := session.Default()
			prior := session.Record{NativeID: "prior", Harness: "opencode", Identity: "fixture", Route: "old/route", Project: "/project"}
			if err := store.Save(prior); err != nil {
				t.Fatal(err)
			}
			before, _ := store.Resolve("opencode", "/project", "")
			record := prior
			record.Route = "new/route"
			if !resume {
				record.NativeID = "new-id"
			}
			dir := t.TempDir()
			broken := filepath.Join(dir, "broken")
			// LookPath succeeds, but kernel process creation fails.
			if err := os.WriteFile(broken, []byte("#!/nonexistent/hrx-fixture-interpreter\n"), 0700); err != nil {
				t.Fatal(err)
			}
			launch := Launch{Executable: broken, pendingSession: &record}
			var err error
			if supervised {
				release := filepath.Join(dir, "released")
				_, err = superviseHarness(launch, recordedLease{release})
				if _, releaseErr := os.Stat(release); releaseErr != nil {
					t.Fatal("failed launch retained lease")
				}
			} else {
				err = execHarness(launch)
			}
			if err == nil {
				t.Fatal("invalid executable accepted")
			}
			after, err := store.Resolve("opencode", "/project", "")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("failed launch changed latest session", err)
			}
			if !resume {
				if _, err := store.Resolve("opencode", "/project", record.NativeID); !errors.Is(err, session.ErrNotFound) {
					t.Fatal("failed fresh launch retained metadata")
				}
			}
		}
	}
}
