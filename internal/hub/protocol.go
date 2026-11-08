// Package hub implements the optional shared HRX control plane. Local monitor
// storage and standalone harness launches have no dependency on this package.
package hub

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/meshcrawler/hrx/internal/monitor"
)

const Version = 1
const MaxBatch = 200
const MaxRequestBytes = 1024 * 1024

var (
	ErrUnauthorized = errors.New("hub authentication or identity scope denied")
	ErrConflict     = errors.New("hub ownership or sequence conflict")
	ErrInvalid      = errors.New("invalid hub request")
	ErrCapacity     = errors.New("hub snapshot capacity exceeded; archive history before retrying")
	identifier      = regexp.MustCompile(`^[a-f0-9]{32}$`)
	digestPattern   = regexp.MustCompile(`^[a-f0-9]{64}$`)
	agentPattern    = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	prefixPattern   = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*/(?:[A-Za-z0-9_-][A-Za-z0-9_.-]*/)?$`)
)

type Host struct {
	ID           string   `json:"host_id"`
	Name         string   `json:"name"`
	Prefixes     []string `json:"prefixes"`
	TokenDigest  string   `json:"-"`
	LastSequence int64    `json:"last_sequence"`
}

type Enrollment struct {
	Version     int      `json:"version"`
	HostID      string   `json:"host_id"`
	Name        string   `json:"name"`
	Prefixes    []string `json:"prefixes"`
	TokenDigest string   `json:"token_digest"`
}

type Batch struct {
	Version  int              `json:"version"`
	Sequence int64            `json:"sequence"`
	Records  []monitor.Record `json:"records"`
}

type LeaseRequest struct {
	Version    int    `json:"version"`
	Identity   string `json:"identity"`
	RunID      string `json:"run_id"`
	Generation int64  `json:"generation,omitempty"`
}

type Lease struct {
	Identity   string    `json:"identity"`
	HostID     string    `json:"host_id"`
	RunID      string    `json:"run_id"`
	Generation int64     `json:"generation"`
	ExpiresAt  time.Time `json:"expires_at"`
	ServerTime time.Time `json:"server_time"`
}

// OwnershipConflict identifies the current owner only after scope validation.
type OwnershipConflict struct {
	HostID     string    `json:"host_id"`
	RunID      string    `json:"run_id"`
	Generation int64     `json:"generation"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func (e *OwnershipConflict) Error() string {
	return fmt.Sprintf("identity owned by host %s, run %s (generation %d, expires %s)", e.HostID, e.RunID, e.Generation, e.ExpiresAt.UTC().Format(time.RFC3339))
}

func (e *OwnershipConflict) Unwrap() error { return ErrConflict }

func Digest(token string) string {
	d := sha256.Sum256([]byte(token))
	return hex.EncodeToString(d[:])
}

func (h Host) Allows(identity string) bool {
	if len(identity) > 256 || !agentPattern.MatchString(identity) {
		return false
	}
	for _, prefix := range h.Prefixes {
		if strings.HasPrefix(identity, prefix) {
			return true
		}
	}
	return false
}

func (e Enrollment) validate() error {
	if e.Version != Version || !identifier.MatchString(e.HostID) || !digestPattern.MatchString(e.TokenDigest) || !bounded(e.Name, 128) || len(e.Prefixes) == 0 || len(e.Prefixes) > 32 {
		return ErrInvalid
	}
	for _, p := range e.Prefixes {
		if len(p) > 200 || !prefixPattern.MatchString(p) {
			return ErrInvalid
		}
	}
	return nil
}

func bounded(s string, maximum int) bool {
	if len(s) == 0 || len(s) > maximum {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return r < 32 || r == 127 })
}

func (b Batch) validate(host Host) error {
	if b.Version != Version || b.Sequence < 1 || len(b.Records) < 1 || len(b.Records) > MaxBatch {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, r := range b.Records {
		if !identifier.MatchString(r.RunID) || seen[r.RunID] || r.HostID != host.ID || !host.Allows(r.Agent) {
			return ErrUnauthorized
		}
		seen[r.RunID] = true
		if r.Harness != "codex" && r.Harness != "claude" && r.Harness != "opencode" {
			return ErrInvalid
		}
		if r.Status != "running" && r.Status != "exited" && r.Status != "unknown" {
			return ErrInvalid
		}
		if !bounded(r.Model, 256) || r.StartedAt.IsZero() || r.LastActivity.IsZero() || r.LastSeen.IsZero() || r.PID < 0 || len(r.SessionID) > 256 {
			return ErrInvalid
		}
		if !monitor.ValidateMetrics(r.Metrics, r.SessionID, r.Harness) {
			return ErrInvalid
		}
		if !monitor.ValidatePitput(r.Pitput) {
			return ErrInvalid
		}
		if r.EndedAt != nil && r.Status != "exited" {
			return ErrInvalid
		}
		if !bounded(r.Activity, 64) || len(r.Credential.Scopes) > 32 {
			return ErrInvalid
		}
		for _, v := range []string{r.Credential.Provider, r.Credential.Host, r.Credential.Username, r.Credential.Profile, r.Credential.ExpiresAt, r.SessionID} {
			if v != "" && !bounded(v, 256) {
				return ErrInvalid
			}
		}
		for _, v := range r.Credential.Scopes {
			if !bounded(v, 64) {
				return ErrInvalid
			}
		}
	}
	return nil
}

func (r LeaseRequest) validate(host Host) error {
	if r.Version != Version || !identifier.MatchString(r.RunID) || r.Generation < 0 {
		return ErrInvalid
	}
	if !host.Allows(r.Identity) {
		return ErrUnauthorized
	}
	return nil
}
