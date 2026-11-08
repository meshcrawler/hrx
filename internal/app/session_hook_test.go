package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/meshcrawler/hrx/internal/monitor"
	"github.com/meshcrawler/hrx/internal/session"
)

func TestSessionHookCapturesNativeIDWithoutTranscript(t *testing.T) {
	// A test launched by HRX must not overwrite the parent agent's hook outputs.
	t.Setenv(monitor.RunEnv, "")
	t.Setenv(resumeReceiptEnv, "")
	project, _ := filepath.EvalSymlinks(t.TempDir())
	s := session.Store{Dir: filepath.Join(t.TempDir(), "sessions")}
	r := session.Record{Harness: "codex", Identity: "model", Route: "provider/model", Reasoning: "high", Mail: true, Agent: "host/hrx/name"}
	metadata, _ := json.Marshal(r)
	event, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "actual-native-id", "cwd": project, "model": r.Route, "transcript_path": "/must/not/be/read", "prompt": "secret-sentinel", "permission_mode": "bypassPermissions"})
	if err := receiveSessionStart(strings.NewReader(string(event)), string(metadata), s); err != nil {
		t.Fatal(err)
	}
	got, err := s.Resolve("codex", project, "actual-native-id")
	if err != nil || got.Agent != r.Agent || got.Route != r.Route {
		t.Fatalf("capture: %+v %v", got, err)
	}
	data, _ := os.ReadFile(filepath.Join(s.Dir, "index.json"))
	for _, forbidden := range []string{"secret-sentinel", "transcript_path", "bypassPermissions", "/must/not/be/read"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatal("hook persisted unrelated content")
		}
	}
}

func TestSessionHookRejectsMismatchedEvents(t *testing.T) {
	project, _ := filepath.EvalSymlinks(t.TempDir())
	r := session.Record{NativeID: "expected", Harness: "codex", Identity: "model", Route: "route"}
	metadata, _ := json.Marshal(r)
	for _, change := range []map[string]any{
		{"session_id": "different"}, {"model": "wrong-route"}, {"hook_event_name": "SubagentStart"}, {"cwd": "relative"},
	} {
		s := session.Store{Dir: filepath.Join(t.TempDir(), "sessions")}
		event := map[string]any{"hook_event_name": "SessionStart", "source": "resume", "session_id": "expected", "cwd": project, "model": "route"}
		for key, value := range change {
			event[key] = value
		}
		payload, _ := json.Marshal(event)
		if err := receiveSessionStart(strings.NewReader(string(payload)), string(metadata), s); err == nil {
			t.Fatal("mismatched event indexed", change)
		}
		if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
			t.Fatal("invalid event wrote index")
		}
	}
}

func TestSessionHookInertOutsideLauncherAndBoundsInput(t *testing.T) {
	s := session.Store{Dir: filepath.Join(t.TempDir(), "sessions")}
	if err := receiveSessionStart(strings.NewReader("bad"), "", s); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Fatal("inert hook wrote index")
	}
	if err := receiveSessionStart(strings.NewReader(strings.Repeat("x", 65537)), `{}`, s); err == nil {
		t.Fatal("unbounded payload accepted")
	}
}

func TestClaudeClearHandoffValidatesBeforeMutationAndIndexesAfterSuccess(t *testing.T) {
	t.Setenv(monitor.RunEnv, "")
	t.Setenv(resumeReceiptEnv, "")
	project, _ := filepath.EvalSymlinks(t.TempDir())
	record := session.Record{Harness: "claude", NativeID: "old", AgentIdentity: "host/dev/agent", Pitput: true, Project: project, Identity: "model", Route: "route"}
	metadata, _ := json.Marshal(record)
	for _, tc := range []struct {
		source, model, cwd  string
		fail, called, saved bool
	}{
		{"clear", "route", project, false, true, true},
		{"clear", "route", project, true, true, false},
		{"clear", "wrong", project, false, false, false},
		{"resume", "route", project, false, false, false},
		{"startup", "route", project, false, false, false},
		{"clear", "route", "relative", false, false, false},
	} {
		s := session.Store{Dir: filepath.Join(t.TempDir(), "sessions")}
		payload, _ := json.Marshal(map[string]string{"hook_event_name": "SessionStart", "session_id": "new", "source": tc.source, "model": tc.model, "cwd": tc.cwd})
		called := false
		err := receiveSessionStartHandoff(strings.NewReader(string(payload)), string(metadata), s, func(r session.Record, next string) error {
			called = true
			if r.NativeID != "old" || next != "new" || r.AgentIdentity != record.AgentIdentity {
				t.Fatal("lost launch ownership")
			}
			if tc.fail {
				return errors.New("retirement failed")
			}
			return nil
		})
		if called != tc.called || (err == nil) != tc.saved {
			t.Fatalf("%+v called=%v err=%v", tc, called, err)
		}
		if _, err := os.Stat(s.Dir); !tc.saved && !os.IsNotExist(err) {
			t.Fatal("failed handoff indexed a session")
		}
	}
}
