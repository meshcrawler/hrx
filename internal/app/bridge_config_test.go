package app

import (
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func decodedKeys(t *testing.T, c bridgeConfig) map[string]any {
	t.Helper()
	data, err := c.marshal()
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := toml.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The typed configs decode to exactly the keys and values the former
// map[string]any builders wrote, so Pitput sees an identical file.
func TestBridgeConfigWireFormat(t *testing.T) {
	board := bridgeAgent{AgentID: "host/dev/agent", Board: "dev", ThreadID: "ses_1", Targeted: true, FollowedThreads: []string{"/b/dev/x"}}
	opencode := openCodeBridge("http://127.0.0.1:4100", "/run/pw", "/work")
	opencode.StateDirectory, opencode.Agents = "/state", []bridgeAgent{board}
	if got, want := decodedKeys(t, opencode), map[string]any{
		"backend": "opencode", "opencode_url": "http://127.0.0.1:4100", "opencode_password_file": "/run/pw",
		"opencode_project_directory": "/work", "state_directory": "/state", "retry_seconds": int64(2),
		"agents": []any{map[string]any{"agent_id": "host/dev/agent", "board": "dev", "thread_id": "ses_1",
			"targeted": true, "followed_threads": []any{"/b/dev/x"}}},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("opencode config\n got %#v\nwant %#v", got, want)
	}
	dm := bridgeConfig{CodexBinary: "/bin/adapter", CodexHome: "/codex", RetrySeconds: 2, StateDirectory: "/state",
		Agents: []bridgeAgent{{AgentID: "host/dev/agent", ThreadID: "thread-1", Source: "dm"}}}
	if got, want := decodedKeys(t, dm), map[string]any{
		"codex_binary": "/bin/adapter", "codex_home": "/codex", "retry_seconds": int64(2), "state_directory": "/state",
		"agents": []any{map[string]any{"agent_id": "host/dev/agent", "thread_id": "thread-1", "source": "dm"}},
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("codex dm config\n got %#v\nwant %#v", got, want)
	}
	claude := claudeTaskBridge("/claude", "/root", "11111111-2222-4333-8444-555555555555")
	claude.StateDirectory, claude.Agents = "/state", []bridgeAgent{{AgentID: "host/dev/agent", Board: "dev", ThreadID: "11111111-2222-4333-8444-555555555555"}}
	got := decodedKeys(t, claude)
	if got["backend"] != "claude-task" || got["claude_home"] != "/claude" || got["monitor_timeout"] != int64(20) ||
		got["claude_task_generation"] == "" || len(got) != 6 {
		t.Fatalf("claude-task config %#v", got)
	}
}

func TestBridgeConfigRejectsWhatPitputRejects(t *testing.T) {
	agent := []bridgeAgent{{AgentID: "a/b/c", Board: "dev", ThreadID: "t"}}
	base := bridgeConfig{CodexBinary: "/bin/x", CodexHome: "/h", StateDirectory: "/s", Agents: agent}
	if err := base.validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*bridgeConfig){
		"unknown backend":         func(c *bridgeConfig) { c.Backend = "gemini" },
		"no state":                func(c *bridgeConfig) { c.StateDirectory = "" },
		"no agents":               func(c *bridgeConfig) { c.Agents = nil },
		"codex without home":      func(c *bridgeConfig) { c.CodexHome = "" },
		"opencode field on codex": func(c *bridgeConfig) { c.OpenCodeURL = "http://127.0.0.1:1" },
		"opencode incomplete": func(c *bridgeConfig) {
			*c = bridgeConfig{Backend: "opencode", OpenCodeURL: "http://127.0.0.1:1", StateDirectory: "/s", Agents: agent}
		},
		"claude without home": func(c *bridgeConfig) {
			*c = bridgeConfig{Backend: "claude-task", StateDirectory: "/s", Agents: agent}
		},
		"generation off claude":   func(c *bridgeConfig) { c.ClaudeTaskGeneration = "g" },
		"refresh without journal": func(c *bridgeConfig) { c.LifecycleRefresh = true },
		"journal without refresh": func(c *bridgeConfig) { c.LifecycleJournalDirectory = "/j" },
		"partial owned admission": func(c *bridgeConfig) { c.OwnedAdmissionBinary = "/bin/own" },
		"agent without thread":    func(c *bridgeConfig) { c.Agents = []bridgeAgent{{AgentID: "a/b/c", Board: "dev"}} },
		"board source without board": func(c *bridgeConfig) {
			c.Agents = []bridgeAgent{{AgentID: "a/b/c", ThreadID: "t"}}
		},
		"dm source with board": func(c *bridgeConfig) {
			c.Agents = []bridgeAgent{{AgentID: "a/b/c", ThreadID: "t", Source: "dm", Board: "dev"}}
		},
	} {
		c := base
		c.Agents = append([]bridgeAgent{}, base.Agents...)
		change(&c)
		if err := c.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := c.marshal(); err == nil {
			t.Errorf("%s: marshalled", name)
		}
	}
}

func TestParseBridgeConfigRejectsUnknownKeys(t *testing.T) {
	good, err := bridgeConfig{CodexBinary: "/bin/x", CodexHome: "/h", StateDirectory: "/s",
		Agents: []bridgeAgent{{AgentID: "a/b/c", Board: "dev", ThreadID: "t"}}}.marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseBridgeConfig(good); err != nil {
		t.Fatal(err)
	}
	if _, err := parseBridgeConfig([]byte(strings.Replace(string(good), "state_directory", "extra_key = 1\nstate_directory", 1))); err == nil {
		t.Fatal("accepted an unknown key")
	}
}
