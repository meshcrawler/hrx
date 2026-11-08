package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"unicode"

	"github.com/meshcrawler/hrx/internal/catalog"
	"github.com/meshcrawler/hrx/internal/session"
)

var profileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)
var profileRoutePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,239}$`)

func profilePath(path, dir string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("cannot resolve credential home")
		}
		path = filepath.Join(home, path[2:])
	}
	if path == "" || strings.ContainsAny(path, "\x00\r\n") || strings.HasPrefix(path, "~") {
		return "", errors.New("invalid credential reference")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return filepath.Clean(path), nil
}

// Deny symlinks at the final component and inspect the opened descriptor, not a
// pre-open stat. Parent directory symlinks are allowed (e.g. a user's config home).
func readProfileCredential(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", errors.New("credential file unavailable; require a private regular file, not a symlink")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 65536 {
		return "", errors.New("credential file must be owner-only, regular and at most 64 KiB")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return "", errors.New("credential file must be owned by the current user")
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	var value struct {
		Key string `json:"key"`
	}
	if err != nil || len(data) > 65536 || json.Unmarshal(data, &value) != nil || value.Key == "" || strings.ContainsFunc(value.Key, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", errors.New("credential file must contain a nonempty JSON key field (contents suppressed)")
	}
	return value.Key, nil
}

func profileDefinition(name string) (*session.LaunchProfile, error) {
	return profileDefinitionMode(name, true)
}

func profileDefinitionMode(name string, requireCredential bool) (*session.LaunchProfile, error) {
	c, dir, err := loadUserConfig()
	if err != nil {
		return nil, err
	}
	if !profileNamePattern.MatchString(name) {
		return nil, errors.New("invalid profile name")
	}
	raw, ok := c.Profiles[name]
	if !ok {
		return nil, fmt.Errorf("unknown HRX profile %q", name)
	}
	fields, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("profile must be a table")
	}
	values := map[string]string{}
	for key, value := range fields {
		if !slices.Contains([]string{"harness", "model", "reasoning", "credential_file"}, key) {
			return nil, fmt.Errorf("profile %s has unsupported field %q", name, key)
		}
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("profile field %s must be a string", key)
		}
		values[key] = text
	}
	p := &session.LaunchProfile{Name: name, Harness: values["harness"], Model: values["model"], Reasoning: values["reasoning"]}
	if !slices.Contains([]string{"codex", "claude", "opencode"}, p.Harness) {
		return nil, errors.New("profile harness must be codex, claude or opencode")
	}
	if !profileRoutePattern.MatchString(p.Model) {
		return nil, errors.New("profile model must be an exact gateway route")
	}
	if strings.ContainsAny(p.Reasoning, " \r\n\t") {
		return nil, errors.New("invalid profile reasoning")
	}
	if requireCredential || values["credential_file"] != "" {
		p.CredentialFile, err = profilePath(values["credential_file"], dir)
		if err != nil {
			return nil, err
		}
	}
	profileFingerprint(p)
	return p, nil
}

func profileFingerprint(p *session.LaunchProfile) {
	data, _ := json.Marshal([]string{p.Name, p.Harness, p.Model, p.Reasoning, p.CredentialFile})
	p.Fingerprint = fmt.Sprintf("%x", sha256.Sum256(data))
}

func resolveProfileOptions(harness string, o LaunchOptions) (LaunchOptions, error) {
	if o.ProfileName == "" {
		return o, nil
	}
	p, err := profileDefinition(o.ProfileName)
	if err != nil {
		return o, err
	}
	if harness != p.Harness {
		return o, errors.New("explicit harness conflicts with HRX profile")
	}
	o.profile = p
	o.provenance = map[string]string{"harness": "profile", "model": "profile", "reasoning": "profile", "credential_file": "profile"}
	if o.Model == "" {
		o.Model = p.Model
	} else {
		o.provenance["model"] = "cli"
	}
	if o.Reasoning == "" {
		o.Reasoning = p.Reasoning
		if p.Reasoning == "" {
			o.provenance["reasoning"] = "gateway-default"
		}
	} else {
		o.provenance["reasoning"] = "cli"
	}
	if o.CredentialFile == "" {
		o.CredentialFile = p.CredentialFile
	} else {
		o.provenance["credential_file"] = "cli"
	}
	return o, nil
}

func prepareLaunchOptions(harness string, args []string) (LaunchOptions, error) {
	o, err := parseLaunchFor(harness, args)
	if err != nil {
		return o, err
	}
	o, err = resolveProfileOptions(harness, o)
	if err != nil {
		return o, err
	}
	o, err = applyLaunchDefaults(o)
	if err != nil {
		return o, err
	}
	o, err = resolveLaunchSession(harness, o)
	if err != nil {
		return o, err
	}
	if harness == "claude" {
		// After defaults and session restore: o.Yolo is the effective permission mode.
		if o, err = validateClaudeConfigOptions(o, os.Getenv); err != nil {
			return o, err
		}
	}
	if o.CredentialFile != "" {
		_, dir, err := loadUserConfig()
		if err != nil {
			return o, err
		}
		o.CredentialFile, err = profilePath(o.CredentialFile, dir)
		if err != nil {
			return o, err
		}
		if _, err = readProfileCredential(o.CredentialFile); err != nil {
			return o, err
		}
	}
	return o, nil
}

func expandProfileCommand(args []string) ([]string, error) {
	if len(args) == 0 || !(args[0] == "--profile" || strings.HasPrefix(args[0], "--profile=")) {
		return args, nil
	}
	_, name, equal := strings.Cut(args[0], "=")
	if !equal {
		if len(args) < 2 {
			return nil, errors.New("--profile requires a name")
		}
		name = args[1]
	}
	p, err := profileDefinition(name)
	if err != nil {
		return nil, err
	}
	return append([]string{p.Harness}, args...), nil
}

// Profiles require exact-route matches, including CLI overrides; the regular
// catalog's friendly canonical-name fallback must never silently switch payer.
func validateProfileRoute(s Settings, snapshot catalog.Snapshot, o LaunchOptions) error {
	if o.profile == nil && o.CredentialFile == "" {
		return nil
	}
	var selected *catalog.Model
	harness := s.Harness
	if harness == "" {
		harness = "codex"
	}
	var candidates []string
	for _, m := range snapshot.Models {
		if m.ID == o.Model {
			candidates = append(candidates, m.Route)
		}
		if m.Route == o.Model {
			copy := m
			selected = &copy
		}
	}
	if selected == nil {
		if len(candidates) > 0 {
			return fmt.Errorf("profile model must be an exact route; candidates: %s", strings.Join(candidates, ", "))
		}
		for _, entry := range snapshot.Inventory {
			if entry.Route == o.Model {
				return fmt.Errorf("exact route %s: %s", o.Model, entry.Status(harness))
			}
		}
		return errors.New("exact route missing from fresh gateway catalog")
	}
	if _, ok := selected.Harnesses[harness]; !ok {
		return errors.New("route lacks declared harness capability")
	}
	if selected.Context <= 0 || selected.Output <= 0 {
		return errors.New("route input/output limits are unknown")
	}
	if harness == "codex" {
		_, _, err := codexCatalogContent(s, snapshot)
		return err
	}
	return nil
}

func attachProfile(o LaunchOptions, l *Launch) {
	if o.profile == nil && o.CredentialFile == "" {
		return
	}
	p := session.LaunchProfile{Harness: l.Executable}
	if o.profile != nil {
		p = *o.profile
	}
	p.Model, p.Reasoning, p.CredentialFile = l.Model, l.Reasoning, o.CredentialFile
	profileFingerprint(&p)
	l.Profile = &p
	l.Provenance = o.provenance
}

type profileValidation struct {
	Name      string                 `json:"name"`
	Profile   *session.LaunchProfile `json:"profile,omitempty"`
	Status    string                 `json:"status"`
	Checks    string                 `json:"checks"`
	Error     string                 `json:"error,omitempty"`
	LiveProof string                 `json:"live_proof"`
}

func runProfiles(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: hrx profiles list|show NAME|validate NAME [--all] [--offline] [--json]")
	}
	command := args[0]
	asJSON, offline, all := false, false, false
	name := ""
	for _, a := range args[1:] {
		switch a {
		case "--json":
			asJSON = true
		case "--offline":
			offline = true
		case "--all":
			all = true
		default:
			if name != "" || strings.HasPrefix(a, "-") {
				return errors.New("invalid profiles arguments")
			}
			name = a
		}
	}
	c, _, err := loadUserConfig()
	if err != nil {
		return err
	}
	names := []string{}
	if command == "list" || all {
		for n := range c.Profiles {
			names = append(names, n)
		}
		sort.Strings(names)
	} else if name != "" {
		names = []string{name}
	}
	if command == "list" {
		if name != "" || all || offline {
			return errors.New("profiles list accepts only --json")
		}
		if asJSON {
			return json.NewEncoder(out).Encode(names)
		}
		for _, n := range names {
			fmt.Fprintln(out, n)
		}
		return nil
	}
	if command != "show" && command != "validate" || name == "" && !all || name != "" && all || command == "show" && (all || offline) {
		return errors.New("usage: hrx profiles show NAME | validate NAME|--all [--offline] [--json]")
	}
	results := []profileValidation{}
	publicCatalogs := map[string]catalog.Dashboard{}
	failed := false
	for _, n := range names {
		r := profileValidation{Name: n, Status: "pass", Checks: "configuration only; credentials and authorization not attempted", LiveProof: "not attempted"}
		p, e := profileDefinition(n)
		r.Profile = p
		if e == nil && command == "validate" {
			r.Checks = "offline configuration and credential file; online authorization not attempted"
			_, e = readProfileCredential(p.CredentialFile)
			if e == nil && !offline {
				r.Checks = "fresh catalog and key authorization; declared compatibility only"
				var s Settings
				s, e = loadHarnessSettings(p.Harness, p.CredentialFile)
				if e == nil {
					var snapshot catalog.Snapshot
					var key string
					snapshot, key, e = refreshWithCatalogCache(ctx, s, publicCatalogs)
					o := LaunchOptions{Model: p.Model, Reasoning: p.Reasoning, CredentialFile: p.CredentialFile, profile: p, DryRun: true}
					if e == nil {
						e = validateProfileRoute(s, snapshot, o)
					}
					if e == nil {
						if p.Harness == "codex" {
							_, e = BuildLaunch(s, snapshot, o)
						} else {
							_, e = buildOtherLaunch(s, snapshot, o, key)
						}
					}
				}
			}
		}
		if e != nil {
			r.Status = "fail"
			r.Error = e.Error()
			failed = true
		}
		results = append(results, r)
	}
	if asJSON {
		if err := json.NewEncoder(out).Encode(results); err != nil {
			return err
		}
	} else {
		for _, r := range results {
			fmt.Fprintf(out, "%s: %s (%s)\n", r.Name, r.Status, r.Checks)
			if r.Profile != nil {
				fmt.Fprintf(out, "  harness=%s model=%s reasoning=%s credential_file=%s\n", r.Profile.Harness, r.Profile.Model, r.Profile.Reasoning, r.Profile.CredentialFile)
			}
			if r.Error != "" {
				fmt.Fprintln(out, "  "+r.Error)
			}
		}
	}
	if failed {
		return errors.New("profile validation failed (see per-profile results)")
	}
	return nil
}
