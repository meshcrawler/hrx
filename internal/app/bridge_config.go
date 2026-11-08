package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// bridgeConfig is the bridge.toml that Pitput's `pitput bridge run` loads
// (ysg_mail/codex_bridge.py _load_config). Field tags are its exact keys;
// validate mirrors its per-backend rules, so HRX never writes a file the bridge
// would reject at startup. Unused fields are omitted, as Pitput forbids extras.
type bridgeConfig struct {
	Backend                   string        `toml:"backend,omitempty"` // "" is Pitput's default, codex
	CodexBinary               string        `toml:"codex_binary,omitempty"`
	CodexHome                 string        `toml:"codex_home,omitempty"`
	OpenCodeURL               string        `toml:"opencode_url,omitempty"`
	OpenCodePasswordFile      string        `toml:"opencode_password_file,omitempty"`
	OpenCodeProjectDirectory  string        `toml:"opencode_project_directory,omitempty"`
	ClaudeHome                string        `toml:"claude_home,omitempty"`
	ClaudeTaskGeneration      string        `toml:"claude_task_generation,omitempty"`
	StateDirectory            string        `toml:"state_directory"`
	RetrySeconds              int           `toml:"retry_seconds,omitempty"`
	MonitorTimeout            int           `toml:"monitor_timeout,omitempty"`
	LifecycleRefresh          bool          `toml:"lifecycle_refresh,omitempty"`
	LifecycleJournalDirectory string        `toml:"lifecycle_journal_directory,omitempty"`
	OwnedAdmissionBinary      string        `toml:"owned_admission_binary,omitempty"`
	OwnedAdmissionEndpoint    string        `toml:"owned_admission_endpoint,omitempty"`
	OwnedAdmissionBinding     string        `toml:"owned_admission_binding,omitempty"`
	Agents                    []bridgeAgent `toml:"agents"`
}

// bridgeAgent is one [[agents]] entry: a board source, or the identity's
// direct-message source (Source "dm", which carries only identity and thread).
type bridgeAgent struct {
	AgentID         string   `toml:"agent_id"`
	Board           string   `toml:"board,omitempty"`
	ThreadID        string   `toml:"thread_id"`
	Source          string   `toml:"source,omitempty"`
	Targeted        bool     `toml:"targeted,omitempty"`
	FollowedThreads []string `toml:"followed_threads,omitempty"`
	Combined        bool     `toml:"combined,omitempty"`
}

func (c bridgeConfig) validate() error {
	codex, opencode, claude := c.Backend == "" || c.Backend == "codex", c.Backend == "opencode", c.Backend == "claude-task"
	switch {
	case !codex && !opencode && !claude:
		return errors.New("bridge config: unknown backend")
	case c.StateDirectory == "" || len(c.Agents) == 0:
		return errors.New("bridge config: state directory and agents are required")
	case codex != (c.CodexBinary != "" && c.CodexHome != "") || (!codex && (c.CodexBinary != "" || c.CodexHome != "")):
		return errors.New("bridge config: codex_binary and codex_home belong to, and are required by, the codex backend")
	case opencode != (c.OpenCodeURL != "" && c.OpenCodePasswordFile != "" && c.OpenCodeProjectDirectory != "") ||
		(!opencode && (c.OpenCodeURL != "" || c.OpenCodePasswordFile != "" || c.OpenCodeProjectDirectory != "")):
		return errors.New("bridge config: opencode fields belong to, and are required by, the opencode backend")
	case claude != (c.ClaudeHome != "") || (!claude && c.ClaudeTaskGeneration != ""):
		return errors.New("bridge config: claude_home and claude_task_generation belong to the claude-task backend")
	case c.LifecycleRefresh != (c.LifecycleJournalDirectory != ""):
		return errors.New("bridge config: lifecycle refresh requires exactly its journal directory")
	}
	owned := c.OwnedAdmissionBinary != "" || c.OwnedAdmissionEndpoint != "" || c.OwnedAdmissionBinding != ""
	if owned && (c.OwnedAdmissionBinary == "" || c.OwnedAdmissionEndpoint == "" || c.OwnedAdmissionBinding == "" || !codex || c.LifecycleRefresh) {
		return errors.New("bridge config: owned admission needs all three fields on the codex backend without lifecycle refresh")
	}
	for _, a := range c.Agents {
		if a.AgentID == "" || a.ThreadID == "" {
			return errors.New("bridge config: agent requires agent_id and thread_id")
		}
		if a.Source == "dm" {
			if a.Board != "" || a.Targeted || a.FollowedThreads != nil || a.Combined {
				return errors.New("bridge config: a direct-message source carries only agent_id and thread_id")
			}
		} else if a.Source != "" || a.Board == "" {
			return errors.New("bridge config: a board source requires a board")
		}
	}
	return nil
}

func (c bridgeConfig) marshal() ([]byte, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	return toml.Marshal(c)
}

// writeBridgeConfig validates and writes dir/bridge.toml privately.
func writeBridgeConfig(dir string, c bridgeConfig) (string, error) {
	data, err := c.marshal()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "bridge.toml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return "", err
	}
	return path, nil
}

// parseBridgeConfig reads a file HRX wrote; unknown keys mean it was not ours.
func parseBridgeConfig(data []byte) (bridgeConfig, error) {
	var c bridgeConfig
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return bridgeConfig{}, errors.New("invalid bridge configuration")
	}
	return c, c.validate()
}
