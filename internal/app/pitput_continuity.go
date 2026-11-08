package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Transport configs belong to native sessions; notification state belongs to
// the enrolled identity and board. A private pointer also allows legacy state
// to remain in place, so migration never copies or invents an ACK.
func (r *pitputRuntime) bridgeState(root, board string) (string, error) {
	dir := filepath.Join(root, "boards", board)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	pointer := filepath.Join(dir, "state-location")
	if info, err := os.Lstat(pointer); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return "", errors.New("unsafe Pitput state-location")
		}
		data, err := os.ReadFile(pointer)
		if err != nil {
			return "", err
		}
		path := strings.TrimSpace(string(data))
		relative, err := filepath.Rel(root, path)
		if err != nil || !filepath.IsAbs(path) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", errors.New("invalid Pitput state-location")
		}
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	candidates, err := filepath.Glob(filepath.Join(root, "*", board, "state", fmt.Sprintf("%x", sha256.Sum256([]byte(r.identity))), "state.json"))
	if err != nil {
		return "", err
	}
	if len(candidates) > 1 {
		return "", fmt.Errorf("multiple legacy Pitput histories for %s; select the predecessor state_directory in %s before retrying (no cursor changed)", board, pointer)
	}
	state := filepath.Join(dir, "state")
	if len(candidates) == 1 {
		state = filepath.Dir(filepath.Dir(candidates[0]))
	}
	f, err := os.CreateTemp(dir, ".state-location-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(state + "\n")
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := os.Rename(f.Name(), pointer); err != nil {
		return "", err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return "", err
	}
	return state, nil
}

func (r *pitputRuntime) prepareBridge(ctx context.Context, config string, initializeOnly bool) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	args := []string{"bridge", "prepare", "--config", config, "--start", "latest"}
	if initializeOnly {
		args = append(args, "--initialize-only")
	}
	cmd := exec.CommandContext(ctx, r.cli, args...)
	var diagnostic limitedBuffer
	cmd.Stdout, cmd.Stderr = io.Discard, &diagnostic
	if err := cmd.Run(); err != nil {
		code := regexp.MustCompile(`(?m)^pitput: ([a-z_]{1,100})$`).FindStringSubmatch(diagnostic.String())
		detail := ""
		if len(code) == 2 {
			detail = " (" + code[1] + ")"
		}
		return fmt.Errorf("Pitput continuity preparation failed%s; install a Pitput version supporting bridge prepare, check connectivity and stopped predecessor ownership; no history is automatically acknowledged", detail)
	}
	return nil
}
