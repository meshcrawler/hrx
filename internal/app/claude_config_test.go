package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var claudeTestTime = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func managedOptions(t *testing.T, parts ...string) LaunchOptions {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	project, _ = filepath.EvalSymlinks(project)
	dir, err := canonicalClaudeConfigDir(filepath.Join(root, "claude"))
	if err != nil {
		t.Fatal(err)
	}
	return LaunchOptions{ClaudeConfigDir: dir, ClaudeInit: parts, project: project, Yolo: true}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func noEnv(string) string { return "" }

func TestClaudeInitParsing(t *testing.T) {
	o, err := parseLaunchFor("claude", []string{"--claude-config-dir", "/x", "--claude-init", "bypass,theme"})
	if err != nil || strings.Join(o.ClaudeInit, ",") != "theme,bypass" || o.ClaudeConfigDir != "/x" {
		t.Fatalf("parts are canonically ordered: %v %v", o.ClaudeInit, err)
	}
	for _, bad := range []string{"", "theme,,trust", "theme,theme", "login", "all", "THEME"} {
		if _, err := parseLaunchFor("claude", []string{"--claude-config-dir", "/x", "--claude-init=" + bad}); err == nil {
			t.Errorf("--claude-init=%q must be refused", bad)
		}
	}
	for _, harness := range []string{"codex", "opencode"} {
		if _, err := parseLaunchFor(harness, []string{"--claude-config-dir", "/x"}); err == nil {
			t.Errorf("%s must refuse --claude-config-dir", harness)
		}
		if _, err := parseLaunchFor(harness, []string{"--claude-init", "theme"}); err == nil {
			t.Errorf("%s must refuse --claude-init", harness)
		}
	}
	if _, err := parseLaunchFor("claude", []string{"--claude-config-dir", "/x", "--claude-config-dir", "/y"}); err == nil {
		t.Error("duplicate --claude-config-dir must be refused")
	}
}

func TestClaudeInitNeedsAManagedDirectory(t *testing.T) {
	_, err := validateClaudeConfigOptions(LaunchOptions{ClaudeInit: []string{"theme"}}, noEnv)
	if err == nil || !strings.Contains(err.Error(), "never initializes an existing user configuration") {
		t.Fatalf("init without a managed directory must be refused, got %v", err)
	}
}

func TestClaudeConfigDirConflictsWithEnvironment(t *testing.T) {
	o := managedOptions(t)
	other := t.TempDir()
	if _, err := validateClaudeConfigOptions(o, func(string) string { return other }); err == nil ||
		!strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("a different CLAUDE_CONFIG_DIR must fail clearly, got %v", err)
	}
	if _, err := validateClaudeConfigOptions(o, func(string) string { return o.ClaudeConfigDir }); err != nil {
		t.Fatalf("the same directory is not a conflict: %v", err)
	}
}

func TestClaudeBypassFollowsEffectivePermissionMode(t *testing.T) {
	o := managedOptions(t, "bypass")
	o.Yolo = false
	if _, err := validateClaudeConfigOptions(o, noEnv); err == nil {
		t.Fatal("bypass must be refused without the bypass permission mode")
	}
	o.Yolo = true
	if _, err := validateClaudeConfigOptions(o, noEnv); err != nil {
		t.Fatal(err)
	}
}

func TestNoManagedDirectoryWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := ensureClaudeConfig(LaunchOptions{project: home}, claudeTestTime); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Fatalf("default launches must not write anything: %v", entries)
	}
}

func TestDryRunNeverCreatesTheManagedDirectory(t *testing.T) {
	o := managedOptions(t, "theme")
	o.DryRun = true
	if err := ensureClaudeConfig(o, claudeTestTime); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(o.ClaudeConfigDir); !os.IsNotExist(err) {
		t.Fatal("dry run created the managed directory")
	}
}

func TestCreatedDirectoryCarriesProvenance(t *testing.T) {
	o := managedOptions(t)
	if err := ensureClaudeConfig(o, claudeTestTime); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Lstat(o.ClaudeConfigDir)
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("managed directory mode %v", info.Mode().Perm())
	}
	marker, err := verifyClaudeConfigDir(o.ClaudeConfigDir)
	if err != nil || marker.CreatedBy != "hrx" || marker.UID != os.Getuid() || len(marker.Initialized) != 0 {
		t.Fatalf("marker %+v, %v", marker, err)
	}
	if _, err := os.Lstat(filepath.Join(o.ClaudeConfigDir, claudeStateFile)); !os.IsNotExist(err) {
		t.Fatal("no init parts must mean no Claude state written")
	}
}

