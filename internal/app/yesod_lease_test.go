package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeLeaseCLI(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "yesod"), []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestYesodLeaseTokensArePrivateAndFresh(t *testing.T) {
	first, err := newYesodLease("test/hrx/session")
	if err != nil {
		t.Fatal(err)
	}
	second, _ := newYesodLease(first.agent)
	if len(first.token) != 64 || first.token == second.token {
		t.Fatal("invalid ownership token generation")
	}
	if _, err := newYesodLease("bad address"); err == nil {
		t.Fatal("accepted invalid identity")
	}
	t.Setenv("YESOD_AGENT_IDENTITY", "parent/agent/name")
	t.Setenv("YESOD_MAIL_IDENTITY", "parent/agent/name")
	t.Setenv("YESOD_MAIL_SELF", "parent/agent/name")
	fakeLeaseCLI(t, `
[ -z "$YESOD_AGENT_IDENTITY$YESOD_MAIL_IDENTITY$YESOD_MAIL_SELF" ] || exit 1
[ "$1" = mail ] && [ "$3" = --as ] && [ "$4" = test/hrx/session ] && [ "$5" = --token-stdin ] || exit 1
read -r token
[ "${#token}" = 64 ] || exit 1
for arg in "$@"; do [ "$arg" != "$token" ] || exit 1; done
case "$2" in claim|renew|release) exit 0;; *) exit 1;; esac
`)
	if err := first.claim(context.Background(), "codex", "model"); err != nil {
		t.Fatal(err)
	}
	if err := first.renew(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := first.release(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestYesodLeaseErrorsAreRedacted(t *testing.T) {
	l, _ := newYesodLease("test/hrx/session")
	for _, code := range []string{"1", "2", "3"} {
		t.Run(code, func(t *testing.T) {
			fakeLeaseCLI(t, "read -r token\necho \"$token\"\necho \"$token\" >&2\nexit "+code+"\n")
			err := l.renew(context.Background())
			if err == nil || strings.Contains(err.Error(), l.token) {
				t.Fatal("lease failure missing or leaked token")
			}
			if errors.Is(err, errYesodLeaseLost) != (code == "3") {
				t.Fatal("incorrect ownership conflict classification")
			}
		})
	}
}

func TestYesodLeaseCallsHonorCancellation(t *testing.T) {
	fakeLeaseCLI(t, "exec /bin/sleep 30\n")
	l, _ := newYesodLease("test/hrx/session")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := l.renew(ctx); err == nil {
		t.Fatal("cancelled renew succeeded")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("cancelled lease operation hung")
	}
}
