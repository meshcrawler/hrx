package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// glab reads GITLAB_TOKEN from its environment on every run, and a running
// session's environment cannot be updated, so the launch token expires under
// long sessions. A private shim first on the child's PATH refreshes it per call.
func installGlabShim(env []string, executable string) ([]string, error) {
	path := environmentValue(env, "PATH")
	real, err := lookPathIn("glab", path)
	if err != nil {
		return env, nil // No glab to wrap; git still refreshes through its helper.
	}
	dir, err := os.MkdirTemp("", "hrx-glab-")
	if err != nil {
		return nil, errors.New("cannot create glab credential shim")
	}
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' _glab \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "glab"), []byte(script), 0700); err != nil {
		return nil, errors.New("cannot create glab credential shim")
	}
	return childEnvironment(env, map[string]string{
		"PATH": dir + string(os.PathListSeparator) + path, envGlabReal: real,
	}, nil), nil
}

// runGlabShim replaces itself with the real glab, using a fresh broker token for
// the launch identity when the broker answers and the launch token otherwise.
func runGlabShim(args []string) error {
	launch := currentGitlabLaunch()
	real := launch.RealGlab
	if !filepath.IsAbs(real) || filepath.Base(real) != "glab" {
		return errors.New("glab credential shim has no real glab")
	}
	env := os.Environ()
	if fresh := refreshedBrokerToken(launch); fresh != "" {
		env = childEnvironment(env, map[string]string{envGitlabToken: fresh}, nil)
	}
	return syscall.Exec(real, append([]string{"glab"}, args...), env)
}

func environmentValue(env []string, name string) string {
	value := ""
	for _, entry := range env {
		if key, v, ok := strings.Cut(entry, "="); ok && key == name {
			value = v
		}
	}
	return value
}

func lookPathIn(name, path string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0 {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}
