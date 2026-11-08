package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/meshcrawler/hrx/internal/session"
)

func TestNativeSessionNormalization(t *testing.T) {
	for _, tc := range []struct {
		harness     string
		args, extra []string
		id          string
		latest      bool
	}{
		{"codex", []string{"resume", "id", "prompt"}, []string{"prompt"}, "id", false},
		{"codex", []string{"exec", "--json", "resume", "id", "prompt"}, []string{"exec", "--json", "prompt"}, "id", false},
		{"codex", []string{"resume", "--last"}, []string{}, "", true},
		{"claude", []string{"-p", "--resume=id", "task"}, []string{"-p", "task"}, "id", false},
		{"opencode", []string{"run", "-s", "id", "task"}, []string{"run", "task"}, "id", false},
		{"opencode", []string{"--continue"}, []string{}, "", true},
	} {
		o, err := normalizeNativeSession(tc.harness, LaunchOptions{Extra: tc.args})
		if err != nil || o.Resume != tc.id || o.Continue != tc.latest || strings.Join(o.Extra, "\x00") != strings.Join(tc.extra, "\x00") {
			t.Fatalf("%s: %+v %v", tc.harness, o, err)
		}
	}
}

func TestOpenCodeYoloAllowsNativeSessionFlag(t *testing.T) {
	o, err := parseLaunchFor("opencode", []string{"--yolo", "--", "run", "-s", "native-id", "task"})
	if err != nil {
		t.Fatal(err)
	}
	o, err = normalizeNativeSession("opencode", o)
	if err != nil || o.Resume != "native-id" || !o.Yolo {
		t.Fatalf("native session: %+v %v", o, err)
	}
	args, err := sessionArguments("opencode", o)
	if err != nil || !reflect.DeepEqual(args, []string{"run", "--auto", "--session", "native-id", "task"}) {
		t.Fatalf("launch: %v %v", args, err)
	}
}

func TestClaudeCommsCannotOverridePromptSnapshotPolicy(t *testing.T) {
	for _, flag := range [][]string{{"--system-prompt-snapshot", "on"}, {"--system-prompt-snapshot=on"}, {"--system-prompt-snapshot", "off"}} {
		args := append([]string{"--yesod-comms", "--"}, flag...)
		if _, err := parseLaunchFor("claude", args); err == nil {
			t.Fatal("accepted snapshot override")
		}
		// Restored communications options must enforce the same invariant.
		if _, err := sessionArguments("claude", LaunchOptions{YesodMail: true, Extra: flag}); err == nil {
			t.Fatal("restored communications accepted snapshot override")
		}
		if _, err := parseLaunchFor("claude", append([]string{"--"}, flag...)); err != nil {
			t.Fatal("unrelated non-comms launch rejected", err)
		}
	}
}

func TestResolveLaunchSessionRestoresBeforeIdentityGuard(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	project, err := launchProject("codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := session.Default()
	if err := store.Save(session.Record{NativeID: "native-id", Harness: "codex", Identity: "model", Route: "route", Reasoning: "high", Project: project, Mail: true, Agent: "host/hrx/saved"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--continue"}, {"--yesod-mail", "--resume", "native-id"}, {"--yesod-mail", "--", "resume", "native-id"}} {
		o, err := ParseLaunch(args)
		if err != nil {
			t.Fatal(err)
		}
		o, err = resolveLaunchSession("codex", o)
		if err != nil || o.Resume != "native-id" || o.Model != "route" || o.YesodAgent != "host/hrx/saved" || o.Yolo {
			t.Fatalf("restoration: %+v %v", o, err)
		}
		if _, err := sessionArguments("codex", o); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAttachSessionHooksRetainsLaunchAndPrivateMetadata(t *testing.T) {
	r := session.Record{Harness: "codex", Route: "route", Identity: "model", Project: "/project"}
	command := sessionHookCommand("/path with space/it's-hrx")
	if command != "'/path with space/it'\"'\"'s-hrx' _session-start" {
		t.Fatal("unsafe shell quoting", command)
	}
	for _, harness := range []string{"codex", "claude"} {
		l := Launch{Executable: harness, Args: []string{"--model", "route"}, Env: []string{"KEEP=value"}}
		if err := attachSessionHook(&l, command, r); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(l.Args[len(l.Args)-2:], []string{"--model", "route"}) {
			t.Fatal("launch arguments replaced")
		}
		for i, arg := range l.Args {
			if strings.HasPrefix(arg, "hooks.SessionStart=") {
				var config map[string]any
				if err := toml.Unmarshal([]byte(arg), &config); err != nil {
					t.Fatal("invalid Codex overlay", err)
				}
			}
			if arg == "--settings" {
				var config map[string]any
				if err := json.Unmarshal([]byte(l.Args[i+1]), &config); err != nil || len(config) != 1 || config["hooks"] == nil {
					t.Fatal("Claude overlay changed unrelated settings")
				}
			}
			if strings.Contains(arg, "dangerously-bypass-hook-trust") {
				t.Fatal("bypassed hook trust")
			}
		}
		if !strings.Contains(strings.Join(l.Env, "\n"), sessionMetadataEnv+"=") {
			t.Fatal("no hook metadata")
		}
	}
}

func TestSessionCaptureDryRunNeverCreatesState(t *testing.T) {
	t.Setenv("PATH", "")
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", state)
	for _, harness := range []string{"codex", "claude", "opencode"} {
		l := Launch{Executable: harness}
		var out bytes.Buffer
		if err := prepareSessionCapture(context.Background(), LaunchOptions{DryRun: true}, &l, &out); err != nil {
			t.Fatal(err)
		}
		if len(l.Args) != 0 || out.Len() != 0 {
			t.Fatal("dry-run changed launch")
		}
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("dry-run created session state")
	}
}

func TestSessionProjectOverrides(t *testing.T) {
	dir := t.TempDir()
	want, _ := filepath.EvalSymlinks(dir)
	for _, tc := range []struct {
		harness string
		args    []string
	}{
		{"codex", []string{"--cd", dir}}, {"codex", []string{"-C" + dir}},
		{"opencode", []string{"--prompt", "task", dir}},
	} {
		got, err := launchProject(tc.harness, tc.args)
		if err != nil || got != want {
			t.Fatalf("project: %q %v", got, err)
		}
	}
}

func TestCodexPitputCaptureUsesAdmissionOwnerWithoutHook(t *testing.T) {
	project := t.TempDir()
	launch := Launch{Executable: "codex", Identity: "model", Model: "route", Args: []string{"initial task"}, Env: []string{"KEEP=value"}, pitputRuntime: &pitputRuntime{identity: "host/worker/agent", boards: []string{"work"}}}
	options := LaunchOptions{project: project, Pitput: true, PitputTargeted: true, PitputFollowThreads: []string{"/b/work/topic"}}
	if err := prepareSessionCapture(context.Background(), options, &launch, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(launch.Args, []string{"initial task"}) || !reflect.DeepEqual(launch.Env, []string{"KEEP=value"}) {
		t.Fatal("admission-owned capture injected hook or modified prompt")
	}
	record := launch.pitputRuntime.record
	if record.NativeID != "" || record.Project != project || record.Route != "route" || record.AgentIdentity != "host/worker/agent" || !record.PitputTargeted || !reflect.DeepEqual(record.PitputFollowThreads, options.PitputFollowThreads) {
		t.Fatal("admission record lost launch binding")
	}
}
