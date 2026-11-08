package monitor

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRemoteProgressDoesNotInventProcessHealth(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "monitor")}
	id := "0123456789abcdef0123456789abcdef"
	if err := s.RegisterRemote(id, "fixture-host", "network/worker", "fixture/worker/agent", "codex", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRemote(id, "ready", false, 3000, "native-session"); err != nil {
		t.Fatal(err)
	}
	before, err := s.RemoteRecord(id)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Snapshot(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	row := rows[0]
	if row.Status != "unknown" || row.PID != 0 || row.Remote.Stage != "ready" || row.Remote.VMID != 3000 || row.SessionID != "native-session" || !row.LastSeen.Equal(before.LastSeen) {
		t.Fatal("snapshot fabricated liveness", row)
	}
	if err := s.UpdateRemote(id, "verifying-readiness", true, 0, ""); err != nil {
		t.Fatal(err)
	}
	rows, err = s.Snapshot(context.Background())
	if err != nil || rows[0].Status != "unknown" || !rows[0].Remote.Failed {
		t.Fatal("observation failure became worker exit", rows, err)
	}
	if err := s.UpdateRemote(id, "destroyed", false, 0, ""); err != nil {
		t.Fatal(err)
	}
	rows, err = s.Snapshot(context.Background())
	if err != nil || rows[0].Status != "exited" || rows[0].Remote.Failed || rows[0].Remote.VMID != 3000 {
		t.Fatal(rows, err)
	}
	// A retry updates the same observation without losing its native binding.
	if err := s.RegisterRemote(id, "fixture-host", "network/worker", "fixture/worker/agent", "codex", "fixture"); err != nil {
		t.Fatal(err)
	}
	after, err := s.RemoteRecord(id)
	if err != nil || after.SessionID != "native-session" || after.Remote.Stage != "destroyed" {
		t.Fatal(after, err)
	}
	if err := s.RegisterRemote(id, "other-host", "network/worker", "fixture/worker/agent", "codex", "fixture"); err == nil {
		t.Fatal("adopted different host")
	}
	if err := s.UpdateRemote(id, "arbitrary secret text", true, 0, ""); err == nil {
		t.Fatal("accepted unclassified failure text")
	}
}

func TestOlderLocalCollectorCannotOverwriteRemoteProgress(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "monitor")}
	id := "abcdef0123456789abcdef0123456789"
	if err := s.RegisterRemote(id, "host", "worker", "host/worker/agent", "codex", "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRemote(id, "bootstrapping", false, 3000, ""); err != nil {
		t.Fatal(err)
	}
	// Older collectors only read and rewrite top-level process records. Simulate
	// one retaining a pre-release record under the same deployment identifier.
	if err := s.update(id, func(r *Record) { *r = Record{RunID: id, HostID: "proxmox:host", Host: "host", Status: "exited"} }); err != nil {
		t.Fatal(err)
	}
	before, err := s.RemoteRecord(id)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Snapshot(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Remote == nil || rows[0].Remote.Stage != "bootstrapping" || rows[0].Status != "unknown" || !rows[0].LastSeen.Equal(before.LastSeen) {
		t.Fatal("older record replaced or duplicated remote evidence", rows, err)
	}
}
