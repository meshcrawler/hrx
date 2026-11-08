package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/meshcrawler/hrx/internal/session"
)

func TestOpenCodeFlagsFollowRunWithRootOptions(t *testing.T) {
	for _, prefix := range [][]string{nil, {"--log-level", "INFO"}, {"--print-logs", "--log-level=DEBUG"}} {
		s := testSettings(t)
		s.Harness = "opencode"
		extra := append(append([]string{}, prefix...), "run", "--title", "--standalone", "--", "literal prompt")
		launch, err := buildOtherLaunch(s, otherSnapshot("opencode"), LaunchOptions{Yolo: true, Resume: "native-id", Extra: extra}, "fixture-key")
		if err != nil {
			t.Fatal(err)
		}
		want := append(append([]string{}, prefix...), "run", "--model", "hrx/provider/example", "--auto", "--session", "native-id", "--title", "--standalone", "--", "literal prompt")
		if !reflect.DeepEqual(launch.Args, want) {
			t.Fatalf("routing: %v != %v", launch.Args, want)
		}
		if err := injectYesod(s, &launch, "fixture instructions"); err != nil {
			t.Fatal(err)
		}
		position := len(prefix) + 1
		if !reflect.DeepEqual(launch.Args[:position], want[:position]) || launch.Args[position] != "--file" || launch.Args[position+2] != "--standalone" || !reflect.DeepEqual(launch.Args[position+3:], want[position:]) {
			t.Fatalf("instructions or standalone flag misplaced: %v", launch.Args)
		}
	}
}

func TestOpenCodeCaptureFlagsFollowRunWithRootOptions(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	launch, project, _ := fakeOpenCodeSessionAPI(t, nil, "0")
	response, _ := json.Marshal(map[string]any{"data": map[string]any{"id": "native-id", "location": map[string]string{"directory": project}}})
	launch.Env = childEnvironment(launch.Env, map[string]string{"HRX_TEST_RESPONSE": string(response)}, nil)
	launch.Executable, launch.Identity = "opencode", "fixture"
	launch.Args = []string{"--log-level", "INFO", "run", "--title", "--standalone", "--", "task"}
	if err := prepareSessionCapture(context.Background(), LaunchOptions{project: project}, &launch, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"--log-level", "INFO", "run", "--standalone", "--session", "native-id", "--title", "--standalone", "--", "task"}
	if !reflect.DeepEqual(launch.Args, want) {
		t.Fatalf("capture: %v != %v", launch.Args, want)
	}
	store, _ := session.Default()
	if _, err := store.Resolve("opencode", project, "native-id"); !errors.Is(err, session.ErrNotFound) || launch.pendingSession == nil {
		t.Fatal("preparation indexed an unstarted native session", err)
	}
}

func TestOpenCodeFlagInsertionIgnoresCommandLikeData(t *testing.T) {
	for _, args := range [][]string{{"--title", "run"}, {"--", "run"}, {"project", "run"}} {
		original := append([]string{}, args...)
		want := append([]string{"--auto"}, args...)
		got := insertOpenCodeFlags(args, "--auto")
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(args, original) {
			t.Fatalf("insertion: %v != %v (input %v)", got, want, args)
		}
	}
}

func TestProjectSelectionIgnoresOptionValues(t *testing.T) {
	want, err := launchProject("codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	for _, args := range [][]string{
		{"exec", "--output-last-message", "-C" + other, "task"},
		{"exec", "--", "-C" + other},
	} {
		got, err := launchProject("codex", args)
		if err != nil || got != want {
			t.Fatal("option value changed project", got, err)
		}
	}
	canonical, _ := filepath.EvalSymlinks(other)
	got, err := launchProject("opencode", []string{"--prompt", "--session", "--log-level", "INFO", other})
	if err != nil || got != canonical {
		t.Fatal("OpenCode option values changed project", got, err)
	}
}

