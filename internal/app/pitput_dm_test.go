package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/meshcrawler/hrx/internal/session"
)

func TestPitputDirectMessageOptions(t *testing.T) {
	for args, want := range map[string]string{
		"--pitput":                  "",
		"--pitput --pitput-no-dm":   "off",
		"--pitput --pitput-dm-only": "only",
	} {
		o, err := ParseLaunch(strings.Fields(args))
		if err != nil || o.PitputDM != want {
			t.Fatal(args, o.PitputDM, err)
		}
	}
	for _, args := range []string{
		"--pitput-no-dm", "--pitput-dm-only", "--pitput --pitput-no-dm=1",
		"--pitput --pitput-no-dm --pitput-dm-only", "--pitput --pitput-dm-only --pitput-board dev",
		"--pitput -- --pitput-no-dm",
	} {
		if _, err := ParseLaunch(strings.Fields(args)); err == nil {
			t.Fatal("invalid direct-message options accepted:", args)
		}
	}
}

func TestPitputDirectMessageModeIsRestored(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	store := session.Store{Dir: filepath.Join(dir, "sessions")}
	if err := os.Mkdir(store.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	record := session.Record{NativeID: "12345678-1234-4123-8123-123456789abc", Harness: "codex", Identity: "fixture", Route: "fixture", Project: project, Pitput: true, AgentIdentity: "host/role/agent", PitputDM: "only"}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	o, _, err := restoreSession(store, "codex", project, LaunchOptions{Continue: true})
	if err != nil || o.PitputDM != "only" || !o.Pitput {
		t.Fatal(o, err)
	}
	o, _, err = restoreSession(store, "codex", project, LaunchOptions{Continue: true, PitputDM: "off"})
	if err != nil || o.PitputDM != "off" {
		t.Fatal("explicit flag must override the saved mode", o, err)
	}
}

// fakePitputCLI answers `agent status` with the given boards, `dm inbox` with dmExit,
// and records every invocation, so launches can be checked without a server.
func fakePitputCLI(t *testing.T, boards string, ready bool, dmExit int) string {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	status := `{"identity":"host/role/agent","boards":` + boards + `,"status":"ready"}`
	if !ready {
		status = `{}`
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n" +
		"case \"$*\" in\n" +
		"  'agent status'*) if [ -f " + dir + "/joined ]; then printf '%s' '{\"identity\":\"host/role/agent\",\"boards\":[],\"status\":\"ready\"}'; else printf '%s' '" + status + "'; fi ;;\n" +
		"  'join --dm-only'*) : > " + dir + "/joined ;;\n" +
		"  *' dm inbox --unhandled') exit " + string(rune('0'+dmExit)) + " ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "pitput"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return log
}

func calls(t *testing.T, log string) []string {
	data, _ := os.ReadFile(log)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestPitputDirectMessagesAreOnByDefaultAndDegradeSafely(t *testing.T) {
	launch := func(o LaunchOptions) (*pitputRuntime, error) {
		o.Pitput, o.AgentIdentity = true, "host/role/agent"
		l := &Launch{Executable: "codex"}
		err := preparePitputRuntime(context.Background(), o, l)
		return l.pitputRuntime, err
	}
	log := fakePitputCLI(t, `["dev"]`, true, 0)
	r, err := launch(LaunchOptions{})
	if err != nil || !r.dm || r.dmNotice != "" || !slices.Equal(r.boards, []string{"dev"}) || r.targets() != "dev, direct messages" {
		t.Fatal(r, err)
	}
	if !slices.Contains(calls(t, log), "--agent host/role/agent dm inbox --unhandled") {
		t.Fatal("capability was not probed read-only", calls(t, log))
	}
	r, err = launch(LaunchOptions{PitputDM: "off"})
	if err != nil || r.dm || r.targets() != "dev" {
		t.Fatal("opt-out ignored", r, err)
	}

	// An older server or CLI must never break existing board delivery.
	fakePitputCLI(t, `["dev"]`, true, 2)
	r, err = launch(LaunchOptions{})
	if err != nil || r.dm || !strings.Contains(r.dmNotice, "boards only") {
		t.Fatal("unsupported DMs did not degrade to boards", r, err)
	}
	if _, err = launch(LaunchOptions{PitputDM: "only"}); err == nil {
		t.Fatal("DM-only launch accepted without direct-message support")
	}

	// DM-only enrolls with no boards and launches no board bridges.
	log = fakePitputCLI(t, `[]`, false, 0)
	r, err = launch(LaunchOptions{PitputDM: "only"})
	if err != nil || !r.dm || len(r.boards) != 0 || r.targets() != "direct messages" {
		t.Fatal(r, err)
	}
	if !slices.Contains(calls(t, log), "join --dm-only --identity host/role/agent") {
		t.Fatal("DM-only enrollment not requested", calls(t, log))
	}
	fakePitputCLI(t, `[]`, true, 0)
	if _, err = launch(LaunchOptions{PitputDM: "off"}); err == nil {
		t.Fatal("launch with neither boards nor direct messages accepted")
	}
}

func readBridgeConfig(t *testing.T, path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := toml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatal("bridge config is not private")
	}
	return config
}

func TestPitputDirectMessageBridgeConfigs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", t.TempDir())
	r := pitputRuntime{identity: "host/role/agent", boards: []string{"dev"}, dm: true, record: session.Record{Project: root}}
	thread := "12345678-1234-4123-8123-123456789abc"
	paths, err := r.bridgeConfigs(root, "/bin/codex", "unix:///tmp/socket", thread)
	if err != nil || len(paths) != 2 {
		t.Fatal(paths, err)
	}
	board, direct := readBridgeConfig(t, paths[0]), readBridgeConfig(t, paths[1])
	agents := direct["agents"].([]any)
	entry := agents[0].(map[string]any)
	if len(agents) != 1 || entry["source"] != "dm" || entry["agent_id"] != r.identity || entry["thread_id"] != thread || entry["board"] != nil {
		t.Fatal("invalid DM bridge entry", entry)
	}
	if board["state_directory"] == direct["state_directory"] || !strings.Contains(paths[1], pitputDMKey) {
		t.Fatal("DM state must be separate from every board", board, direct)
	}
	if _, err := os.Stat(direct["codex_binary"].(string)); err != nil {
		t.Fatal("DM queue adapter missing", err)
	}
	// Resume keeps the same per-identity DM state, so the consumer session is reused.
	again, err := r.bridgeConfigs(root, "/bin/codex", "unix:///tmp/other", thread)
	if err != nil || readBridgeConfig(t, again[1])["state_directory"] != direct["state_directory"] {
		t.Fatal("resume moved DM state", err)
	}

	r.boards = nil
	opencode, err := r.openCodeBridgeConfigs(root, "http://127.0.0.1:4011", "/tmp/private/password", "ses_fixture")
	if err != nil || len(opencode) != 1 {
		t.Fatal(opencode, err)
	}
	config := readBridgeConfig(t, opencode[0])
	if config["backend"] != "opencode" || config["agents"].([]any)[0].(map[string]any)["source"] != "dm" {
		t.Fatal("invalid OpenCode DM bridge", config)
	}
	r.dm = false
	if none, _ := r.openCodeBridgeConfigs(root, "http://127.0.0.1:4011", "/tmp/private/password", "ses_fixture"); len(none) != 0 {
		t.Fatal("opt-out still configured a DM bridge")
	}
}

func TestPitputPromptExplainsDirectMessageDelivery(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for harness, want := range map[string]string{
		"codex":    "private direct messages unless launched with --pitput-no-dm",
		"opencode": "plus one for private direct messages",
		"claude":   "compact-task configurations",
	} {
		prompt := pitputPrompt(context.Background(), harness, "host/role/agent")
		if !strings.Contains(prompt, want) || !strings.Contains(prompt, "never bodies") {
			t.Fatal(harness, "prompt lacks direct-message guidance")
		}
	}
}

func TestPitputBridgeAdaptersRetainControllerSourceEndpoints(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", t.TempDir())
	r := pitputRuntime{identity: "host/role/agent", boards: []string{"dev"}, dm: true, record: session.Record{Project: root}}
	paths, err := r.bridgeConfigsForEndpoints(root, "/bin/codex", "unix:///private/board.sock", "unix:///private/dm.sock", "12345678-1234-4123-8123-123456789abc")
	if err != nil || len(paths) != 2 {
		t.Fatal(paths, err)
	}
	for i, want := range []string{"unix:///private/board.sock", "unix:///private/dm.sock"} {
		config := readBridgeConfig(t, paths[i])
		raw, err := os.ReadFile(config["codex_binary"].(string))
		if err != nil {
			t.Fatal(err)
		}
		expected := pitputQueueAdapter("/bin/codex", want)
		if string(raw) != expected {
			t.Fatal("adapter changed ingress source", i)
		}
	}
}
