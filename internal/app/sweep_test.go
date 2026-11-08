package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/monitor"
)

var sweepNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

type fakeSweepHost struct {
	guests   []sweepResource
	calls    []string
	failCall string
	// afterBackup lets a test retag the guest between backup and destroy.
	afterBackup func(*fakeSweepHost)
}

func (h *fakeSweepHost) Run(_ context.Context, host string, argv []string, _ io.Reader) ([]byte, error) {
	call := host + " " + strings.Join(argv, " ")
	if argv[0] != "pvesh" {
		h.calls = append(h.calls, call)
	}
	if h.failCall != "" && argv[0] == h.failCall {
		return nil, errors.New("remote command failed (exit 2)")
	}
	switch argv[0] {
	case "pvesh":
		if argv[2] == "/cluster/status" {
			local := strings.TrimPrefix(host, "root@")
			return json.Marshal([]map[string]any{
				{"type": "cluster", "name": "lab"},
				{"type": "node", "name": "pve1", "ip": "192.0.2.10", "local": b2i(local == "pve1")},
				{"type": "node", "name": "pve3", "ip": "192.0.2.12", "local": b2i(local == "pve3")},
				{"type": "node", "name": "pve2", "ip": "192.0.2.11", "local": b2i(local == "pve2")},
			})
		}
		return json.Marshal(h.guests)
	case "vzdump":
		if h.afterBackup != nil {
			h.afterBackup(h)
		}
	case "pct":
		if argv[1] == "destroy" {
			kept := h.guests[:0]
			for _, g := range h.guests {
				if strings.Fields(call)[3] != itoa(g.VMID) {
					kept = append(kept, g)
				}
			}
			h.guests = kept
		}
	}
	return []byte("{}"), nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func sweepGuests() []sweepResource {
	return []sweepResource{
		{VMID: 104, Name: "dashboard", Status: "running", Tags: "life-prod;owner-infra", Node: "pve2", Type: "lxc", Uptime: 900000},
		{VMID: 115, Name: "hrx-exp1-api", Status: "running", Tags: "hrx-experiment-1;life-experiment;owner-hrx;hrx-keep", Node: "pve1", Type: "lxc", Uptime: 900000},
		{VMID: 137, Name: "hrx-a095fe2ca5d1", Status: "stopped", Tags: "hrx-managed;hrx-native-acceptance", Node: "pve1", Type: "lxc"},
		{VMID: 711, Name: "hrx-hub-test", Status: "running", Tags: "life-experiment;owner-hrx", Node: "pve3", Type: "lxc", Uptime: 900000},
		{VMID: 723, Name: "gitlab-px-broker", Status: "running", Tags: "hrx-infra", Node: "pve3", Type: "lxc", Uptime: 900000},
		{VMID: 3004, Name: "hrx-c5dd410c3ee3", Status: "running", Tags: "hrx-managed", Node: "pve2", Type: "lxc", Uptime: 900000},
		{VMID: 3005, Name: "hrx-new", Status: "running", Tags: "hrx-managed", Node: "pve2", Type: "lxc", Uptime: 600},
		{VMID: 160, Name: "docs-runner", Status: "stopped", Tags: "life-prod;owner-docs", Node: "pve2", Type: "lxc"},
		{VMID: 900, Name: "hrx-vm", Status: "stopped", Tags: "owner-hrx", Node: "pve2", Type: "qemu"},
	}
}

func sweepStore(t *testing.T) monitor.Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "monitor")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return monitor.Store{Dir: dir}
}

func sweepRT(store monitor.Store, host *fakeSweepHost) sweepRuntime {
	return sweepRuntime{
		transport: host, client: http.DefaultClient,
		store: func() (monitor.Store, error) { return store, nil },
		now:   func() time.Time { return sweepNow }, errOut: io.Discard,
	}
}

func ids(findings []sweepFinding) []string {
	out := []string{}
	for _, f := range findings {
		out = append(out, f.ID)
	}
	return out
}