func TestUserOwnedDirectoriesAreRefused(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"existing without marker": func(t *testing.T, dir string) {
			os.Mkdir(dir, 0o700)
			os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{}"), 0o600)
		},
		"existing empty": func(t *testing.T, dir string) { os.Mkdir(dir, 0o700) },
		"symlinked directory": func(t *testing.T, dir string) {
			target := t.TempDir()
			os.Symlink(target, dir)
		},
		"group-writable directory": func(t *testing.T, dir string) {
			os.Mkdir(dir, 0o700)
			stampMarker(t, dir)
			os.Chmod(dir, 0o770)
		},
		"symlinked marker": func(t *testing.T, dir string) {
			os.Mkdir(dir, 0o700)
			real := filepath.Join(t.TempDir(), "marker.json")
			os.WriteFile(real, validMarker(), 0o600)
			os.Symlink(real, filepath.Join(dir, claudeConfigMarker))
		},
		"malformed marker": func(t *testing.T, dir string) {
			os.Mkdir(dir, 0o700)
			os.WriteFile(filepath.Join(dir, claudeConfigMarker), []byte(`{"schema":1,"created_by":"someone"}`), 0o600)
		},
		"group-readable directory": func(t *testing.T, dir string) {
			os.Mkdir(dir, 0o700)
			stampMarker(t, dir)
			os.Chmod(dir, 0o750)
		},
		"world-readable directory": func(t *testing.T, dir string) {
			os.Mkdir(dir, 0o700)
			stampMarker(t, dir)
			os.Chmod(dir, 0o755)
		},
		"world-readable marker": func(t *testing.T, dir string) {
			os.Mkdir(dir, 0o700)
			stampMarker(t, dir)
			os.Chmod(filepath.Join(dir, claudeConfigMarker), 0o644)
		},
		"marker followed by junk": func(t *testing.T, dir string) {
			os.Mkdir(dir, 0o700)
			os.WriteFile(filepath.Join(dir, claudeConfigMarker), append(validMarker(), []byte(" junk")...), 0o600)
		},
		"marker followed by a second value": func(t *testing.T, dir string) {
			os.Mkdir(dir, 0o700)
			os.WriteFile(filepath.Join(dir, claudeConfigMarker), append(validMarker(), validMarker()...), 0o600)
		},
		"marker with an unknown initialized part": func(t *testing.T, dir string) {
			os.Mkdir(dir, 0o700)
			data := strings.Replace(string(validMarker()), `"initialized":[]`,
				`"initialized":[{"part":"login","at":"2026-10-23T12:00:00Z"}]`, 1)
			os.WriteFile(filepath.Join(dir, claudeConfigMarker), []byte(data), 0o600)
		},
		"marker with trust lacking a workspace": func(t *testing.T, dir string) {
			os.Mkdir(dir, 0o700)
			data := strings.Replace(string(validMarker()), `"initialized":[]`,
				`"initialized":[{"part":"trust","at":"2026-10-23T12:00:00Z"}]`, 1)
			os.WriteFile(filepath.Join(dir, claudeConfigMarker), []byte(data), 0o600)
		},
		"marker with a workspace on theme": func(t *testing.T, dir string) {
			os.Mkdir(dir, 0o700)
			data := strings.Replace(string(validMarker()), `"initialized":[]`,
				`"initialized":[{"part":"theme","workspace":"/x","at":"2026-10-23T12:00:00Z"}]`, 1)
			os.WriteFile(filepath.Join(dir, claudeConfigMarker), []byte(data), 0o600)
		},
		"marker with a zero record time": func(t *testing.T, dir string) {
			os.Mkdir(dir, 0o700)
			data := strings.Replace(string(validMarker()), `"initialized":[]`,
				`"initialized":[{"part":"theme","at":"0001-01-01T00:00:00Z"}]`, 1)
			os.WriteFile(filepath.Join(dir, claudeConfigMarker), []byte(data), 0o600)
		},
		"marker without initialized": func(t *testing.T, dir string) {
			os.Mkdir(dir, 0o700)
			data := strings.Replace(string(validMarker()), `,"initialized":[]`, ``, 1)
			os.WriteFile(filepath.Join(dir, claudeConfigMarker), []byte(data), 0o600)
		},
		"marker with unknown fields": func(t *testing.T, dir string) {
			os.Mkdir(dir, 0o700)
			data := strings.Replace(string(validMarker()), `"schema"`, `"extra":1,"schema"`, 1)
			os.WriteFile(filepath.Join(dir, claudeConfigMarker), []byte(data), 0o600)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			o := managedOptions(t, "theme")
			setup(t, o.ClaudeConfigDir)
			before := snapshotTree(t, filepath.Dir(o.ClaudeConfigDir))
			if err := ensureClaudeConfig(o, claudeTestTime); err == nil {
				t.Fatal("a directory HRX did not create must be refused")
			}
			if after := snapshotTree(t, filepath.Dir(o.ClaudeConfigDir)); after != before {
				t.Fatal("a refused directory was modified")
			}
		})
	}
}

