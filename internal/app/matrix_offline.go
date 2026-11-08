package app

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/meshcrawler/hrx/internal/catalog"
	"github.com/meshcrawler/hrx/internal/matrix"
	"github.com/meshcrawler/hrx/internal/session"
)

//go:embed matrix_probe.py
var matrixProbe string

//go:embed matrix_pty.py
var matrixPTYProgram string

type offlineMatrixExecutor struct {
	input              matrix.Input
	executable, python string
	// diagnostics, when set (tests), receives the bounded worker output of a
	// timed-out or failed cell. Reports never carry it.
	diagnostics func(string)
}

type matrixFixture struct {
	Cell      matrix.Cell   `json:"cell"`
	Entry     catalog.Entry `json:"entry"`
	Agent     string        `json:"agent"`
	Reasoning string        `json:"reasoning"`
}

func matrixFailure(category string) matrix.Outcome {
	return matrix.Outcome{Status: "fail", Category: category, Coverage: []string{}}
}

func (e offlineMatrixExecutor) ExecuteOffline(ctx context.Context, cell matrix.Cell) matrix.Outcome {
	if len(cell.ID) != 64 {
		return matrixFailure("invalid-fixture")
	}
	var entry *catalog.Entry
	for _, candidate := range e.input.Snapshots[cell.Harness].Inventory {
		if candidate.ID == cell.Identity && candidate.Route == cell.Route && candidate.Provider == cell.Provider && candidate.Account == cell.Account {
			entry = &candidate
			break
		}
	}
	if entry == nil {
		return matrixFailure("invalid-fixture")
	}
	// Build the tuple's launchable view exactly as its declared authorization and
	// capability allow. No real key, credential helper or gateway is consulted.
	if cell.Expected == "reject" {
		if entry.Status(cell.Harness) == "ready" {
			return matrixFailure("assertion")
		}
		s := Settings{Harness: cell.Harness}
		o := LaunchOptions{Model: cell.Route}
		var err error
		if cell.Harness == "codex" {
			_, err = BuildLaunch(s, catalog.Snapshot{}, o)
		} else {
			_, err = buildOtherLaunch(s, catalog.Snapshot{}, o, "")
		}
		if err == nil {
			return matrixFailure("assertion")
		}
		return matrix.Outcome{Status: "pass", Category: "expected-rejection", Coverage: []string{"authorization-rejection", "secret-redaction"}}
	}
	if !slices.Contains([]string{"one-shot", "interactive"}, cell.Execution) {
		return matrixFailure("invalid-fixture")
	}
	if e.executable == "" || e.python == "" {
		return matrix.Outcome{Status: "blocked", Category: "missing-binary", Coverage: []string{}}
	}
	dir, err := os.MkdirTemp("", "hrx-offline-cell-")
	if err != nil {
		return matrixFailure("invalid-fixture")
	}
	defer os.RemoveAll(dir)
	// Keep the harness PATH isolated while allowing real process observations.
	ps, err := exec.LookPath("ps")
	if err != nil {
		return matrix.Outcome{Status: "blocked", Category: "missing-binary", Coverage: []string{}}
	}
	if os.Symlink(ps, filepath.Join(dir, "ps")) != nil {
		return matrixFailure("invalid-fixture")
	}
	fixture := matrixFixture{Cell: cell, Entry: *entry, Agent: "fixture/hrx/" + cell.ID[:16], Reasoning: cell.Reasoning}
	if fixture.Reasoning == "" {
		fixture.Reasoning = entry.Harnesses[cell.Harness].DefaultReasoning
	}
	data, err := json.Marshal(fixture)
	if err != nil {
		return matrixFailure("invalid-fixture")
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	scripts := map[string]string{
		"fixture.json": string(data),
		"probe.py":     matrixProbe,
		"pty.py":       matrixPTYProgram,
		cell.Harness:   "#!/bin/sh\nexec " + quote(e.python) + " -I " + quote(filepath.Join(dir, "probe.py")) + " \"$@\"\n",
		"python3":      "#!/bin/sh\nprintf 'allocate\\n' >> \"$HRX_MATRIX_DIR/events\"\nprintf '{\"agent\":\"%s\"}' \"$HRX_MATRIX_AGENT\"\n",
		"yesod": `#!/bin/sh
[ -z "$YESOD_AGENT_IDENTITY$YESOD_MAIL_IDENTITY$YESOD_MAIL_SELF" ] || exit 9
read -r token
[ "${#token}" = 64 ] || exit 9
case "$2" in claim|release|renew) printf '%s\n' "$2" >> "$HRX_MATRIX_DIR/events";; *) exit 9;; esac
`,
	}
	for name, contents := range scripts {
		mode := os.FileMode(0600)
		if slices.Contains([]string{cell.Harness, "python3", "yesod"}, name) {
			mode = 0700
		}
		if os.WriteFile(filepath.Join(dir, name), []byte(contents), mode) != nil {
			return matrixFailure("invalid-fixture")
		}
	}
	cmd := exec.CommandContext(ctx, e.executable, "_matrix-child")
	if cell.Execution == "interactive" {
		cmd = exec.CommandContext(ctx, e.python, "-I", filepath.Join(dir, "pty.py"), e.executable)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 4 * time.Second
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + dir, "HOME=" + dir, "XDG_STATE_HOME=" + filepath.Join(dir, "state"),
		"HRX_MATRIX_DIR=" + dir, "HRX_MATRIX_AGENT=" + fixture.Agent, "GORACE=atexit_sleep_ms=0",
		"HRX_OFFLINE_KEY=offline-key-sentinel",
		"YESOD_AGENT_IDENTITY=parent/fixture/unused", "YESOD_MAIL_IDENTITY=parent/fixture/unused", "YESOD_MAIL_SELF=parent/fixture/unused",
		"ANTHROPIC_AUTH_TOKEN=parent-fixture-token", "ANTHROPIC_API_KEY=parent-fixture-key"}
	remaining := 15 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
	}
	// The PTY driver's own deadline fires first, leaving time for its cleanup and
	// for it to report the phase it was waiting in before the context cancels it.
	childBudget := max(remaining-3*time.Second, remaining/2)
	cmd.Env = append(cmd.Env, "HRX_MATRIX_TIMEOUT="+strconv.FormatFloat(childBudget.Seconds(), 'f', 6, 64))
	if e.diagnostics != nil {
		cmd.Env = append(cmd.Env, "HRX_SUPERVISOR_TRACE="+filepath.Join(dir, "supervisor.trace"))
	}
	var output limitedBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err = cmd.Run()
	diagnose := func(outcome matrix.Outcome) matrix.Outcome {
		if e.diagnostics != nil {
			events, _ := os.ReadFile(filepath.Join(dir, "events"))
			trace, _ := os.ReadFile(filepath.Join(dir, "supervisor.trace"))
			e.diagnostics(fmt.Sprintf("%s after %s (context %v, exit %v); events %q; output %q; supervisor trace %q", outcome.Category,
				childBudget.Round(time.Millisecond), ctx.Err(), err, events,
				strings.ReplaceAll(output.String(), "offline-key-sentinel", "[redacted]"), trace))
		}
		return outcome
	}
	if ctx.Err() != nil {
		return diagnose(matrix.Outcome{Status: "blocked", Category: "timeout-or-cancelled", Coverage: []string{}})
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) && exited.ExitCode() == 124 && cell.Execution == "interactive" {
		return diagnose(matrix.Outcome{Status: "blocked", Category: "timeout-or-cancelled", Coverage: []string{}})
	}
	validExit := cell.Execution == "interactive" && err == nil || cell.Execution == "one-shot" && errors.As(err, &exited) && exited.ExitCode() == 37
	if !validExit || output.String() != "offline-probe-ok\n" {
		return diagnose(matrixFailure("unexpected-exit"))
	}
	events, err := os.ReadFile(filepath.Join(dir, "events"))
	want := "launch\n"
	checks := []string{"argv", "environment", "configuration", "reasoning", "secret-redaction", "permissions"}
	if cell.Communications != "off" {
		want = "allocate\nclaim\nlaunch\nrelease\n"
		if cell.Execution == "interactive" {
			want = "allocate\nclaim\nlaunch\nrenew\nrelease\n"
		}
		checks = append(checks, "identity-isolation", "prompt-injection", "lease-cleanup")
	}
	if err != nil || string(events) != want {
		return matrixFailure("assertion")
	}
	if cell.Session != "fresh" {
		checks = append(checks, "session-restoration")
	}
	if cell.Execution == "interactive" {
		checks = append(checks, "pty-signals", "pty-resize", "pty-job-control")
	}
	return matrix.Outcome{Status: "pass", Coverage: checks}
}

