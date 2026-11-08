package backend

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteCommandPreservesArguments(t *testing.T) {
	args := []string{"printf", "%s\\n", "", "with space", "quote'and\"double", "$(touch /tmp/hrx-unwanted)", "`echo bad`", "line\nbreak"}
	command, err := remoteCommand(args)
	if err != nil {
		t.Fatal(err)
	}
	got, err := exec.Command("sh", "-c", command).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != strings.Join(args[2:], "\n")+"\n" {
		t.Fatalf("arguments changed: %q", got)
	}
}

func fakeSSH(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSSHPrivateInputAndFailure(t *testing.T) {
	binary := fakeSSH(t, `[ "$1" = -o ] && [ "$2" = BatchMode=yes ] || exit 99
cat >&2
printf 'private-response'
exit 7
`)
	got, err := (SSH{Binary: binary}).Run(context.Background(), "pve1", []string{"worker"}, strings.NewReader("private-token"))
	if got != nil || err == nil || !strings.Contains(err.Error(), "exit 7") || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe failure: %q %v", got, err)
	}
}

func TestSSHRejectsIncompleteOutput(t *testing.T) {
	binary := fakeSSH(t, "printf 123456789\n")
	got, err := (SSH{Binary: binary, MaxOutput: 4}).Run(context.Background(), "user@host", []string{"observe"}, nil)
	if got != nil || err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("accepted truncated observation: %q %v", got, err)
	}
}

func TestSSHValidationAndCancellation(t *testing.T) {
	for _, host := range []string{"-oProxyCommand=bad", "host;bad", "host\nother", ""} {
		if _, err := (SSH{}).Run(context.Background(), host, []string{"true"}, nil); err == nil {
			t.Fatalf("accepted host %q", host)
		}
	}
	if _, err := remoteCommand([]string{"bad\x00command"}); err == nil {
		t.Fatal("accepted NUL")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (SSH{Binary: fakeSSH(t, "exit 0\n")}).Run(ctx, "host", []string{"true"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}