func validMarker() []byte {
	return []byte(fmt.Sprintf(`{"schema":1,"created_by":"hrx","created_at":"2026-10-23T12:00:00Z","uid":%d,"initialized":[]}`,
		os.Getuid()))
}

func stampMarker(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, claudeConfigMarker), validMarker(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// snapshotTree lists every path, mode and content under root (symlinks unfollowed).
// The lock file is excluded: creating it changes no configuration or provenance.
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || filepath.Base(path) == claudeConfigLock {
			return nil
		}
		fmt.Fprintf(&b, "%s %v", path, info.Mode())
		if info.Mode().IsRegular() {
			data, _ := os.ReadFile(path)
			b.Write(data)
		}
		b.WriteString("\n")
		return nil
	})
	return b.String()
}

func TestUnsafeOrMalformedClaudeStateIsNeverOverwritten(t *testing.T) {
	cases := map[string]func(t *testing.T, path string){
		"malformed JSON":    func(t *testing.T, path string) { os.WriteFile(path, []byte("{not json"), 0o600) },
		"JSON array":        func(t *testing.T, path string) { os.WriteFile(path, []byte("[]"), 0o600) },
		"world-writable":    func(t *testing.T, path string) { os.WriteFile(path, []byte("{}"), 0o600); os.Chmod(path, 0o666) },
		"group-readable":    func(t *testing.T, path string) { os.WriteFile(path, []byte("{}"), 0o600); os.Chmod(path, 0o640) },
		"malformed project": func(t *testing.T, path string) { os.WriteFile(path, []byte(`{"projects":[]}`), 0o600) },
		"symlink": func(t *testing.T, path string) {
			real := filepath.Join(t.TempDir(), "state.json")
			os.WriteFile(real, []byte("{}"), 0o600)
			os.Symlink(real, path)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			o := managedOptions(t, "theme", "trust")
			if err := ensureClaudeConfig(LaunchOptions{ClaudeConfigDir: o.ClaudeConfigDir}, claudeTestTime); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(o.ClaudeConfigDir, claudeStateFile)
			setup(t, path)
			before := snapshotTree(t, o.ClaudeConfigDir)
			if err := ensureClaudeConfig(o, claudeTestTime); err == nil {
				t.Fatal("unsafe Claude state must fail")
			}
			if after := snapshotTree(t, o.ClaudeConfigDir); after != before {
				t.Fatal("unsafe Claude state or marker was modified")
			}
		})
	}
}

func TestEachPartWritesOnlyItsOwnAnswer(t *testing.T) {
	expect := map[string][]string{
		"theme":  {"hasCompletedOnboarding", "theme"},
		"trust":  {"projects"},
		"bypass": {"bypassPermissionsModeAccepted"},
	}
	for part, keys := range expect {
		t.Run(part, func(t *testing.T) {
			o := managedOptions(t, part)
			if err := ensureClaudeConfig(o, claudeTestTime); err != nil {
				t.Fatal(err)
			}
			state := readJSON(t, filepath.Join(o.ClaudeConfigDir, claudeStateFile))
			if len(state) != len(keys) {
				t.Fatalf("%s wrote %v", part, state)
			}
			for _, key := range keys {
				if _, ok := state[key]; !ok {
					t.Fatalf("%s did not write %s", part, key)
				}
			}
			marker, _ := verifyClaudeConfigDir(o.ClaudeConfigDir)
			if len(marker.Initialized) != 1 || marker.Initialized[0].Part != part {
				t.Fatalf("provenance %+v", marker.Initialized)
			}
		})
	}
}

