package app

import (
	"context"
	"reflect"
	"testing"

	"github.com/meshcrawler/hrx/internal/monitor"
	"github.com/meshcrawler/hrx/internal/session"
)

func TestDashboardPitputLaunchMetadata(t *testing.T) {
	for _, tc := range []struct {
		name    string
		launch  Launch
		enabled bool
		state   string
	}{
		{"none", Launch{Executable: "codex"}, false, ""},
		{"configured", Launch{Executable: "claude", pendingSession: &session.Record{Pitput: true, AgentIdentity: "host/dev/agent", PitputBoards: []string{"dev", "ops"}}}, true, "configured"},
		{"joined", Launch{Executable: "codex", pitputRuntime: &pitputRuntime{identity: "host/dev/agent", boards: []string{"dev", "ops"}}}, true, "joined"},
		{"claude-hook", Launch{Executable: "claude", Pitput: true, Env: []string{sessionMetadataEnv + `={"pitput":true,"agent_identity":"host/dev/agent","pitput_boards":["dev","ops"]}`}}, true, "configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("HRX_MONITOR", "")
			trackLaunch(&tc.launch)
			s, _ := monitor.Default()
			rows, err := s.Snapshot(context.Background())
			if err != nil || len(rows) != 1 {
				t.Fatal("missing launch observation", err)
			}
			got := rows[0].Pitput
			want := monitor.NewPitputInfo(tc.enabled, "host/dev/agent", []string{"dev", "ops"}, tc.state)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("launch boards got %+v want %+v", got, want)
			}
		})
	}
}
