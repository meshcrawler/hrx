// Package monitor contains secret-free, host-scoped agent observations.
package monitor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const RunEnv = "HRX_MONITOR_RUN"

type Credential struct {
	Provider  string   `json:"provider"`
	Host      string   `json:"host,omitempty"`
	Username  string   `json:"username,omitempty"`
	Profile   string   `json:"profile,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
	ExpiresAt string   `json:"expires_at,omitempty"`
}

type Record struct {
	Remote       *Remote         `json:"remote,omitempty"`
	RunID        string          `json:"run_id"`
	HostID       string          `json:"host_id"`
	Host         string          `json:"host"`
	Agent        string          `json:"agent"`
	Harness      string          `json:"harness"`
	Model        string          `json:"model"`
	SessionID    string          `json:"session_id,omitempty"`
	PID          int             `json:"pid,omitempty"`
	ProcessStart string          `json:"-"`
	StartedAt    time.Time       `json:"started_at"`
	EndedAt      *time.Time      `json:"ended_at,omitempty"`
	LastSeen     time.Time       `json:"last_seen"`
	LastActivity time.Time       `json:"last_activity"`
	Activity     string          `json:"activity"`
	Status       string          `json:"status"`
	Credential   Credential      `json:"credential"`
	Metrics      *SessionMetrics `json:"metrics,omitempty"`
	Pitput       *PitputInfo     `json:"pitput,omitempty"`
	Telemetry    *telemetryState `json:"-"`
}

type diskRecord struct {
	Record
	ProcessStart string          `json:"process_start"`
	Telemetry    *telemetryState `json:"telemetry,omitempty"`
}

type Store struct{ Dir string }

var runID = regexp.MustCompile(`^[a-f0-9]{32}$`)

func readPrivate(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 262144 {
		return nil, errors.New("invalid private monitor file")
	}
	b, err := io.ReadAll(io.LimitReader(f, 262145))
	if len(b) > 262144 {
		return nil, errors.New("monitor file too large")
	}
	return b, err
}

func Default() (Store, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Store{}, err
		}
		base = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(base) {
		return Store{}, errors.New("state directory must be absolute")
	}
	return Store{filepath.Join(base, "hrx", "monitor")}, nil
}

func randomID() (string, error) {
	var b [16]byte
	_, err := rand.Read(b[:])
	return hex.EncodeToString(b[:]), err
}

func (s Store) init() error {
	if !filepath.IsAbs(s.Dir) {
		return errors.New("monitor directory must be absolute")
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(s.Dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("monitor directory must be private")
	}
	return nil
}

func (s Store) HostID() (string, error) {
	if err := s.init(); err != nil {
		return "", err
	}
	path := filepath.Join(s.Dir, "host-id")
	id, err := randomID()
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		_, err = f.WriteString(id)
		closeErr := f.Close()
		if err != nil {
			return "", err
		}
		return id, closeErr
	}
	if !os.IsExist(err) {
		return "", err
	}
	// Another first launch may still be writing the host ID.
	for i := 0; i < 10; i++ {
		b, err := readPrivate(path)
		if err != nil {
			return "", err
		}
		if runID.Match(b) {
			return string(b), nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "", errors.New("invalid monitor host identity")
}

// ProcessTable uses OS start times to avoid treating a recycled PID as a live agent.
func ProcessTable(ctx context.Context) (map[int]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	b, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	result := map[int]string{}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 6 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err == nil {
			result[pid] = strings.Join(fields[1:], " ")
		}
	}
	return result, nil
}

func (s Store) Register(r Record) (Record, error) {
	id, err := s.HostID()
	if err != nil {
		return r, err
	}
	r.HostID = id
	r.RunID, err = randomID()
	if err != nil {
		return r, err
	}
	r.Host, _ = os.Hostname()
	r.PID = os.Getpid()
	processes, err := ProcessTable(context.Background())
	if err != nil {
		return r, err
	}
	r.ProcessStart = processes[r.PID]
	if r.ProcessStart == "" {
		return r, errors.New("cannot identify launcher process")
	}
	now := time.Now().UTC()
	r.StartedAt = now
	r.LastSeen = now
	r.LastActivity = now
	r.Activity = "launch"
	r.Status = "running"
	if r.Telemetry == nil {
		r.Telemetry = newTelemetryState(r.Harness, os.Environ())
	}
	return r, s.update(r.RunID, func(current *Record) { *current = r })
}

func (s Store) update(id string, change func(*Record)) error {
	if !runID.MatchString(id) {
		return errors.New("invalid monitor run ID")
	}
	if err := s.init(); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(s.Dir, id+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	info, err := lock.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("invalid monitor lock")
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	path := filepath.Join(s.Dir, id+".json")
	var disk diskRecord
	b, err := readPrivate(path)
	if err == nil {
		if json.Unmarshal(b, &disk) != nil {
			return errors.New("invalid monitor record")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	r := disk.Record
	r.ProcessStart = disk.ProcessStart
	r.Telemetry = disk.Telemetry
	change(&r)
	if r.RunID == "" {
		return nil
	}
	b, err = json.Marshal(diskRecord{r, r.ProcessStart, r.Telemetry})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, ".run-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func (s Store) Activity(id, event, session string) error {
	switch event {
	case "session-start", "user-message", "assistant-turn", "tool-use":
	default:
		return errors.New("unknown activity event")
	}
	return s.update(id, func(r *Record) {
		if r.RunID == "" || r.EndedAt != nil {
			return
		}
		now := time.Now().UTC()
		r.LastSeen = now
		r.LastActivity = now
		r.Activity = event
		if session != "" && len(session) <= 256 {
			r.SessionID = session
		}
	})
}

// RefreshedCredential records a refreshed broker token's expiry for the same
// GitLab host and account. It never adds a credential or changes its identity.
func (s Store) RefreshedCredential(id, host, username, expiresAt string) error {
	if _, err := time.Parse("2006-01-02", expiresAt); err != nil {
		return errors.New("invalid credential expiry")
	}
	return s.update(id, func(r *Record) {
		c := &r.Credential
		if r.RunID == "" || c.Provider != "gitlab" || c.Host != host || c.Username != username {
			return
		}
		c.ExpiresAt = expiresAt
	})
}

// Finish records an explicit end while the supervisor may still be alive.
// Retain history and credentials metadata, but never revive this run from a
// late hook or a PID that remains alive during teardown.
func (s Store) Finish(id string) error {
	return s.update(id, func(r *Record) {
		if r.RunID == "" || r.EndedAt != nil {
			return
		}
		now := time.Now().UTC()
		r.EndedAt, r.LastSeen, r.Status = &now, now, "exited"
	})
}

func (s Store) Snapshot(ctx context.Context) ([]Record, error) {
	if err := s.init(); err != nil {
		return nil, err
	}
	processes, processErr := ProcessTable(ctx)
	files, err := filepath.Glob(filepath.Join(s.Dir, "*.json"))
	if err != nil {
		return nil, err
	}
	result := []Record{}
	budget := int64(16 * 1024 * 1024)
	telemetryCtx, cancelTelemetry := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancelTelemetry()
	for _, path := range files {
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		if !runID.MatchString(id) {
			continue
		}
		var row Record
		err := s.update(id, func(r *Record) {
			if r.Remote != nil {
				row = *r
				return
			}
			if r.EndedAt != nil {
				r.Status = "exited"
			} else if processErr != nil {
				r.Status = "unknown"
			} else if processes[r.PID] != "" && processes[r.PID] == r.ProcessStart {
				r.Status = "running"
				r.LastSeen = time.Now().UTC()
			} else {
				r.Status = "exited"
			}
			collectTelemetry(telemetryCtx, r, &budget)
			row = *r
		})
		if err != nil {
			return nil, errors.New("cannot read monitor records")
		}
		if row.RunID != "" {
			result = append(result, row)
		}
	}
	remote, err := s.remoteSnapshot()
	if err != nil {
		return nil, err
	}
	// A pre-release dashboard may have consumed an old top-level remote record.
	// The isolated observation takes precedence, without altering local history.
	ids := map[string]bool{}
	for _, row := range remote {
		ids[row.RunID] = true
	}
	kept := result[:0]
	for _, row := range result {
		if !ids[row.RunID] {
			kept = append(kept, row)
		}
	}
	return append(kept, remote...), processErr
}
