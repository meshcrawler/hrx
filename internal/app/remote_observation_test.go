package app

import (
	"context"
	"strings"
	"testing"

	"github.com/meshcrawler/hrx/internal/monitor"
)

func TestRemoteObservationRegistersAndStagesExternalWorker(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	id := "0123456789abcdef0123456789abcdef"
	steps := []string{
		`{"operation":"register","deployment":"` + id + `","host":"pve2","name":"ttrac-lab/pilot-r5/lead","identity":"laptop/ttrac-lab/lead","harness":"opencode","model":"or-acme/deepseek/deepseek-v4.1-flash-1017"}`,
		`{"operation":"stage","deployment":"` + id + `","stage":"ready","vmid":3004,"session":"ses_abc"}`,
	}
	for _, step := range steps {
		if err := runRemoteObservation(strings.NewReader(step)); err != nil {
			t.Fatal(err)
		}
	}
	store, _ := monitor.Default()
	rows, err := store.Snapshot(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatal(err, rows)
	}
	r := rows[0]
	if r.Host != "pve2" || r.Agent != "laptop/ttrac-lab/lead" || r.Remote == nil || r.Remote.Name != "ttrac-lab/pilot-r5/lead" || r.Remote.Stage != "ready" || r.Remote.VMID != 3004 || r.SessionID != "ses_abc" || r.Status != "unknown" {
		t.Fatalf("unexpected observation %+v %+v", r, r.Remote)
	}
	if err := runRemoteObservation(strings.NewReader(`{"operation":"stage","deployment":"` + id + `","stage":"destroyed"}`)); err != nil {
		t.Fatal(err)
	}
	if rows, _ = store.Snapshot(context.Background()); rows[0].Status != "exited" {
		t.Fatal("destroyed worker not exited", rows[0].Status)
	}
}

func TestRemoteObservationRejectsInvalidInput(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	id := "0123456789abcdef0123456789abcdef"
	for _, bad := range []string{
		`not json`,
		`{"operation":"delete","deployment":"` + id + `"}`,
		`{"operation":"register","deployment":"` + id + `","host":"pve2","name":"n","identity":"a/b/c","harness":"bash","model":"m"}`,
		`{"operation":"register","deployment":"short","host":"pve2","name":"n","identity":"a/b/c","harness":"opencode","model":"m"}`,
		`{"operation":"stage","deployment":"` + id + `","stage":"invented"}`,
		`{"operation":"stage","deployment":"` + id + `","stage":"ready","token":"secret"}`,
	} {
		if runRemoteObservation(strings.NewReader(bad)) == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	store, _ := monitor.Default()
	if rows, _ := store.Snapshot(context.Background()); len(rows) != 0 {
		t.Fatal("invalid input created an observation")
	}
}