func TestNativeResumeSeparatesOptionsAndData(t *testing.T) {
	for _, tc := range []struct {
		harness     string
		args, extra []string
		id          string
		latest      bool
	}{
		{"codex", []string{"exec", "--output-last-message", "resume", "task"}, []string{"exec", "--output-last-message", "resume", "task"}, "", false},
		{"codex", []string{"exec", "--", "resume", "literal"}, []string{"exec", "--", "resume", "literal"}, "", false},
		{"codex", []string{"resume", "id", "--", "--last"}, []string{"--", "--last"}, "id", false},
		{"codex", []string{"resume", "--output-last-message", "--last", "id"}, []string{"--output-last-message", "--last"}, "id", false},
		{"codex", []string{"resume", "--", "id", "--last"}, []string{"--", "--last"}, "id", false},
		{"claude", []string{"-p", "--append-system-prompt", "--resume", "task"}, []string{"-p", "--append-system-prompt", "--resume", "task"}, "", false},
		{"claude", []string{"--", "--resume", "literal"}, []string{"--", "--resume", "literal"}, "", false},
		{"claude", []string{"--resume=id", "--append-system-prompt", "--continue", "task"}, []string{"--append-system-prompt", "--continue", "task"}, "id", false},
		{"opencode", []string{"run", "--title", "--continue", "--session=id", "task"}, []string{"run", "--title", "--continue", "task"}, "id", false},
		{"opencode", []string{"--prompt", "--session", "--continue"}, []string{"--prompt", "--session"}, "", true},
	} {
		o, err := normalizeNativeSession(tc.harness, LaunchOptions{Extra: tc.args})
		if err != nil || o.Resume != tc.id || o.Continue != tc.latest || !reflect.DeepEqual(o.Extra, tc.extra) {
			t.Fatalf("%s %v => %+v %v", tc.harness, tc.args, o, err)
		}
		if nativeResume(tc.harness, tc.args) != (tc.id != "" || tc.latest) {
			t.Fatal("guard and normalization disagree", tc.args)
		}
	}
}

func TestNativeResumePreservesCommandPosition(t *testing.T) {
	for _, args := range [][]string{
		{"--json", "exec", "resume", "id", "task"},
		{"-C", "/project", "resume", "id", "task"},
		{"exec", "--json", "resume", "id", "task"},
	} {
		o, err := normalizeNativeSession("codex", LaunchOptions{Extra: args})
		if err != nil {
			t.Fatal(err)
		}
		got, err := sessionArguments("codex", o)
		if err != nil {
			t.Fatal(err)
		}
		want := args
		// Exec flags may follow the normalized resume subcommand.
		if args[0] == "exec" {
			want = []string{"exec", "resume", "id", "--json", "task"}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestNativeGuardsIgnoreDataButRejectActualOverrides(t *testing.T) {
	for _, tc := range []struct {
		harness string
		args    []string
	}{
		{"claude", []string{"--yesod-comms", "--", "-p", "--append-system-prompt", "--background", "attach"}},
		{"claude", []string{"--yolo", "--", "--append-system-prompt", "--permission-mode", "task"}},
		{"opencode", []string{"--yolo", "--", "run", "--title", "--model", "task"}},
		{"codex", []string{"--resume", "id", "--", "exec", "--", "resume"}},
	} {
		if _, err := parseLaunchFor(tc.harness, tc.args); err != nil {
			t.Fatalf("data rejected: %s %v: %v", tc.harness, tc.args, err)
		}
	}
	for _, tc := range []struct {
		harness string
		args    []string
	}{
		{"claude", []string{"--yesod-comms", "--", "--verbose", "attach", "id"}},
		{"codex", []string{"--yesod-comms", "--", "--json", "fork", "id"}},
		{"opencode", []string{"--", "--log-level", "INFO", "serve"}},
		{"claude", []string{"--", "--append-system-prompt", "safe", "--model", "other"}},
	} {
		if _, err := parseLaunchFor(tc.harness, tc.args); err == nil {
			t.Fatalf("unsafe option accepted: %s %v", tc.harness, tc.args)
		}
	}
}