// This internal entry point never reads user settings or credentials. Its
// environment and directory are private fixtures created by ExecuteOffline.
func runMatrixChild(ctx context.Context) error {
	dir := os.Getenv("HRX_MATRIX_DIR")
	if dir == "" || os.Getenv("PATH") != dir || os.Getenv("HOME") != dir || os.Getenv("XDG_STATE_HOME") != filepath.Join(dir, "state") {
		return errors.New("missing offline fixture")
	}
	data, err := os.ReadFile(filepath.Join(dir, "fixture.json"))
	var fixture matrixFixture
	if err != nil || len(data) > 1024*1024 || json.Unmarshal(data, &fixture) != nil {
		return errors.New("invalid offline fixture")
	}
	cell := fixture.Cell
	if !slices.Contains([]string{"codex", "claude", "opencode"}, cell.Harness) || fixture.Entry.Status(cell.Harness) != "ready" {
		return errors.New("invalid offline tuple")
	}
	o := LaunchOptions{Model: cell.Route, Reasoning: cell.Reasoning, Yolo: cell.Permissions == "yolo",
		YesodMail: cell.Communications == "mail" || cell.Communications == "both", YesodBBS: cell.Communications == "bulletin" || cell.Communications == "both",
		Extra: []string{map[string]string{"codex": "exec", "claude": "-p", "opencode": "run"}[cell.Harness], "offline fixture prompt"}}
	if cell.Execution == "interactive" {
		o.Extra = nil
	}
	if cell.Session != "fresh" {
		store, err := session.Default()
		if err != nil {
			return err
		}
		r := session.Record{NativeID: "offline-session", Harness: cell.Harness, Identity: cell.Identity, Route: cell.Route, Reasoning: fixture.Reasoning, Project: dir, Mail: o.YesodMail, BBS: o.YesodBBS}
		if r.Mail || r.BBS {
			r.Agent = fixture.Agent
		}
		if err = store.Save(r); err != nil {
			return err
		}
		o.Model, o.Reasoning = "", ""
		o.YesodMail, o.YesodBBS = false, false
		o.Resume = "offline-session"
		if cell.Session == "continue" {
			o.Resume, o.Continue = "", true
		}
		o, _, err = restoreSession(store, cell.Harness, dir, o)
		if err != nil || o.Model != r.Route || o.Reasoning != r.Reasoning || o.YesodAgent != r.Agent || o.Yolo != (cell.Permissions == "yolo") {
			return errors.New("offline restoration mismatch")
		}
	}
	s := Settings{Harness: cell.Harness, ProviderName: "fixture", CachePath: filepath.Join(dir, "catalog.json"), GeneratedCatalogPath: filepath.Join(dir, "codex.json"), Provider: Provider{BaseURL: "http://127.0.0.1:1/v1", WireAPI: "responses", EnvKey: "HRX_OFFLINE_KEY"}}
	model := catalog.Model{ID: cell.Identity, Route: cell.Route, Harnesses: fixture.Entry.Harnesses, Context: fixture.Entry.Context, Output: fixture.Entry.Output}
	snapshot := catalog.Snapshot{Models: []catalog.Model{model}}
	template, _ := json.Marshal(map[string]any{"models": []any{map[string]string{"slug": cell.Route, "base_instructions": "offline base instructions"}}})
	if os.WriteFile(s.GeneratedCatalogPath, template, 0600) != nil {
		return errors.New("offline catalog write failed")
	}
	var launch Launch
	if cell.Harness == "codex" {
		launch, err = BuildLaunch(s, snapshot, o)
	} else {
		launch, err = buildOtherLaunch(s, snapshot, o, "offline-key-sentinel")
	}
	if err != nil {
		return err
	}
	if err = prepareYesod(ctx, s, o, &launch, io.Discard); err != nil {
		return err
	}
	defer func() {
		// Successful exec/supervision exits the worker; only pre-exec failures
		// return here and need their already-claimed lease released.
		if launch.lease != nil {
			_ = launch.lease.release()
		}
	}()
	encoded, _ := json.Marshal(launch)
	if strings.Contains(string(encoded), "offline-key-sentinel") {
		return errors.New("offline redaction failed")
	}
	args, _ := json.Marshal(append([]string{}, launch.Args...))
	if os.WriteFile(filepath.Join(dir, "argv.json"), args, 0600) != nil {
		return errors.New("offline argv write failed")
	}
	return execHarness(launch)
}
