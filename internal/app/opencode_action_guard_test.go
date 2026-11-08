package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/meshcrawler/hrx/internal/session"
)

func TestOpenCodeActionGuardInstallation(t *testing.T) {
	dir := t.TempDir()
	l := Launch{Env: []string{`OPENCODE_CONFIG_CONTENT={"plugins":["existing"],"agent":{"build":{"permission":{"shell":"ask"}}}}`, "HRX_OPENCODE_ACTION_GUARD=stale"}}
	if err := installOpenCodeActionGuard(&l, dir); err != nil {
		t.Fatal(err)
	}
	env := envMap(l.Env)
	if env["HRX_OPENCODE_ACTION_GUARD"] != "" {
		t.Fatal("inherited binding retained")
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(env["OPENCODE_CONFIG_CONTENT"]), &config); err != nil {
		t.Fatal(err)
	}
	plugins := config["plugins"].([]any)
	if len(plugins) != 2 || plugins[0] != "existing" || config["agent"] == nil {
		t.Fatal("existing configuration lost")
	}
	info, err := os.Stat(filepath.Join(plugins[1].(string), "server.mjs"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("plugin must be installed privately", err)
	}
	raw, _ := os.ReadFile(filepath.Join(plugins[1].(string), "server.mjs"))
	if string(raw) != string(openCodeActionGuard) {
		t.Fatal("wrong plugin bytes")
	}
	l.Env = []string{`OPENCODE_CONFIG_CONTENT={"plugins":false}`}
	if err := installOpenCodeActionGuard(&l, dir); err == nil {
		t.Fatal("invalid configuration accepted")
	}
}

func TestOpenCodeActionGuardBinding(t *testing.T) {
	root := t.TempDir()
	r := &pitputRuntime{identity: "host/worker/agent", boards: []string{"first", "second"}, dm: true, record: session.Record{NativeID: "ses_test", Project: root}}
	paths, err := r.openCodeBridgeConfigs(root, "http://127.0.0.1:1234", "/private/password", r.record.NativeID)
	if err != nil {
		t.Fatal(err)
	}
	l := Launch{Model: "custom/route", Args: []string{"--prompt", "authorized task"}, pitputRuntime: r}
	if err := bindOpenCodeActionGuard(&l, paths); err != nil {
		t.Fatal(err)
	}
	raw := envMap(l.Env)["HRX_OPENCODE_ACTION_GUARD"]
	var b struct {
		Identity, Session, Project, Model string
		States                            []string
		Initial                           bool `json:"initial_action"`
	}
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatal(err)
	}
	if len(b.States) != 3 || b.Session != "ses_test" || b.Identity != r.identity || b.Project != root || b.Model != l.Model || !b.Initial {
		t.Fatalf("wrong binding: %+v", b)
	}
	for _, state := range b.States {
		if filepath.Base(state) != "state.json" || !strings.HasPrefix(state, root+string(filepath.Separator)) {
			t.Fatal("wrong state file", state)
		}
	}
	if strings.Contains(raw, "password") {
		t.Fatal("credential path leaked into plugin binding")
	}
}
