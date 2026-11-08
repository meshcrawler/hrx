package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestClaudeSuccessorConfigPreservesSelection(t *testing.T) {
	old := "12345678-1234-4123-8123-123456789abc"
	next := "22345678-1234-4123-8123-123456789abc"
	data := []byte(`backend = "claude-task"
claude_task_generation = "42345678-1234-4123-8123-123456789abc"
claude_home = "/private/claude"
state_directory = "/private/state"
[[agents]]
agent_id = "host/dev/agent"
thread_id = "` + old + `"
board = "dev"
targeted = true
followed_threads = ["/b/dev/topic"]
`)
	updated, err := successorClaudeConfig(data, "host/dev/agent", old, next)
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]any
	_ = toml.Unmarshal(data, &before)
	_ = toml.Unmarshal(updated, &after)
	if after["claude_task_generation"] == before["claude_task_generation"] {
		t.Fatal("successor reused retired task generation")
	}
	retry, err := successorClaudeConfig(data, "host/dev/agent", old, next)
	if err != nil || string(retry) != string(updated) {
		t.Fatal("retry changed successor generation")
	}
	after["claude_task_generation"] = before["claude_task_generation"]
	after["agents"].([]any)[0].(map[string]any)["thread_id"] = old
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("handoff changed source or selection")
	}
	if _, err := successorClaudeConfig(data, "other/owner/agent", old, next); err == nil {
		t.Fatal("wrong owner accepted")
	}
}

func TestClaudeHandoffRetriesBeforePublishingAndTracksRepeatedClear(t *testing.T) {
	const old = "12345678-1234-4123-8123-123456789abc"
	const next = "22345678-1234-4123-8123-123456789abc"
	const third = "32345678-1234-4123-8123-123456789abc"
	root := t.TempDir()
	r := &pitputRuntime{identity: "host/dev/agent", boards: []string{"dev"}, dm: true}
	paths, err := r.claudeTaskConfigs(root, root, "/private/claude", old)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := newClaudeBinding(root, "/bin/pitput", r.identity, old, "canonical protocol", paths)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	retired := map[string]int{}
	fail := true
	commands := 0
	run := func(ctx context.Context, cli string, args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		commands++
		if slices.Contains(args, "--retire") {
			retired[args[len(args)-2]]++
			return nil
		}
		if retired[old] < 2 {
			return errors.New("prepared before all predecessors retired")
		}
		if fail {
			fail = false
			return errors.New("interrupted preparation")
		}
		return nil
	}
	ctx := context.Background()
	if prompt, err := handoffClaudeTasks(ctx, manifest, r.identity, old, next, run); err == nil || prompt != "" {
		t.Fatal("failed handoff published commands")
	}
	data, _ := os.ReadFile(manifest)
	var binding claudeTaskBinding
	_ = json.Unmarshal(data, &binding)
	if binding.Current != old || binding.Pending != next {
		t.Fatal("interrupted handoff lost its target")
	}
	if _, err := handoffClaudeTasks(ctx, manifest, r.identity, old, third, run); err == nil {
		t.Fatal("incomplete transition silently replaced")
	}
	prompt, err := handoffClaudeTasks(ctx, manifest, r.identity, old, next, run)
	if err != nil || !strings.Contains(prompt, next) || strings.Contains(prompt, old) {
		t.Fatalf("successor commands: %v %s", err, prompt)
	}
	count := commands
	replay, err := handoffClaudeTasks(ctx, manifest, r.identity, old, next, run)
	if err != nil || replay != prompt || commands != count {
		t.Fatal("completed handoff did not replay idempotently")
	}
	if _, err := handoffClaudeTasks(ctx, manifest, r.identity, old, third, run); err != nil || retired[next] != 2 {
		t.Fatal("second clear did not retire current sources", err, retired)
	}
	if _, err := handoffClaudeTasks(ctx, manifest, r.identity, old, next, run); err == nil {
		t.Fatal("retired native session reactivated")
	}
	for _, path := range paths {
		data, _ := os.ReadFile(path)
		if !strings.Contains(string(data), old) {
			t.Fatal("predecessor configuration changed")
		}
	}
}
