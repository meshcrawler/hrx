package monitor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Remote is a controller's last observation, not a heartbeat or a local PID.
// Keep native credentials, prompts and raw failure text out of this record.
type Remote struct {
	Name   string `json:"name"`
	Stage  string `json:"stage"`
	Failed bool   `json:"failed"`
	VMID   int    `json:"vmid,omitempty"`
}

func remoteStage(stage string) bool {
	switch stage {
	case "enrolling", "provisioning", "bootstrapping", "starting", "verifying-readiness", "ready", "releasing-work", "work-released", "stopping", "stopped", "resuming", "replacing-lease", "credential-installed", "destroying", "destroyed":
		return true
	}
	return false
}

func (s Store) RegisterRemote(id, host, name, agent, harness, model string) error {
	s = Store{Dir: filepath.Join(s.Dir, "remote")}
	if !runID.MatchString(id) || host == "" || name == "" || agent == "" {
		return errors.New("invalid remote observation")
	}
	var conflict bool
	err := s.update(id, func(r *Record) {
		if r.RunID != "" {
			conflict = r.Remote == nil || r.Host != host || r.Remote.Name != name || r.Agent != agent || r.Harness != harness || r.Model != model
			return
		}
		now := time.Now().UTC()
		*r = Record{RunID: id, HostID: "proxmox:" + host, Host: host, Agent: agent, Harness: harness, Model: model, StartedAt: now, LastSeen: now, LastActivity: now, Status: "unknown", Credential: Credential{Provider: "remote-lease"}, Remote: &Remote{Name: name}}
	})
	if conflict {
		return errors.New("remote observation identity conflict")
	}
	return err
}

// UpdateRemote never creates an agent from lifecycle metadata alone. Unknown
// legacy deployments remain untracked until a launch supplies their identity.
func (s Store) UpdateRemote(id, stage string, failed bool, vmid int, session string) error {
	s = Store{Dir: filepath.Join(s.Dir, "remote")}
	if !remoteStage(stage) {
		return errors.New("invalid remote stage")
	}
	return s.update(id, func(r *Record) {
		if r.Remote == nil {
			return
		}
		now := time.Now().UTC()
		r.Remote.Stage, r.Remote.Failed = stage, failed
		if vmid > 0 {
			r.Remote.VMID = vmid
		}
		if session != "" {
			r.SessionID = session
		}
		r.LastSeen, r.LastActivity, r.Activity = now, now, "remote-stage"
		r.Status = "unknown" // Never infer worker liveness from controller progress.
		if !failed && (stage == "stopped" || stage == "destroyed") {
			r.Status = "exited"
		}
	})
}

// RemoteRecord returns only an explicitly registered remote observation.
func (s Store) RemoteRecord(id string) (*Record, error) {
	if !runID.MatchString(id) {
		return nil, errors.New("invalid deployment ID")
	}
	raw, err := readPrivate(filepath.Join(s.Dir, "remote", id+".json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record Record
	if json.Unmarshal(raw, &record) != nil || record.RunID != id || record.Remote == nil {
		return nil, errors.New("invalid remote observation")
	}
	return &record, nil
}

func (s Store) remoteSnapshot() ([]Record, error) {
	paths, err := filepath.Glob(filepath.Join(s.Dir, "remote", "*.json"))
	if err != nil {
		return nil, err
	}
	rows := make([]Record, 0, len(paths))
	for _, path := range paths {
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		if !runID.MatchString(id) {
			continue
		}
		record, err := s.RemoteRecord(id)
		if err != nil {
			return nil, err
		}
		if record != nil {
			rows = append(rows, *record)
		}
	}
	return rows, nil
}

// RemoteRecords lists every explicitly registered remote observation.
func (s Store) RemoteRecords() ([]Record, error) { return s.remoteSnapshot() }

// RetireRemote marks a stale remote observation exited without claiming the
// container was stopped or destroyed. The original stage stays visible.
func (s Store) RetireRemote(id string) error {
	if !runID.MatchString(id) {
		return errors.New("invalid deployment ID")
	}
	s = Store{Dir: filepath.Join(s.Dir, "remote")}
	var missing bool
	err := s.update(id, func(r *Record) {
		if r.Remote == nil {
			missing = true
			return
		}
		now := time.Now().UTC()
		r.Status, r.Activity, r.LastSeen = "exited", "swept", now
		if r.EndedAt == nil {
			r.EndedAt = &now
		}
	})
	if missing {
		return errors.New("no remote observation with that deployment ID")
	}
	return err
}
