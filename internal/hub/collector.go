package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/meshcrawler/hrx/internal/monitor"
)

// Outbox holds one retryable batch plus fingerprints of accepted observations.
// It is a latest-observation stream, not an audit log of every tool/model event.
type outbox struct {
	Version      int               `json:"version"`
	NextSequence int64             `json:"next_sequence"`
	Pending      *Batch            `json:"pending,omitempty"`
	Sent         map[string]string `json:"sent"`
}

var ErrCollectorRunning = errors.New("HRX hub collector already running")

type Forwarder struct {
	Client  *Client
	Monitor monitor.Store
	Dir     string
	lock    *os.File
	state   outbox
	host    Host
}

func (f *Forwarder) Open(ctx context.Context) error {
	if err := os.MkdirAll(f.Dir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(f.Dir, "collector.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrCollectorRunning
		}
		return err
	}
	f.lock = lock
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	host, err := f.Client.Host(ctx)
	if err != nil {
		return err
	}
	localID, err := f.Monitor.HostID()
	if err != nil {
		return err
	}
	if localID != host.ID {
		return errors.New("hub credential belongs to a different host installation")
	}
	f.host = host
	f.state = outbox{Version: Version, NextSequence: host.LastSequence + 1, Sent: map[string]string{}}
	file, err := os.OpenFile(filepath.Join(f.Dir, "outbox.json"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err == nil {
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return errors.New("invalid private hub outbox")
		}
		data, err := io.ReadAll(io.LimitReader(file, 4*1024*1024+1))
		if err != nil || len(data) > 4*1024*1024 || json.Unmarshal(data, &f.state) != nil {
			return errors.New("invalid hub outbox")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if f.state.Version != Version || f.state.NextSequence < 1 || len(f.state.Sent) > 10000 {
		return errors.New("invalid hub outbox state")
	}
	if f.state.Pending != nil && (f.state.Pending.Sequence != f.state.NextSequence || f.state.Pending.validate(host) != nil) {
		return errors.New("invalid pending hub batch")
	}
	if f.state.Sent == nil {
		f.state.Sent = map[string]string{}
	}
	// A pending request may already be committed after a lost response. Any
	// other mismatch indicates restored/copied state and must not be replayed.
	expected := host.LastSequence + 1
	if f.state.NextSequence != expected && !(f.state.Pending != nil && f.state.NextSequence == host.LastSequence) {
		return errors.New("hub outbox sequence diverged; reconcile this host's local state")
	}
	ok = true
	return nil
}

func (f *Forwarder) Close() {
	if f.lock != nil {
		f.lock.Close()
		f.lock = nil
	}
}

func (f *Forwarder) save() error {
	data, err := json.Marshal(f.state)
	if err != nil || len(data) > 4*1024*1024 {
		return errors.New("hub outbox capacity exceeded")
	}
	file, err := os.CreateTemp(f.Dir, ".outbox-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), filepath.Join(f.Dir, "outbox.json")); err != nil {
		return err
	}
	dir, err := os.Open(f.Dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func observationDigest(r monitor.Record) string {
	data, _ := json.Marshal(r)
	return Digest(string(data))
}

func (f *Forwarder) deliver(ctx context.Context) error {
	if f.state.Pending == nil {
		return nil
	}
	// A previous save may have failed while leaving an in-memory batch. Do
	// not publish it until its exact retry payload is durable on this host.
	if err := f.save(); err != nil {
		return err
	}
	if err := f.Client.Call(ctx, "/api/v1/observations", f.state.Pending, nil); err != nil {
		return err
	}
	for _, r := range f.state.Pending.Records {
		f.state.Sent[r.RunID] = observationDigest(r)
	}
	f.state.NextSequence++
	f.state.Pending = nil
	return f.save()
}

func (f *Forwarder) Sync(ctx context.Context) error {
	if f.lock == nil {
		return errors.New("hub collector is not open")
	}
	rows, err := f.Monitor.Snapshot(ctx)
	if err != nil {
		return err
	}
	var live []string
	var selected []monitor.Record
	for _, r := range rows {
		if !f.host.Allows(r.Agent) {
			continue
		}
		selected = append(selected, r)
		if r.Status == "running" {
			live = append(live, r.RunID)
		}
	}
	if len(selected) > 10000 || len(live) > 1000 {
		return errors.New("hub collector capacity exceeded; archive old local runs")
	}
	// Presence is current local evidence, never a replayed batch's timestamp.
	if err = f.Client.Call(ctx, "/api/v1/heartbeat", map[string]any{"version": Version, "live_runs": live}, nil); err != nil {
		return err
	}
	if err = f.deliver(ctx); err != nil {
		return err
	}
	retained := make(map[string]bool, len(selected))
	for _, r := range selected {
		retained[r.RunID] = true
	}
	for id := range f.state.Sent {
		if !retained[id] {
			delete(f.state.Sent, id)
		}
	}
	if err = f.save(); err != nil {
		return err
	}
	// Exit records are forwarded first, retaining deterministic batch ordering.
	sort.Slice(selected, func(i, j int) bool {
		if (selected[i].Status == "exited") != (selected[j].Status == "exited") {
			return selected[i].Status == "exited"
		}
		return selected[i].RunID < selected[j].RunID
	})
	var changed []monitor.Record
	for _, r := range selected {
		if f.state.Sent[r.RunID] != observationDigest(r) {
			changed = append(changed, r)
		}
	}
	for len(changed) > 0 {
		batch, err := observationBatch(f.state.NextSequence, changed)
		if err != nil {
			return err
		}
		f.state.Pending = &batch
		if err = f.deliver(ctx); err != nil {
			return err
		}
		changed = changed[len(batch.Records):]
	}
	return nil
}

func observationBatch(sequence int64, records []monitor.Record) (Batch, error) {
	for n := min(len(records), MaxBatch); n > 0; n /= 2 {
		batch := Batch{Version: Version, Sequence: sequence, Records: records[:n]}
		data, err := json.Marshal(batch)
		if err != nil {
			return Batch{}, err
		}
		if len(data) <= MaxRequestBytes {
			batch.Records = append([]monitor.Record(nil), batch.Records...)
			return batch, nil
		}
	}
	return Batch{}, errors.New("hub observation exceeds request capacity")
}
