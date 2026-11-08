package app

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

//go:embed assets/opencode-action-guard.mjs
var openCodeActionGuard []byte

// Install from the binary, not from a mutable project or a network package.
// The caller owns this private directory and removes it after child processes exit.
func installOpenCodeActionGuard(l *Launch, dir string) error {
	var config map[string]any
	env := l.Env
	if env == nil {
		env = os.Environ()
	}
	raw := ""
	for _, item := range env {
		if value, ok := strings.CutPrefix(item, "OPENCODE_CONFIG_CONTENT="); ok {
			raw = value
		}
	}
	if raw == "" {
		raw = "{}"
	}
	if json.Unmarshal([]byte(raw), &config) != nil || config == nil {
		return errors.New("invalid OpenCode runtime configuration for action guard")
	}
	plugins, ok := config["plugins"].([]any)
	if config["plugins"] != nil && !ok {
		return errors.New("invalid OpenCode plugins configuration")
	}
	pluginDir := filepath.Join(dir, "hrx-action-guard")
	if err := os.MkdirAll(pluginDir, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "package.json"), []byte(`{"name":"hrx-action-guard","type":"module","exports":{".":"./server.mjs","./server":"./server.mjs"}}`), 0600); err != nil {
		return err
	}
	filename := filepath.Join(pluginDir, "server.mjs")
	if err := os.WriteFile(filename, openCodeActionGuard, 0600); err != nil {
		return err
	}
	config["plugins"] = append(plugins, pluginDir)
	data, err := json.Marshal(config)
	if err != nil {
		return err
	}
	l.Env = childEnvironment(env, map[string]string{"OPENCODE_CONFIG_CONTENT": string(data)}, []string{"HRX_OPENCODE_ACTION_GUARD"})
	return nil
}

func bindOpenCodeActionGuard(l *Launch, configs []string) error {
	r := l.pitputRuntime
	states := []string{}
	for _, filename := range configs {
		data, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		var config struct {
			State string `toml:"state_directory"`
		}
		if toml.Unmarshal(data, &config) != nil || !filepath.IsAbs(config.State) {
			return errors.New("invalid action guard bridge state binding")
		}
		states = append(states, filepath.Join(config.State, fmt.Sprintf("%x", sha256.Sum256([]byte(r.identity))), "state.json"))
	}
	initial := false
	for _, arg := range l.Args {
		if arg == "--prompt" {
			initial = true
		}
	}
	data, err := json.Marshal(map[string]any{"version": 1, "identity": r.identity, "session": r.record.NativeID, "project": r.record.Project, "model": l.Model, "states": states, "initial_action": initial})
	if err != nil {
		return err
	}
	l.Env = childEnvironment(l.Env, map[string]string{"HRX_OPENCODE_ACTION_GUARD": string(data)}, nil)
	return nil
}
