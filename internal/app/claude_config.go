package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// An HRX-managed Claude configuration directory is one HRX itself created with
// --claude-config-dir. Only such a directory may receive first-run state, and only
// the parts explicitly named with --claude-init. A user's own configuration
// (~/.claude or an existing CLAUDE_CONFIG_DIR) is never written.
const (
	claudeConfigMarker = ".hrx-claude-config.json"
	claudeConfigLock   = ".hrx-claude-config.lock"
	claudeStateFile    = ".claude.json"
)

var claudeInitParts = []string{"theme", "trust", "bypass"}

// errClaudeMarkerMissing is the only verification failure that a concurrent HRX
// launch, between creating the directory and writing its marker, can explain.
var errClaudeMarkerMissing = errors.New("--claude-config-dir exists but was not created by HRX; refusing to use it")

type claudeConfigProvenance struct {
	Schema      int                `json:"schema"`
	CreatedBy   string             `json:"created_by"`
	CreatedAt   time.Time          `json:"created_at"`
	UID         int                `json:"uid"`
	Initialized []claudeInitRecord `json:"initialized"`
}

// claudeInitRecord names one initialized first-run answer; trust records its workspace.
type claudeInitRecord struct {
	Part      string    `json:"part"`
	Workspace string    `json:"workspace,omitempty"`
	At        time.Time `json:"at"`
}

// parseClaudeInit accepts a comma list of distinct, known parts in any order.
func parseClaudeInit(value string) ([]string, error) {
	var parts []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if !slices.Contains(claudeInitParts, part) {
			return nil, fmt.Errorf("--claude-init takes a comma list of %s", strings.Join(claudeInitParts, ", "))
		}
		if slices.Contains(parts, part) {
			return nil, fmt.Errorf("duplicate --claude-init part %q", part)
		}
		parts = append(parts, part)
	}
	slices.SortFunc(parts, func(a, b string) int {
		return slices.Index(claudeInitParts, a) - slices.Index(claudeInitParts, b)
	})
	return parts, nil
}

