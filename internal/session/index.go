// Package session stores only the metadata needed to reconstruct an hrx launch.
package session

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

var ErrNotFound = errors.New("no matching hrx session metadata; use explicit launch settings for legacy sessions")

// Record deliberately has no arbitrary metadata, environment, prompt, token,
// or permission-bypass fields. NativeID must come from the harness contract.
type Record struct {
	Profile               *LaunchProfile `json:"launch_profile,omitempty"`
	NativeID              string         `json:"native_id"`
	Harness               string         `json:"harness"`
	Identity              string         `json:"model_identity"`
	Route                 string         `json:"model_route"`
	Reasoning             string         `json:"reasoning,omitempty"`
	Project               string         `json:"project"`
	Mail                  bool           `json:"yesod_mail"`
	BBS                   bool           `json:"yesod_bbs"`
	Agent                 string         `json:"yesod_agent,omitempty"`
	Plugin                string         `json:"plugin,omitempty"`
	AgentIdentity         string         `json:"agent_identity,omitempty"`
	Pitput                bool           `json:"pitput,omitempty"`
	PitputBoards          []string       `json:"pitput_boards,omitempty"`
	PitputCombinedPreview bool           `json:"pitput_combined_preview,omitempty"`
	PitputTargeted        bool           `json:"pitput_targeted,omitempty"`
	PitputFollowThreads   []string       `json:"pitput_follow_threads,omitempty"`
	PitputDM              string         `json:"pitput_dm,omitempty"`
	// ClaudeConfigDir is the canonical HRX-managed Claude configuration, if any.
	ClaudeConfigDir string    `json:"claude_config_dir,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// LaunchProfile is private resume metadata, never a credential or a hub payload.
type LaunchProfile struct {
	Name           string `json:"name"`
	Fingerprint    string `json:"fingerprint"`
	Harness        string `json:"harness"`
	Model          string `json:"model"`
	Reasoning      string `json:"reasoning"`
	CredentialFile string `json:"credential_file"`
}

type index struct {
	Version  int      `json:"version"`
	Sessions []Record `json:"sessions"`
}

type Store struct{ Dir string }

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,255}$`)
var address = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var brokerAgent = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,200}$`)

func valid(r Record) bool {
	if p := r.Profile; p != nil {
		if p.Harness != r.Harness || p.Model != r.Route || p.Reasoning != r.Reasoning || !filepath.IsAbs(p.CredentialFile) || len(p.Fingerprint) != 64 {
			return false
		}
		for _, value := range []string{p.Name, p.Fingerprint, p.CredentialFile} {
			if len(value) > 4096 || strings.ContainsFunc(value, func(c rune) bool { return c < 32 || c == 127 }) {
				return false
			}
		}
	}
	if r.Plugin != "" && (r.Plugin != "gitlab-service-account-broker" || !brokerAgent.MatchString(r.AgentIdentity)) || r.Plugin == "" && r.AgentIdentity != "" && !address.MatchString(r.AgentIdentity) {
		return false
	}
	if !identifier.MatchString(r.NativeID) || (r.Harness != "codex" && r.Harness != "claude" && r.Harness != "opencode") {
		return false
	}
	if r.Identity == "" || r.Route == "" || !filepath.IsAbs(r.Project) || filepath.Clean(r.Project) != r.Project {
		return false
	}
	for _, value := range []string{r.Identity, r.Route, r.Reasoning, r.Project} {
		if len(value) > 4096 || strings.ContainsFunc(value, func(c rune) bool { return c < 32 || c == 127 }) {
			return false
		}
	}
	return (r.Mail || r.BBS) && address.MatchString(r.Agent) || (!r.Mail && !r.BBS && r.Agent == "")
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
		return Store{}, errors.New("XDG_STATE_HOME must be absolute")
	}
	return Store{Dir: filepath.Join(base, "hrx", "sessions")}, nil
}

func privateFile(path string, flags int) (*os.File, error) {
	f, err := os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, errors.New("session index files must be private regular files")
	}
	return f, nil
}

func (s Store) locked(write bool, action func(*index) error) error {
	if !filepath.IsAbs(s.Dir) {
		return errors.New("session directory must be absolute")
	}
	if !write {
		if _, err := os.Lstat(s.Dir); os.IsNotExist(err) {
			return ErrNotFound
		}
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(s.Dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("session directory must be private and not a symlink")
	}
	lock, err := privateFile(filepath.Join(s.Dir, "index.lock"), os.O_CREATE|os.O_RDWR)
	if err != nil {
		return err
	}
	defer lock.Close()
	mode := syscall.LOCK_SH
	if write {
		mode = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(lock.Fd()), mode); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	data := index{Version: 1}
	f, err := privateFile(filepath.Join(s.Dir, "index.json"), os.O_RDONLY)
	if err == nil {
		defer f.Close()
		info, err := f.Stat()
		if err != nil || info.Size() > 4*1024*1024 {
			return errors.New("session index exceeds size limit")
		}
		decoder := json.NewDecoder(io.LimitReader(f, 4*1024*1024+1))
		decoder.DisallowUnknownFields()
		var extra any
		if decoder.Decode(&data) != nil || decoder.Decode(&extra) != io.EOF || data.Version != 1 {
			return errors.New("invalid or unsupported session index; refusing to overwrite")
		}
		seen := map[string]bool{}
		for _, r := range data.Sessions {
			key := r.Harness + "/" + r.NativeID
			if !valid(r) || seen[key] || r.CreatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) {
				return errors.New("invalid session record; refusing to overwrite index")
			}
			seen[key] = true
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return action(&data)
}

func (s Store) Save(r Record) error {
	return s.save(r, nil)
}

// List returns the existing launch history; it is not evidence of live processes.
func (s Store) List() ([]Record, error) {
	var result []Record
	err := s.locked(false, func(data *index) error { result = append([]Record{}, data.Sessions...); return nil })
	if errors.Is(err, ErrNotFound) {
		return []Record{}, nil
	}
	return result, err
}

// SaveAndStart holds the index lock across process creation. A returned startup
// error restores the prior index before readers or concurrent writers can see it.
// start must only create/exec the process, never wait for it or access this store.
// A successful exec closes the lock descriptor automatically (close-on-exec).
func (s Store) SaveAndStart(r Record, start func() error) error {
	if start == nil {
		return errors.New("missing session process starter")
	}
	return s.save(r, start)
}

func (s Store) save(r Record, start func() error) error {
	if !valid(r) {
		return errors.New("invalid session metadata")
	}
	return s.locked(true, func(data *index) error {
		priorIndex := index{Version: data.Version, Sessions: append([]Record(nil), data.Sessions...)}
		now := time.Now().UTC()
		r.CreatedAt, r.UpdatedAt = now, now
		found := false
		for i, prior := range data.Sessions {
			if prior.Harness == r.Harness && prior.NativeID == r.NativeID {
				if prior.Project != r.Project {
					return errors.New("session ID already belongs to another project")
				}
				r.CreatedAt = prior.CreatedAt
				if !now.After(prior.UpdatedAt) {
					r.UpdatedAt = prior.UpdatedAt.Add(time.Nanosecond)
				}
				data.Sessions[i], found = r, true
				break
			}
		}
		if !found {
			data.Sessions = append(data.Sessions, r)
		}
		sort.Slice(data.Sessions, func(i, j int) bool {
			a, b := data.Sessions[i], data.Sessions[j]
			return a.Harness+"/"+a.NativeID < b.Harness+"/"+b.NativeID
		})
		err := s.writeIndex(data)
		if start == nil {
			return err
		}
		if err == nil {
			err = start()
		}
		if err != nil {
			if rollbackErr := s.writeIndex(&priorIndex); rollbackErr != nil {
				return errors.New("session launch failed and prior index could not be restored")
			}
		}
		return err
	})
}

// Caller owns the exclusive index lock.
func (s Store) writeIndex(data *index) error {
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	if len(encoded) > 4*1024*1024 {
		return errors.New("session index exceeds size limit")
	}
	f, err := os.CreateTemp(s.Dir, ".index-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(encoded); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(s.Dir, "index.json")); err != nil {
		return err
	}
	dir, err := os.Open(s.Dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Resolve uses an exact native ID, or chooses the latest record for precisely
// this harness/project. Stable ID order breaks timestamp ties deterministically.
func (s Store) Resolve(harness, project, nativeID string) (Record, error) {
	var result Record
	err := s.locked(false, func(data *index) error {
		for _, r := range data.Sessions {
			if r.Harness != harness || r.Project != project || (nativeID != "" && r.NativeID != nativeID) {
				continue
			}
			if result.NativeID == "" || r.UpdatedAt.After(result.UpdatedAt) || (r.UpdatedAt.Equal(result.UpdatedAt) && r.NativeID > result.NativeID) {
				result = r
			}
		}
		if result.NativeID == "" {
			return ErrNotFound
		}
		return nil
	})
	return result, err
}
