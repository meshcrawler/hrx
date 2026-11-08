package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestClaudeNoninteractivePitputDoesNotPrepareTasks(t *testing.T) {
	for _, args := range [][]string{{"-p", "task"}, {"--print", "task"}, {"--print=task"}} {
		l := Launch{Executable: "claude", Args: args}
		if err := preparePitputRuntime(context.Background(), LaunchOptions{Pitput: true, Extra: args}, &l); err != nil {
			t.Fatal(err)
		}
		if l.pitputRuntime != nil {
			t.Fatal("noninteractive launch prepared tasks")
		}
	}
}

func TestClaudeTaskSessionBinding(t *testing.T) {
	const id = "12345678-1234-4123-8123-123456789abc"
	for _, tc := range []struct {
		args   []string
		resume string
		bad    bool
	}{
		{args: []string{"--session-id", id}},
		{args: []string{"--session-id=" + id}},
		{args: []string{"--resume", id}, resume: id},
		{args: []string{"--session-id", id}, resume: id, bad: true},
		{args: []string{"--session-id", id, "--session-id=" + id}, bad: true},
		{args: []string{"--session-id"}, bad: true},
		{args: []string{"--session-id", "bad"}, bad: true},
	} {
		l := Launch{Args: slices.Clone(tc.args)}
		got, err := bindClaudeTaskSession(&l, tc.resume)
		if (err != nil) != tc.bad || !tc.bad && got != id {
			t.Fatalf("args=%v resume=%s: %s %v", tc.args, tc.resume, got, err)
		}
		if !slices.Equal(l.Args, tc.args) {
			t.Fatal("existing native session flags changed")
		}
	}
	l := Launch{Args: []string{"--", "--session-id", "literal prompt"}}
	id1, err := bindClaudeTaskSession(&l, "")
	if err != nil || !pitputThreadID.MatchString(id1) || l.Args[0] != "--session-id" || l.Args[1] != id1 {
		t.Fatalf("new session not pinned: %v %v", l.Args, err)
	}
	id2, err := bindClaudeTaskSession(&l, "")
	if err != nil || id2 != id1 {
		t.Fatal("repeated preparation changed session")
	}
}

func TestClaudeTaskConfigsRetainSourceState(t *testing.T) {
	root := t.TempDir()
	r := &pitputRuntime{identity: "host/dev/agent", boards: []string{"dev"}, dm: true, targeted: true}
	first, err := r.claudeTaskConfigs(root, root, "/private/claude", "12345678-1234-4123-8123-123456789abc")
	if err != nil || len(first) != 2 {
		t.Fatalf("configs: %v %v", first, err)
	}
	second, err := r.claudeTaskConfigs(root, root, "/private/claude", "22345678-1234-4123-8123-123456789abc")
	if err != nil {
		t.Fatal(err)
	}
	states := make(map[string]bool)
	for i := range first {
		var before, after map[string]any
		for path, target := range map[string]*map[string]any{first[i]: &before, second[i]: &after} {
			data, err := os.ReadFile(path)
			if err != nil || toml.Unmarshal(data, target) != nil {
				t.Fatal("invalid config", err)
			}
			info, _ := os.Stat(path)
			if info.Mode().Perm() != 0600 {
				t.Fatal("config not private")
			}
		}
		if before["backend"] != "claude-task" || before["claude_home"] != "/private/claude" || before["state_directory"] != after["state_directory"] {
			t.Fatal("transport or continuity binding changed", before, after)
		}
		state := before["state_directory"].(string)
		if states[state] {
			t.Fatal("board and DM share pending state")
		}
		states[state] = true
		if filepath.Dir(first[i]) == filepath.Dir(second[i]) {
			t.Fatal("session configs overlap")
		}
	}
}

func TestClaudeResumeCreatesFreshGenerationWithSameSourceState(t *testing.T) {
	stateRoot := t.TempDir()
	const id = "12345678-1234-4123-8123-123456789abc"
	r := &pitputRuntime{identity: "host/dev/agent", boards: []string{"dev"}, dm: true}
	first, err := r.claudeTaskConfigs(t.TempDir(), stateRoot, "/private/claude", id)
	if err != nil {
		t.Fatal(err)
	}
	before := make([][]byte, len(first))
	for i, path := range first {
		before[i], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	second, err := r.claudeTaskConfigs(t.TempDir(), stateRoot, "/private/claude", id)
	if err != nil || len(second) != len(first) {
		t.Fatal("resume configs", err)
	}
	for i, path := range second {
		var old, next map[string]any
		data, err := os.ReadFile(path)
		if err != nil || toml.Unmarshal(data, &next) != nil || toml.Unmarshal(before[i], &old) != nil {
			t.Fatal("invalid config", err)
		}
		if old["state_directory"] != next["state_directory"] || old["claude_task_generation"] == next["claude_task_generation"] || path == first[i] {
			t.Fatal("resume did not isolate generation while retaining source state")
		}
		still, err := os.ReadFile(first[i])
		if err != nil || string(still) != string(before[i]) {
			t.Fatal("resume modified stale task configuration")
		}
	}
}

func TestClaudeSystemPromptPreservesLiteralArguments(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args, want []string
	}{
		{"plain", []string{"task"}, []string{"--append-system-prompt", "owned", "task"}},
		{"equal", []string{"--append-system-prompt=user", "task"}, []string{"--append-system-prompt", "user\nowned", "task"}},
		{"duplicates", []string{"--append-system-prompt", "yesod", "--append-system-prompt=user"}, []string{"--append-system-prompt", "yesod\nuser\nowned"}},
		{"literal", []string{"--", "--append-system-prompt", "user"}, []string{"--append-system-prompt", "owned", "--", "--append-system-prompt", "user"}},
		{"value", []string{"--system-prompt", "--append-system-prompt", "task"}, []string{"--append-system-prompt", "owned", "--system-prompt", "--append-system-prompt", "task"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := slices.Clone(tc.args)
			got, err := appendClaudeSystemPrompt(tc.args, "owned")
			if err != nil || !slices.Equal(got, tc.want) || !slices.Equal(before, tc.args) {
				t.Fatalf("got %q, err %v; want %q; input %q", got, err, tc.want, tc.args)
			}
		})
	}
	if _, err := appendClaudeSystemPrompt([]string{"--append-system-prompt"}, "owned"); err == nil {
		t.Fatal("accepted missing prompt value")
	}
}
