package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestPitputCombinedPreviewRequiresOneConsumer(t *testing.T) {
	good := []string{"--pitput", "--pitput-board", "exchange", "--pitput-combined-preview"}
	options, err := ParseLaunch(good)
	if err != nil || !options.PitputCombinedPreview {
		t.Fatal(options, err)
	}
	for _, harness := range []string{"claude", "opencode"} {
		if _, err := parseLaunchFor(harness, good); err != nil {
			t.Fatal("supported harness rejected", harness, err)
		}
	}
	for _, args := range [][]string{
		{"--pitput-combined-preview"},
		append(append([]string{}, good...), "--pitput-no-dm"),
		append(append([]string{}, good...), "--pitput-board", "other"),
	} {
		if _, err := ParseLaunch(args); err == nil {
			t.Fatal("invalid combined selection accepted", args)
		}
	}
}

func TestPitputNativeCombinedConfigRetainsBackendAndGeneration(t *testing.T) {
	root := t.TempDir()
	id := "12345678-1234-4123-8123-123456789abc"
	r := &pitputRuntime{identity: "host/role/agent", boards: []string{"exchange"}, dm: true, combined: true}
	r.record.Project = root
	for _, backend := range []string{"opencode", "claude-task"} {
		var paths []string
		var err error
		if backend == "opencode" {
			paths, err = r.openCodeBridgeConfigs(root, "http://127.0.0.1:1234", "/private/password", "ses_fixture")
		} else {
			paths, err = r.claudeTaskConfigs(root, root, root, id)
		}
		if err != nil || len(paths) != 1 {
			t.Fatal(paths, err)
		}
		raw, err := os.ReadFile(paths[0])
		var config map[string]any
		if err != nil || toml.Unmarshal(raw, &config) != nil || config["backend"] != backend {
			t.Fatal("native backend lost", config, err)
		}
		agents := config["agents"].([]any)
		if len(agents) != 1 || agents[0].(map[string]any)["combined"] != true {
			t.Fatal("combined must own one worker", config)
		}
		if backend == "claude-task" && config["claude_task_generation"] != claudeTaskGeneration(root, id) {
			t.Fatal("Claude generation lost")
		}
		if backend == "claude-task" {
			next := "22345678-1234-4123-8123-123456789abc"
			successor, err := successorClaudeConfig(raw, r.identity, id, next)
			var retained map[string]any
			if err != nil || toml.Unmarshal(successor, &retained) != nil || retained["state_directory"] != config["state_directory"] || retained["agents"].([]any)[0].(map[string]any)["combined"] != true {
				t.Fatal("Claude successor lost shared selection/state", err)
			}
			prompt := claudeTaskPrompt("pitput", r.identity, id, "canonical attention protocol", paths)
			if !strings.Contains(prompt, paths[0]) || !strings.Contains(prompt, "explicit handling ACK") || !strings.Contains(prompt, "Task completion is not handling") {
				t.Fatal("combined task lost canonical read/ACK instruction")
			}
		}
		if _, exists := config["owned_admission_binary"]; exists {
			t.Fatal("native backend acquired Codex admission")
		}
	}
}

func TestPitputCombinedConfigUsesOneIdentityWorkerAndCurrentOwnedBinding(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "codex-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	runtime := &pitputRuntime{identity: "host/role/agent", boards: []string{"exchange"}, dm: true, combined: true, admissionBinary: "/fixture/hrx"}
	configs, err := runtime.combinedConfig(root, "/fixture/codex", "unix:///private/owned.sock", "12345678-1234-4123-8123-123456789abc", "binding")
	if err != nil || len(configs) != 1 {
		t.Fatal(configs, err)
	}
	raw, err := os.ReadFile(configs[0])
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Binding  string `toml:"owned_admission_binding"`
		Endpoint string `toml:"owned_admission_endpoint"`
		Agents   []struct {
			Combined bool   `toml:"combined"`
			Owner    string `toml:"agent_id"`
		} `toml:"agents"`
	}
	if toml.Unmarshal(raw, &config) != nil || len(config.Agents) != 1 || !config.Agents[0].Combined || config.Binding != "binding" || config.Endpoint != "unix:///private/owned.sock" {
		t.Fatal("combined config lost ownership")
	}
}
