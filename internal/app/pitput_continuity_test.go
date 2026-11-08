package app

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/meshcrawler/hrx/internal/session"
)

func TestPitputStateSurvivesSuccessorAndHarnessSwitch(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	r := pitputRuntime{identity: "host/role/agent", boards: []string{"dev"}, record: session.Record{Project: root}}
	first, err := r.openCodeBridgeConfigs(root, "http://127.0.0.1:4011", "/tmp/password", "ses_first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.openCodeBridgeConfigs(root, "http://127.0.0.1:4012", "/tmp/password2", "ses_second")
	if err != nil {
		t.Fatal(err)
	}
	third, err := r.bridgeConfigs(root, "/tmp/codex", "unix:///tmp/socket", "12345678-1234-4123-8123-123456789abc")
	if err != nil {
		t.Fatal(err)
	}
	var state string
	for _, path := range []string{first[0], second[0], third[0]} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var config map[string]any
		if err := toml.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		actual := config["state_directory"].(string)
		if state != "" && actual != state {
			t.Fatal("native session changed notification state")
		}
		state = actual
	}
	if first[0] == second[0] {
		t.Fatal("successor overwrote predecessor ACK config")
	}
}

func legacyState(t *testing.T, root, native, identity string) string {
	t.Helper()
	state := filepath.Join(root, native, "dev", "state")
	target := filepath.Join(state, fmt.Sprintf("%x", sha256.Sum256([]byte(identity))))
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "state.json"), []byte("legacy fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestPitputStateAdoptsUniqueLegacyWithoutCopy(t *testing.T) {
	root := t.TempDir()
	r := pitputRuntime{identity: "host/role/agent"}
	old := legacyState(t, root, "ses_old", r.identity)
	got, err := r.bridgeState(root, "dev")
	if err != nil || got != old {
		t.Fatal(got, err)
	}
	// Later stale legacy artifacts cannot change the persisted selection.
	legacyState(t, root, "ses_other", r.identity)
	got, err = r.bridgeState(root, "dev")
	if err != nil || got != old {
		t.Fatal(got, err)
	}
}

func TestPitputStateRefusesAmbiguousLegacy(t *testing.T) {
	root := t.TempDir()
	r := pitputRuntime{identity: "host/role/agent"}
	legacyState(t, root, "ses_first", r.identity)
	legacyState(t, root, "ses_second", r.identity)
	if _, err := r.bridgeState(root, "dev"); err == nil {
		t.Fatal("ambiguous history silently selected")
	}
	if _, err := os.Stat(filepath.Join(root, "boards", "dev", "state-location")); !os.IsNotExist(err) {
		t.Fatal("selection persisted on ambiguity")
	}
}
