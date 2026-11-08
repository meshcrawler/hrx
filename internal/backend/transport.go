// Package backend implements remote execution without coupling it to a model or harness.
package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
)

// Transport executes an exact argument vector on a configured infrastructure host.
// Secret inputs belong on stdin, never in arguments or returned diagnostics.
type Transport interface {
	Run(context.Context, string, []string, io.Reader) ([]byte, error)
}

// SSH uses existing operator SSH configuration; it never copies SSH credentials
// to guests. Output is bounded and failure diagnostics omit remote output, which
// may contain credentials. Callers must request only sanitized observations.
type SSH struct {
	Binary    string
	MaxOutput int
}

var hostPattern = regexp.MustCompile(`^(?:[a-zA-Z0-9_][a-zA-Z0-9_.-]*@)?[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func remoteCommand(argv []string) (string, error) {
	if len(argv) == 0 || argv[0] == "" {
		return "", errors.New("remote command requires an executable")
	}
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		if strings.IndexByte(arg, 0) >= 0 {
			return "", errors.New("remote arguments cannot contain NUL")
		}
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
	}
	return strings.Join(quoted, " "), nil
}

func (s SSH) Run(ctx context.Context, host string, argv []string, input io.Reader) ([]byte, error) {
	if !hostPattern.MatchString(host) {
		return nil, errors.New("invalid SSH host; use a configured host alias or user@hostname")
	}
	command, err := remoteCommand(argv)
	if err != nil {
		return nil, err
	}
	binary := s.Binary
	if binary == "" {
		binary = "ssh"
	}
	limit := s.MaxOutput
	if limit == 0 {
		limit = 4 << 20
	}
	if limit < 1 {
		return nil, errors.New("remote output limit must be positive")
	}
	output := &boundedOutput{limit: limit}
	cmd := exec.CommandContext(ctx, binary, "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "--", host, command)
	cmd.Stdin = input
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("remote command interrupted: %w", ctx.Err())
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("remote command failed (exit %d); inspect protected host diagnostics", exit.ExitCode())
		}
		return nil, errors.New("cannot execute SSH transport")
	}
	if output.overflow {
		return nil, errors.New("remote output exceeded limit; observation is incomplete")
	}
	return output.buffer.Bytes(), nil
}

type boundedOutput struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.buffer.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	_, _ = b.buffer.Write(p)
	// Drain the process even when full, so a rejected observation cannot deadlock it.
	return n, nil
}