func TestTrustIsScopedToTheExactWorkspace(t *testing.T) {
	o := managedOptions(t, "trust")
	if err := ensureClaudeConfig(o, claudeTestTime); err != nil {
		t.Fatal(err)
	}
	projects := readJSON(t, filepath.Join(o.ClaudeConfigDir, claudeStateFile))["projects"].(map[string]any)
	if len(projects) != 1 || projects[o.project].(map[string]any)["hasTrustDialogAccepted"] != true {
		t.Fatalf("trust must cover exactly %s: %v", o.project, projects)
	}
	marker, _ := verifyClaudeConfigDir(o.ClaudeConfigDir)
	if marker.Initialized[0].Workspace != o.project {
		t.Fatal("trust provenance must name the workspace")
	}
}

func TestRepeatLaunchIsIdempotentAndKeepsClaudeKeys(t *testing.T) {
	o := managedOptions(t, "theme", "trust")
	if err := ensureClaudeConfig(o, claudeTestTime); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(o.ClaudeConfigDir, claudeStateFile)
	state := readJSON(t, path)
	state["numStartups"] = 7.0
	state["theme"] = "light"
	state["projects"].(map[string]any)[o.project].(map[string]any)["allowedTools"] = []any{"Bash"}
	data, _ := json.Marshal(state)
	os.WriteFile(path, data, 0o600)
	for i := 0; i < 3; i++ {
		if err := ensureClaudeConfig(o, claudeTestTime.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	after := readJSON(t, path)
	if after["numStartups"] != 7.0 || after["theme"] != "light" ||
		after["projects"].(map[string]any)[o.project].(map[string]any)["allowedTools"] == nil {
		t.Fatalf("Claude-written keys were lost: %v", after)
	}
	marker, _ := verifyClaudeConfigDir(o.ClaudeConfigDir)
	if len(marker.Initialized) != 2 {
		t.Fatalf("repeat launches must not duplicate provenance: %+v", marker.Initialized)
	}
}

func TestRepeatNeverReappliesAnswersClaudeMigrated(t *testing.T) {
	// Job 4402: a repeat launch rewrote the marker. Once a part is initialized,
	// Claude owns the keys: a repeat leaves Claude's state and the marker byte-identical
	// even after Claude removes or migrates an answer.
	o := managedOptions(t, "theme", "trust", "bypass")
	if err := ensureClaudeConfig(o, claudeTestTime); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(o.ClaudeConfigDir, claudeStateFile)
	markerPath := filepath.Join(o.ClaudeConfigDir, claudeConfigMarker)
	state := readJSON(t, statePath)
	delete(state, "bypassPermissionsModeAccepted")
	delete(state, "theme")
	state["hasCompletedOnboarding"] = false
	delete(state["projects"].(map[string]any)[o.project].(map[string]any), "hasTrustDialogAccepted")
	data, _ := json.Marshal(state)
	os.WriteFile(statePath, data, 0o600)
	stateBefore, _ := os.ReadFile(statePath)
	markerBefore, _ := os.ReadFile(markerPath)
	for i := 0; i < 3; i++ {
		if err := ensureClaudeConfig(o, claudeTestTime.Add(time.Duration(i+1)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	stateAfter, _ := os.ReadFile(statePath)
	markerAfter, _ := os.ReadFile(markerPath)
	if string(stateAfter) != string(stateBefore) {
		t.Fatal("a repeat launch rewrote Claude's state")
	}
	if string(markerAfter) != string(markerBefore) {
		t.Fatal("a repeat launch rewrote the provenance marker")
	}
}

func TestThemeWrittenAloneIsRecordedOnceAcrossMigration(t *testing.T) {
	// Review 968: onboarding already complete but no theme. HRX writes the theme, so
	// the part must be recorded once, and a later Claude migration of the theme key
	// must not make the next init write it again.
	o := managedOptions(t)
	if err := ensureClaudeConfig(o, claudeTestTime); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(o.ClaudeConfigDir, claudeStateFile)
	markerPath := filepath.Join(o.ClaudeConfigDir, claudeConfigMarker)
	os.WriteFile(statePath, []byte(`{"hasCompletedOnboarding":true,"numStartups":1}`), 0o600)
	o.ClaudeInit = []string{"theme"}
	if err := ensureClaudeConfig(o, claudeTestTime); err != nil {
		t.Fatal(err)
	}
	state := readJSON(t, statePath)
	if state["theme"] != "dark" || state["numStartups"] != 1.0 {
		t.Fatalf("theme must be written and Claude keys kept: %v", state)
	}
	marker, _ := verifyClaudeConfigDir(o.ClaudeConfigDir)
	if len(marker.Initialized) != 1 || marker.Initialized[0].Part != "theme" {
		t.Fatalf("writing only the theme must record the part once: %+v", marker.Initialized)
	}
	// Claude migrates the theme key away; repeats must not write it again.
	delete(state, "theme")
	data, _ := json.Marshal(state)
	os.WriteFile(statePath, data, 0o600)
	stateBefore, _ := os.ReadFile(statePath)
	markerBefore, _ := os.ReadFile(markerPath)
	for i := 0; i < 3; i++ {
		if err := ensureClaudeConfig(o, claudeTestTime.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	stateAfter, _ := os.ReadFile(statePath)
	markerAfter, _ := os.ReadFile(markerPath)
	if string(stateAfter) != string(stateBefore) || string(markerAfter) != string(markerBefore) {
		t.Fatal("a migrated theme was written again or re-recorded")
	}
}

func TestThemeNeedingNothingIsANoOp(t *testing.T) {
	o := managedOptions(t)
	if err := ensureClaudeConfig(o, claudeTestTime); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(o.ClaudeConfigDir, claudeStateFile),
		[]byte(`{"hasCompletedOnboarding":true,"theme":"light"}`), 0o600)
	before := snapshotTree(t, o.ClaudeConfigDir)
	o.ClaudeInit = []string{"theme"}
	if err := ensureClaudeConfig(o, claudeTestTime); err != nil {
		t.Fatal(err)
	}
	if snapshotTree(t, o.ClaudeConfigDir) != before {
		t.Fatal("nothing to write must leave Claude's state and the marker untouched")
	}
}

func TestTrustForANewWorkspaceIsStillInitialized(t *testing.T) {
	o := managedOptions(t, "trust")
	if err := ensureClaudeConfig(o, claudeTestTime); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(o.project), "other")
	os.Mkdir(other, 0o700)
	other, _ = filepath.EvalSymlinks(other)
	second := o
	second.project = other
	if err := ensureClaudeConfig(second, claudeTestTime); err != nil {
		t.Fatal(err)
	}
	projects := readJSON(t, filepath.Join(o.ClaudeConfigDir, claudeStateFile))["projects"].(map[string]any)
	if len(projects) != 2 || projects[other].(map[string]any)["hasTrustDialogAccepted"] != true {
		t.Fatalf("trust for a new workspace must be initialized: %v", projects)
	}
	marker, _ := verifyClaudeConfigDir(o.ClaudeConfigDir)
	if len(marker.Initialized) != 2 || marker.Initialized[1].Workspace != other {
		t.Fatalf("each workspace's trust is recorded once: %+v", marker.Initialized)
	}
}

func TestConcurrentInitsLoseNoKeys(t *testing.T) {
	o := managedOptions(t)
	if err := ensureClaudeConfig(o, claudeTestTime); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			launch := o
			launch.ClaudeInit = [][]string{{"theme"}, {"trust"}, {"bypass"}}[i%3]
			errs <- ensureClaudeConfig(launch, claudeTestTime)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	state := readJSON(t, filepath.Join(o.ClaudeConfigDir, claudeStateFile))
	for _, key := range []string{"hasCompletedOnboarding", "projects", "bypassPermissionsModeAccepted"} {
		if _, ok := state[key]; !ok {
			t.Fatalf("a concurrent init lost %s: %v", key, state)
		}
	}
	marker, _ := verifyClaudeConfigDir(o.ClaudeConfigDir)
	if len(marker.Initialized) != 3 {
		t.Fatalf("each part must be recorded exactly once: %+v", marker.Initialized)
	}
}

func TestConcurrentCreationNeverAdoptsAForeignDirectory(t *testing.T) {
	o := managedOptions(t, "theme")
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- ensureClaudeConfig(o, claudeTestTime) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("racing HRX launches must agree on one created directory: %v", err)
		}
	}
	if _, err := verifyClaudeConfigDir(o.ClaudeConfigDir); err != nil {
		t.Fatal(err)
	}
}

func TestResumeRestoresAndRevalidatesTheManagedDirectory(t *testing.T) {
	o := managedOptions(t, "theme")
	saved := o.ClaudeConfigDir
	restored, err := restoreClaudeConfigDir(LaunchOptions{}, saved)
	if err != nil || restored.ClaudeConfigDir != saved {
		t.Fatalf("resume must restore the saved directory: %v %v", restored.ClaudeConfigDir, err)
	}
	if _, err := restoreClaudeConfigDir(LaunchOptions{ClaudeConfigDir: t.TempDir()}, saved); err == nil {
		t.Fatal("a different directory on resume must be refused")
	}
	if _, err := restoreClaudeConfigDir(LaunchOptions{ClaudeConfigDir: saved}, ""); err == nil {
		t.Fatal("a session launched without a managed directory must not move into one")
	}
	// The restored directory is verified again, not trusted from session history.
	os.Mkdir(saved, 0o700)
	restored.ClaudeInit = nil
	if err := ensureClaudeConfig(restored, claudeTestTime); err == nil {
		t.Fatal("a restored directory without provenance must be refused")
	}
}

func TestResumeNeverRecreatesAMissingManagedDirectory(t *testing.T) {
	o := managedOptions(t, "theme")
	restored, err := restoreClaudeConfigDir(LaunchOptions{project: o.project}, o.ClaudeConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	restored.ClaudeInit = []string{"theme"}
	if err := ensureClaudeConfig(restored, claudeTestTime); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("a missing session directory must be refused, got %v", err)
	}
	if _, err := os.Lstat(o.ClaudeConfigDir); !os.IsNotExist(err) {
		t.Fatal("resume recreated the session's managed directory")
	}
	// An intact one still works on resume.
	if err := ensureClaudeConfig(o, claudeTestTime); err != nil {
		t.Fatal(err)
	}
	if err := ensureClaudeConfig(restored, claudeTestTime); err != nil {
		t.Fatalf("an intact restored directory must validate: %v", err)
	}
	// An explicitly repeated, matching directory is also treated as restored.
	explicit, err := restoreClaudeConfigDir(LaunchOptions{ClaudeConfigDir: o.ClaudeConfigDir}, o.ClaudeConfigDir)
	if err != nil || !explicit.claudeConfigRestored {
		t.Fatalf("a matching explicit directory on resume must be marked restored: %v", err)
	}
}

func TestMarkerIsPublishedWholeWithNoTemporaryLeftovers(t *testing.T) {
	o := managedOptions(t, "theme", "trust")
	if err := ensureClaudeConfig(o, claudeTestTime); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(o.ClaudeConfigDir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".hrx-write-") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
		info, _ := entry.Info()
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s is not private: %v", entry.Name(), info.Mode().Perm())
		}
	}
	if _, err := verifyClaudeConfigDir(o.ClaudeConfigDir); err != nil {
		t.Fatal(err)
	}
	// Publishing never replaces an existing marker.
	if err := publishPrivateFile(filepath.Join(o.ClaudeConfigDir, claudeConfigMarker), []byte("{}")); err == nil {
		t.Fatal("publishing over an existing marker must fail")
	}
}

func TestManagedDirectoryReachesClaudeEnvironment(t *testing.T) {
	s := testSettings(t)
	s.Harness = "claude"
	o := managedOptions(t)
	launch, err := buildOtherLaunch(s, otherSnapshot("claude"), o, "key")
	if err != nil {
		t.Fatal(err)
	}
	if envMap(launch.Env)["CLAUDE_CONFIG_DIR"] != o.ClaudeConfigDir {
		t.Fatal("the managed directory must be the child's CLAUDE_CONFIG_DIR")
	}
	launch, err = buildOtherLaunch(s, otherSnapshot("claude"), LaunchOptions{}, "key")
	if err != nil {
		t.Fatal(err)
	}
	if _, set := envMap(launch.Env)["CLAUDE_CONFIG_DIR"]; set && os.Getenv("CLAUDE_CONFIG_DIR") == "" {
		t.Fatal("without the flag HRX must not choose a configuration directory")
	}
}