func TestPlanSweepOnlyTargetsUnprotectedHRXContainers(t *testing.T) {
	live := monitor.Record{RunID: strings.Repeat("a", 32), Status: "unknown", LastSeen: sweepNow.Add(-time.Hour), Remote: &monitor.Remote{Name: "ttrac", Stage: "ready", VMID: 3004}}
	got := ids(planSweep(sweepInputs{now: sweepNow, idle: 24 * time.Hour, local: []monitor.Record{live}, containers: sweepGuests()}))
	want := []string{"ct:137", "ct:711"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("findings = %v, want %v (prod, hrx-infra, hrx-keep, in-use, young, non-HRX and VMs must be skipped)", got, want)
	}
}

func TestPlanSweepRowsAndSources(t *testing.T) {
	stale := monitor.Record{RunID: strings.Repeat("b", 32), Host: "pve2", Status: "unknown", LastSeen: sweepNow.Add(-72 * time.Hour), Remote: &monitor.Remote{Name: "ttrac-lab/p/lead", Stage: "ready", VMID: 3010}}
	quiet := monitor.Record{RunID: strings.Repeat("c", 32), Host: "pve2", Status: "unknown", LastSeen: sweepNow.Add(-72 * time.Hour), Remote: &monitor.Remote{Name: "long", Stage: "ready", VMID: 3004}}
	gone := monitor.Record{RunID: strings.Repeat("d", 32), Host: "pve2", Status: "exited", LastSeen: sweepNow.Add(-72 * time.Hour), Remote: &monitor.Remote{Name: "done", Stage: "destroyed"}}
	snap := &monitor.Snapshot{Version: 1, Sources: []monitor.Source{
		{ID: "local", Status: "connected"},
		{ID: "hrx-exp1-agent-1", Status: "unreachable"},
		{ID: "laptop", Status: "unreachable", LastSuccess: sweepNow.Add(-time.Minute)},
		{ID: "old", Status: "unreachable", LastSuccess: sweepNow.Add(-50 * time.Hour)},
	}}
	findings := planSweep(sweepInputs{now: sweepNow, idle: 24 * time.Hour, local: []monitor.Record{stale, quiet, gone}, dashboard: snap, containers: sweepGuests()})
	got := ids(findings)
	// The quiet row on running CT 3004 is not retired; the container itself is
	// flagged instead, because only a human can tell an idle agent from a busy one.
	want := []string{"row:" + stale.RunID, "source:hrx-exp1-agent-1", "source:old", "ct:137", "ct:3004", "ct:711"}
	sortedWant := append([]string{}, want...)
	if strings.Join(got, ",") != strings.Join(sortStrings(sortedWant), ",") {
		t.Fatalf("findings = %v, want %v", got, sortedWant)
	}
	for _, f := range findings {
		if strings.HasPrefix(f.ID, "source:") && f.Applicable {
			t.Fatalf("%s must be advice only", f.ID)
		}
	}
}

func sortStrings(s []string) []string {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s
}