// canonicalClaudeConfigDir resolves the parent's symlinks but never follows the
// final component: a symlink there is refused when the directory is verified.
func canonicalClaudeConfigDir(path string) (string, error) {
	if path == "" {
		return "", errors.New("--claude-config-dir requires a path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", errors.New("--claude-config-dir parent directory must exist")
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

// validateClaudeConfigOptions checks the request without touching the directory.
// It runs after defaults are applied, so o.Yolo is the effective permission mode.
func validateClaudeConfigOptions(o LaunchOptions, env func(string) string) (LaunchOptions, error) {
	if o.ClaudeConfigDir == "" {
		if len(o.ClaudeInit) > 0 {
			return o, errors.New("--claude-init requires --claude-config-dir; HRX never initializes an existing user configuration")
		}
		return o, nil
	}
	dir, err := canonicalClaudeConfigDir(o.ClaudeConfigDir)
	if err != nil {
		return o, err
	}
	o.ClaudeConfigDir = dir
	if existing := env("CLAUDE_CONFIG_DIR"); existing != "" {
		other, err := canonicalClaudeConfigDir(existing)
		if err != nil || other != dir {
			return o, errors.New("CLAUDE_CONFIG_DIR conflicts with --claude-config-dir; unset it or pass the same directory")
		}
	}
	if slices.Contains(o.ClaudeInit, "bypass") && !o.Yolo {
		return o, errors.New("--claude-init bypass requires the bypass permission mode (--yolo); it is never accepted otherwise")
	}
	if slices.Contains(o.ClaudeInit, "trust") && o.project == "" {
		return o, errors.New("--claude-init trust requires a resolved launch workspace")
	}
	return o, nil
}

// privateOwned checks an existing path: not a symlink, owned by this user, of the
// expected kind, and with no group or other access at all.
func privateOwned(info fs.FileInfo, dir bool) error {
	if info.Mode()&fs.ModeSymlink != 0 {
		return errors.New("is a symlink")
	}
	if info.IsDir() != dir || (!dir && !info.Mode().IsRegular()) {
		return errors.New("has an unexpected file type")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
		return errors.New("is not owned by this user")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("is accessible by group or others (requires 0700 directories and 0600 files)")
	}
	return nil
}

func readClaudeProvenance(dir string) (claudeConfigProvenance, error) {
	var marker claudeConfigProvenance
	path := filepath.Join(dir, claudeConfigMarker)
	info, err := os.Lstat(path)
	if err != nil {
		return marker, errClaudeMarkerMissing
	}
	if err := privateOwned(info, false); err != nil {
		return marker, fmt.Errorf("HRX Claude config marker %s", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return marker, errors.New("cannot read the HRX Claude config marker")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	malformed := errors.New("malformed HRX Claude config marker; refusing to use it")
	if decoder.Decode(&marker) != nil || marker.Schema != 1 || marker.CreatedBy != "hrx" ||
		marker.UID != os.Getuid() || marker.CreatedAt.IsZero() || marker.Initialized == nil {
		return marker, malformed
	}
	// Exactly one JSON value: trailing data or a second value is malformed.
	if _, err := decoder.Token(); err != io.EOF {
		return marker, malformed
	}
	for _, entry := range marker.Initialized {
		trust := entry.Part == "trust"
		if !slices.Contains(claudeInitParts, entry.Part) || entry.At.IsZero() ||
			trust != (entry.Workspace != "") || (trust && !filepath.IsAbs(entry.Workspace)) {
			return marker, malformed
		}
	}
	return marker, nil
}

// verifyClaudeConfigDir accepts only a private directory carrying a valid HRX marker.
func verifyClaudeConfigDir(dir string) (claudeConfigProvenance, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return claudeConfigProvenance{}, err
	}
	if err := privateOwned(info, true); err != nil {
		return claudeConfigProvenance{}, fmt.Errorf("--claude-config-dir %s", err)
	}
	return readClaudeProvenance(dir)
}

// writePrivateFile replaces path atomically with a 0600 file in the same directory.
func writePrivateFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".hrx-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err == nil {
		if _, err = f.Write(data); err == nil {
			err = f.Sync()
		}
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// ensureClaudeConfig creates (or re-verifies) the managed directory and applies the
// requested first-run parts under an exclusive lock. A dry run never writes.
func ensureClaudeConfig(o LaunchOptions, now time.Time) error {
	if o.ClaudeConfigDir == "" {
		return nil
	}
	dir := o.ClaudeConfigDir
	if o.DryRun {
		if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		_, err := verifyClaudeConfigDir(dir)
		return err
	}
	if o.claudeConfigRestored {
		// The native session lives in this directory: a missing one is never replaced.
		if _, err := verifyClaudeConfigDir(dir); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return errors.New("the session's HRX-managed Claude config directory is missing; refusing to recreate it")
			}
			return err
		}
	} else if err := createClaudeConfigDir(dir, now); err != nil {
		return err
	}
	if len(o.ClaudeInit) == 0 {
		return nil
	}
	lock, err := os.OpenFile(filepath.Join(dir, claudeConfigLock), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return errors.New("cannot open the HRX Claude config lock")
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return errors.New("cannot lock the HRX Claude config")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	// Re-read both files under the lock, so concurrent launches never lose keys.
	marker, err := verifyClaudeConfigDir(dir)
	if err != nil {
		return err
	}
	state, err := readClaudeState(dir)
	if err != nil {
		return err
	}
	changed := false
	record := func(part, workspace string) {
		changed = true
		marker.Initialized = append(marker.Initialized, claudeInitRecord{Part: part, Workspace: workspace, At: now.UTC()})
	}
	// Initialization happens once per part (and per workspace for trust). After that
	// Claude owns its configuration and may migrate or rewrite these keys; a repeat
	// launch never re-applies them, so it leaves both Claude's state and the marker
	// untouched. A first-run screen that reappears stays visible, never silently patched.
	initialized := func(part, workspace string) bool {
		return slices.ContainsFunc(marker.Initialized, func(r claudeInitRecord) bool {
			return r.Part == part && r.Workspace == workspace
		})
	}
	for _, part := range o.ClaudeInit {
		workspace := ""
		if part == "trust" {
			workspace = o.project
		}
		if initialized(part, workspace) {
			continue
		}
		switch part {
		case "theme":
			// Record the part exactly once whenever HRX writes either key, so a theme
			// value Claude later migrates is never written again. Nothing to write
			// stays a no-op.
			_, hasTheme := state["theme"]
			if state["hasCompletedOnboarding"] != true || !hasTheme {
				state["hasCompletedOnboarding"] = true
				if !hasTheme {
					state["theme"] = "dark"
				}
				record(part, "")
			}
		case "trust":
			projects, _ := state["projects"].(map[string]any)
			if projects == nil {
				if _, present := state["projects"]; present {
					return errors.New("Claude configuration has malformed projects; refusing to overwrite it")
				}
				projects = map[string]any{}
			}
			entry, _ := projects[o.project].(map[string]any)
			if entry == nil {
				if _, present := projects[o.project]; present {
					return errors.New("Claude configuration has a malformed project entry; refusing to overwrite it")
				}
				entry = map[string]any{}
			}
			if entry["hasTrustDialogAccepted"] != true {
				entry["hasTrustDialogAccepted"] = true
				projects[o.project] = entry
				state["projects"] = projects
				record(part, o.project)
			}
		case "bypass":
			if state["bypassPermissionsModeAccepted"] != true {
				state["bypassPermissionsModeAccepted"] = true
				record(part, "")
			}
		}
	}
	if !changed {
		return nil
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := writePrivateFile(filepath.Join(dir, claudeStateFile), append(data, '\n')); err != nil {
		return err
	}
	data, _ = json.MarshalIndent(marker, "", "  ")
	return writePrivateFile(filepath.Join(dir, claudeConfigMarker), append(data, '\n'))
}

// createClaudeConfigDir creates the managed directory and publishes a complete
// provenance marker atomically, so a racing launch sees either no marker (and
// retries) or a whole one, never a partial file. It never adopts a directory it
// did not create.
func createClaudeConfigDir(dir string, now time.Time) error {
	switch err := os.Mkdir(dir, 0o700); {
	case err == nil:
		marker := claudeConfigProvenance{Schema: 1, CreatedBy: "hrx", CreatedAt: now.UTC(), UID: os.Getuid(),
			Initialized: []claudeInitRecord{}}
		data, _ := json.MarshalIndent(marker, "", "  ")
		return publishPrivateFile(filepath.Join(dir, claudeConfigMarker), append(data, '\n'))
	case errors.Is(err, fs.ErrExist):
		// Another launch may be creating it: allow a moment for its marker to appear.
		// Any other failure (symlink, owner, mode, malformed marker) is final at once.
		_, verr := verifyClaudeConfigDir(dir)
		for attempt := 0; errors.Is(verr, errClaudeMarkerMissing) && attempt < 10; attempt++ {
			time.Sleep(100 * time.Millisecond)
			_, verr = verifyClaudeConfigDir(dir)
		}
		return verr
	default:
		return fmt.Errorf("cannot create --claude-config-dir: %w", err)
	}
}

// publishPrivateFile writes a complete 0600 file, then hard-links it into place,
// which fails rather than replaces if the path already exists.
func publishPrivateFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".hrx-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err == nil {
		if _, err = f.Write(data); err == nil {
			err = f.Sync()
		}
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Link(f.Name(), path)
}

// readClaudeState returns Claude's own state as a JSON object. A malformed or
// unsafe file fails without being overwritten.
func readClaudeState(dir string) (map[string]any, error) {
	path := filepath.Join(dir, claudeStateFile)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := privateOwned(info, false); err != nil {
		return nil, fmt.Errorf("Claude configuration %s %s; refusing to write it", claudeStateFile, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state map[string]any
	if json.Unmarshal(data, &state) != nil || state == nil {
		return nil, fmt.Errorf("malformed Claude configuration %s; refusing to overwrite it", claudeStateFile)
	}
	return state, nil
}

// restoreClaudeConfigDir keeps a resumed session on the managed directory it was
// launched with: a native session id only exists inside that configuration.
func restoreClaudeConfigDir(o LaunchOptions, saved string) (LaunchOptions, error) {
	if saved == "" {
		if o.ClaudeConfigDir != "" {
			return o, errors.New("this session was not launched with --claude-config-dir; resume it without one")
		}
		return o, nil
	}
	o.claudeConfigRestored = true
	if o.ClaudeConfigDir == "" {
		o.ClaudeConfigDir = saved
		return o, nil
	}
	dir, err := canonicalClaudeConfigDir(o.ClaudeConfigDir)
	if err != nil {
		return o, err
	}
	if dir != saved {
		return o, errors.New("--claude-config-dir differs from the directory this session was launched with")
	}
	return o, nil
}
