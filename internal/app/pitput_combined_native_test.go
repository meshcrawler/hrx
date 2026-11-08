package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/session"
)

// The existing integration supervisor owns PostgreSQL, pinned native Codex,
// installed Pitput, the scripted Responses provider and every descendant. This
// exercises the production launch/owned ingress path, not Python lifecycle emulation.
func TestNativePitputCombinedLaunchCodex(t *testing.T) {
	if os.Getenv("HRX_PITPUT_COMBINED_NATIVE") != "1" {
		t.Skip("GitLab disposable combined integration only")
	}
	root := os.Getenv("HRX_PITPUT_COMBINED_CONTROL")
	provider := os.Getenv("HRX_PITPUT_COMBINED_PROVIDER")
	helper := os.Getenv("HRX_PITPUT_ADMISSION_BINARY")
	if root == "" || !strings.HasPrefix(provider, "http://127.0.0.1:") || !filepath.IsAbs(helper) {
		t.Fatal("combined fixture inputs missing")
	}
	stage := func(name string) {
		raw, _ := json.Marshal(map[string]string{"stage": name})
		if err := os.WriteFile(filepath.Join(root, "native-stage.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	stage("fixture_setup")
	f := &targetedFixture{t: t, harness: "codex", identity: "test/combined/agent", receipt: map[string]any{}, bound: map[string]bool{}}
	f.setup()
	f.gateway.forward = provider
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	stage("fresh_launch_prepare")
	_, launch, err := f.launch(ctx, []string{"--pitput", "--pitput-board", targetedBoard, "--pitput-combined-preview", "--agent-identity", f.identity, "--model", "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	launch.pitputRuntime.admissionBinary = helper
	stage("fresh_native_start")
	stop, rpc, id := f.start(&launch, "")
	defer func() { rpc.conn.Close(); stop() }()
	stage("fresh_wait_consumer")
	var config string
	for deadline := time.Now().Add(40 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		paths, _ := filepath.Glob(filepath.Join(os.Getenv("XDG_STATE_HOME"), "hrx/pitput/*", id, "@combined/bridge.toml"))
		if len(paths) == 1 {
			if status, err := f.tryStatus(paths[0]); err == nil && status.Running {
				config = paths[0]
				break
			}
		}
		f.sidecar(&launch)
		if time.Now().After(deadline) {
			t.Fatal("combined consumer not running")
		}
	}
	savedStore, err := session.Default()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := savedStore.Resolve("codex", f.project, id)
	if err != nil || !saved.PitputCombinedPreview {
		t.Fatal("combined option not retained", err)
	}
	stage("fresh_stop")
	rpc.conn.Close()
	stop()
	ready, _ := json.Marshal(map[string]string{"config": config, "thread": id})
	if err := os.WriteFile(filepath.Join(root, "stopped.ready"), ready, 0600); err != nil {
		t.Fatal(err)
	}
	wait := func(name string) {
		for deadline := time.Now().Add(120 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(root, name)); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("combined phase deadline:", name)
			}
		}
	}
	stage("stopped_pending_wait")
	wait("resume.go")
	stage("resume_launch_prepare")
	_, resumed, err := f.launch(ctx, []string{"--resume", id, "--agent-identity", f.identity})
	if err != nil || !resumed.pitputRuntime.combined {
		t.Fatal("combined resume preparation", err)
	}
	resumed.pitputRuntime.admissionBinary = helper
	stage("resume_native_start")
	stop, rpc, resumedID := f.start(&resumed, id)
	if resumedID != id {
		t.Fatal("combined native session changed")
	}
	if err := os.WriteFile(filepath.Join(root, "resumed.ready"), ready, 0600); err != nil {
		t.Fatal(err)
	}
	stage("installed_helper_completion")
	wait("done.go")
	stage("native_post_tool_verification")
	var native json.RawMessage
	if err := rpc.call("thread/read", map[string]any{"threadId": id, "includeTurns": true}, &native); err != nil || !strings.Contains(string(native), "fixture-combined-complete:ask") {
		t.Fatal("combined native post-tool completion missing", err)
	}
	if status, err := f.tryStatus(config); err != nil || status.Agents[0].Pending != nil {
		t.Fatal("combined source remained pending", err)
	}
	outcome := map[string]any{"status": "PASS", "scope": "hrx_owned_combined_attention_v1", "same_native_session": true, "saved_combined_option": true, "single_consumer_config": true, "native_post_tool_completion": true}
	raw, _ := json.Marshal(outcome)
	if err := os.WriteFile(filepath.Join(root, "native-receipt.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}
