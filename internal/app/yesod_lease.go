package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const yesodLeaseTTL = 180 * time.Second
const yesodLeaseCallTimeout = 10 * time.Second

var errYesodLeaseLost = errors.New("Yesod identity is owned by another session or its lease expired")

// The supervisor owns this value; never attach it to serializable launch or
// session metadata, or pass it into the harness environment.
type yesodLease struct {
	agent string
	token string
}

func newYesodLease(agent string) (*yesodLease, error) {
	if !yesodAddress.MatchString(agent) {
		return nil, errors.New("invalid Yesod lease address")
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, errors.New("cannot generate Yesod ownership token")
	}
	return &yesodLease{agent: agent, token: hex.EncodeToString(token[:])}, nil
}

func (l *yesodLease) claim(ctx context.Context, harness, model string) error {
	return l.call(ctx, "claim", "--ttl", "180", "--runtime", harness, "--model", model)
}

func (l *yesodLease) renew(ctx context.Context) error {
	return l.call(ctx, "renew", "--ttl", "180")
}

func (l *yesodLease) release() error {
	// Launch cancellation must not cancel the separate cleanup attempt.
	return l.call(context.Background(), "release")
}

func (l *yesodLease) call(ctx context.Context, operation string, extra ...string) error {
	ctx, cancel := context.WithTimeout(ctx, yesodLeaseCallTimeout)
	defer cancel()
	args := append([]string{"mail", operation, "--as", l.agent, "--token-stdin"}, extra...)
	cmd := exec.CommandContext(ctx, "yesod", args...)
	cmd.WaitDelay = time.Second
	cmd.Stdin = strings.NewReader(l.token + "\n")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.Env = childEnvironment(os.Environ(), map[string]string{"NO_COLOR": "1", "TERM": "dumb"},
		[]string{"YESOD_AGENT_IDENTITY", "YESOD_MAIL_IDENTITY", "YESOD_MAIL_SELF"})
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 3 {
			return errYesodLeaseLost
		}
		// Neither helper output nor exec errors are safe to forward: provider
		// configuration and database connection details can appear in them.
		return errors.New("Yesod lease " + operation + " failed; requires a reachable registry and claim/renew/release support")
	}
	return nil
}
