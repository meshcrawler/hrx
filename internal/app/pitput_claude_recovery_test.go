package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/meshcrawler/hrx/internal/session"
)

type failedClaudeHookOutput struct{}

func (failedClaudeHookOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestClaudePromptRecoversOnlyRecordedHandoffAndRemainsQuietAfterPublication(t *testing.T) {
	t.Setenv(resumeReceiptEnv, "")
	const old = "12345678-1234-4123-8123-123456789abc"
	const next = "22345678-1234-4123-8123-123456789abc"
	const other = "32345678-1234-4123-8123-123456789abc"
	for _, failure := range []string{"retirement", "publication"} {
		t.Run(failure, func(t *testing.T) {
			root, _ := filepath.EvalSymlinks(t.TempDir())
			r := &pitputRuntime{identity: "host/dev/agent", boards: []string{"dev"}}
			paths, err := r.claudeTaskConfigs(root, root, "/private/claude", old)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := newClaudeBinding(root, "/bin/pitput", r.identity, old, "canonical protocol", paths)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			run := func(context.Context, string, ...string) error { calls++; return nil }
			ctx := context.Background()
			if failure == "retirement" {
				_, err = handoffClaudeTasks(ctx, binding, r.identity, old, next,
					func(context.Context, string, ...string) error { return errors.New("interrupted") })
			} else {
				var prompt string
				prompt, err = handoffClaudeTasks(ctx, binding, r.identity, old, next, run)
				if err != nil {
					t.Fatal(err)
				}
				err = publishClaudeHandoff(failedClaudeHookOutput{}, binding, next, "SessionStart", prompt)
			}
			if err == nil {
				t.Fatal("failure not injected")
			}
			record := session.Record{Harness: "claude", NativeID: old, AgentIdentity: r.identity,
				Pitput: true, Project: root, Identity: "model", Route: "route"}
			metadata, _ := json.Marshal(record)
			store := session.Store{Dir: filepath.Join(root, "sessions")}
			recover := func(id string, out io.Writer) error {
				payload, _ := json.Marshal(map[string]string{"hook_event_name": "UserPromptSubmit", "session_id": id, "cwd": root})
				return recoverClaudeHandoff(ctx, bytes.NewReader(payload), string(metadata), binding, store, out, run)
			}
			before := calls
			var out bytes.Buffer
			if err := recover(other, &out); err == nil || calls != before || out.Len() != 0 {
				t.Fatal("wrong native session triggered recovery")
			}
			if err := recover(next, &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "UserPromptSubmit") || !strings.Contains(out.String(), next) || strings.Contains(out.String(), old) {
				t.Fatal("wrong recovery commands", out.String())
			}
			if failure == "publication" && calls != before {
				t.Fatal("publication retry repeated source mutation")
			}
			if got, err := store.Resolve("claude", root, next); err != nil || got.AgentIdentity != r.identity {
				t.Fatal("recovered native session not indexed", err)
			}
			before = calls
			out.Reset()
			if err := recover(next, &out); err != nil || out.Len() != 0 || calls != before {
				t.Fatal("normal prompt repeated recovery", err)
			}
		})
	}
}