func TestSweepPlanChangesNothing(t *testing.T) {
	host := &fakeSweepHost{guests: sweepGuests()}
	var out bytes.Buffer
	if err := sweepRT(sweepStore(t), host).run(context.Background(), []string{"--host", "root@pve1"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(host.calls) != 0 {
		t.Fatalf("plan ran host commands: %v", host.calls)
	}
	for _, want := range []string{"plan only; nothing changed", "ct:137", "ct:711", "--host root@pve1 --apply ID"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("plan output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestSweepApplyBacksUpBeforeDestroyOnTheOwningNode(t *testing.T) {
	host := &fakeSweepHost{guests: sweepGuests()}
	var out bytes.Buffer
	if err := sweepRT(sweepStore(t), host).run(context.Background(), []string{"--host", "root@pve1", "--apply", "ct:711"}, &out); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"root@192.0.2.12 pct shutdown 711 --timeout 60 --forceStop 1",
		"root@192.0.2.12 vzdump 711 --storage local --mode stop --compress zstd --tmpdir /var/tmp",
		"root@192.0.2.12 pct destroy 711 --purge 1",
	}
	if strings.Join(host.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(host.calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestSweepNeverDestroysAfterFailedBackupOrRetag(t *testing.T) {
	host := &fakeSweepHost{guests: sweepGuests(), failCall: "vzdump"}
	err := sweepRT(sweepStore(t), host).run(context.Background(), []string{"--host", "root@pve1", "--apply", "ct:137"}, io.Discard)
	if err == nil || strings.Contains(strings.Join(host.calls, "\n"), "destroy") {
		t.Fatalf("err=%v calls=%v: a failed backup must stop the destroy", err, host.calls)
	}
	host = &fakeSweepHost{guests: sweepGuests(), afterBackup: func(h *fakeSweepHost) {
		for i := range h.guests {
			if h.guests[i].VMID == 137 {
				h.guests[i].Tags += ";hrx-keep"
			}
		}
	}}
	err = sweepRT(sweepStore(t), host).run(context.Background(), []string{"--host", "root@pve1", "--apply", "ct:137"}, io.Discard)
	if err == nil || strings.Contains(strings.Join(host.calls, "\n"), "destroy") {
		t.Fatalf("err=%v calls=%v: a container retagged hrx-keep must survive", err, host.calls)
	}
}

func TestSweepApplyRejectsUnknownAndAdviceIDsBeforeActing(t *testing.T) {
	host := &fakeSweepHost{guests: sweepGuests()}
	for _, id := range []string{"ct:104", "ct:115", "ct:723", "ct:9999"} {
		err := sweepRT(sweepStore(t), host).run(context.Background(), []string{"--host", "root@pve1", "--apply", "ct:137", "--apply", id}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "not in the current plan") {
			t.Fatalf("%s: err = %v", id, err)
		}
	}
	if len(host.calls) != 0 {
		t.Fatalf("an invalid ID still let earlier IDs act: %v", host.calls)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(monitor.Snapshot{Version: 1, GeneratedAt: sweepNow, Sources: []monitor.Source{{ID: "dead", Status: "unreachable"}}})
	}))
	defer srv.Close()
	err := sweepRT(sweepStore(t), host).run(context.Background(), []string{"--dashboard", srv.URL, "--apply", "source:dead"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "advice only") {
		t.Fatalf("source apply err = %v", err)
	}
}

func TestSweepRetiresStaleRowWithoutClaimingDestroyed(t *testing.T) {
	store := sweepStore(t)
	id := strings.Repeat("e", 32)
	if err := store.RegisterRemote(id, "pve2", "ttrac-lab/p/lead", "laptop/ttrac-lab/lead", "opencode", "m"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateRemote(id, "ready", false, 3010, "ses_x"); err != nil {
		t.Fatal(err)
	}
	rt := sweepRT(store, &fakeSweepHost{})
	rt.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	var out bytes.Buffer
	if err := rt.run(context.Background(), []string{"--apply", "row:" + id}, &out); err != nil {
		t.Fatal(err)
	}
	record, err := store.RemoteRecord(id)
	if err != nil || record.Status != "exited" || record.Remote.Stage != "ready" || record.Activity != "swept" || record.EndedAt == nil {
		t.Fatalf("record = %+v err=%v", record, err)
	}
}

func TestSweepRejectsUnsafeOptions(t *testing.T) {
	rt := sweepRT(sweepStore(t), &fakeSweepHost{})
	for _, args := range [][]string{
		{"--idle", "5m"},
		{"--backup-storage", "local;rm"},
		{"--dashboard", "http://dashboard.example.com/hrx"},
		{"extra"},
	} {
		if err := rt.run(context.Background(), args, io.Discard); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
}
