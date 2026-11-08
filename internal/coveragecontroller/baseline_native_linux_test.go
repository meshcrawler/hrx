package coveragecontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/meshcrawler/hrx/internal/codexlaunch"
	"github.com/meshcrawler/hrx/internal/codexrpc"
	"github.com/meshcrawler/hrx/internal/coveragejournal"
)

// GitLab opt-in only. Real pinned app-server, no model inference or TUI release.
func TestNativeBaselineOwnedBootstrapJournal(t *testing.T) {
	if os.Getenv("HRX_COVERAGE_NATIVE_TESTS") != "1" {
		t.Skip("requires pinned GitLab native fixture")
	}
	for _, loseAfterSync := range []bool{false, true} {
		name := "publish"
		if loseAfterSync {
			name = "loss-after-sync"
		}
		t.Run(name, func(t *testing.T) { nativeBaseline(t, loseAfterSync, false, "", "flex", "fixture") })
	}
	t.Run("unknown-feature-rejected", func(t *testing.T) { nativeBaseline(t, false, true, "", "flex", "fixture") })
	t.Run("archive-revokes-publication", func(t *testing.T) { nativeBaseline(t, false, false, "thread/archive", "flex", "fixture") })
	t.Run("settings-revoke-publication", func(t *testing.T) { nativeBaseline(t, false, false, "thread/settings/update", "flex", "fixture") })
	t.Run("compaction-stages-native-pair", func(t *testing.T) { nativeBaseline(t, false, false, "thread/compact/start", "flex", "fixture") })
	t.Run("service-tier-fast", func(t *testing.T) { nativeBaseline(t, false, false, "", "fast", "gpt-6.1-sol") })
	t.Run("service-tier-fast-unsupported", func(t *testing.T) { nativeBaseline(t, false, false, "", "fast", "fixture") })
	t.Run("launcher-tui-profile", func(t *testing.T) { nativeBaseline(t, false, false, "launcher-tui-profile", "flex", "fixture") })
	t.Run("launcher-tui-profile-project", func(t *testing.T) { nativeBaseline(t, false, false, "launcher-tui-profile-project", "flex", "fixture") })
	t.Run("launcher-tui", func(t *testing.T) { nativeBaseline(t, false, false, "launcher-tui", "flex", "fixture") })
	t.Run("launcher-runtime", func(t *testing.T) { nativeBaseline(t, false, false, "launcher-runtime", "flex", "fixture") })
	t.Run("gateway-unix-resume", func(t *testing.T) { nativeBaseline(t, false, false, "gateway-resume", "flex", "fixture") })
	t.Run("gateway-unix-startup", func(t *testing.T) { nativeBaseline(t, false, false, "gateway-start", "flex", "fixture") })
	t.Run("service-tier-default", func(t *testing.T) { nativeBaseline(t, false, false, "", "default", "fixture") })
}

type nativeErrorOutput struct{ data bytes.Buffer }

func (b *nativeErrorOutput) Write(p []byte) (int, error) {
	n := len(p)
	if room := 4096 - b.data.Len(); room > 0 {
		_, _ = b.data.Write(p[:min(room, n)])
	}
	return n, nil
}

func nativeBaseline(t *testing.T, loseAfterSync, rejectFeature bool, mutation, serviceTier, model string) {
	compact := mutation == "thread/compact/start"
	profileTUI := strings.HasPrefix(mutation, "launcher-tui-profile")
	wantInstructions := "HRX profile instruction proof"
	if mutation == "launcher-tui-profile-project" {
		wantInstructions = "HRX project instruction precedence proof"
	}
	providerSeen := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	home := t.TempDir()
	for _, dir := range []string{"codex", "tmp", "journal"} {
		if err := os.Mkdir(filepath.Join(home, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	var requests atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		if profileTUI && n == 1 && r.Method == http.MethodPost && r.URL.Path == "/v1/responses" {
			raw, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
			var body struct {
				Model string
				Input []struct {
					Role    string
					Content json.RawMessage
				}
			}
			decoded := json.Unmarshal(raw, &body) == nil
			developerInit := false
			for _, item := range body.Input {
				if item.Role == "developer" && bytes.Contains(item.Content, []byte(wantInstructions)) {
					developerInit = true
				}
			}
			if !decoded || body.Model != "fixture-profile" || !developerInit || bytes.Count(raw, []byte(wantInstructions)) != 1 || !bytes.Contains(raw, []byte("HRX native terminal prompt proof")) {
				t.Errorf("profile provider parity: model=%v developer_init=%v init_count=%d initial_input=%v", body.Model == "fixture-profile", developerInit, bytes.Count(raw, []byte(wantInstructions)), bytes.Contains(raw, []byte("HRX native terminal prompt proof")))
			}
			if mutation == "launcher-tui-profile-project" && bytes.Contains(raw, []byte("HRX profile instruction proof")) {
				t.Error("profile guidance overrode project instructions")
			}
			nativeCompactionResponse(w)
			providerSeen <- struct{}{}
			return
		}
		if compact && n == 1 && r.Method == http.MethodPost && r.URL.Path == "/v1/responses" {
			nativeCompactionResponse(w)
			return
		}
		http.Error(w, "baseline fixture forbids model requests", http.StatusForbidden)
	}))
	defer endpoint.Close()
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := filepath.Abs("../../tests/compatibility/subreaper.py")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(home, "server.sock")
	receipt := filepath.Join(home, "cleanup.json")
	userArgs := []string{"--ask-for-approval", "never", "--sandbox", "read-only", "--disable", "multi_agent", "-c", fmt.Sprintf("service_tier=%q", serviceTier)}
	if serviceTier == "fast" {
		userArgs = append(userArgs, "--enable", "fast_mode")
	}
	const unknownFeature = "hrx_fixture_unknown_feature_0_163"
	if rejectFeature {
		userArgs = append(userArgs, "--enable", unknownFeature)
	}
	plan, err := codexlaunch.Build(codexlaunch.Input{
		Project: home,
		HRXArgs: []string{"--model", model, "-c", `model_provider="fixture"`, "-c",
			fmt.Sprintf(`model_providers.fixture={name="fixture",base_url=%q,wire_api="responses",env_key="HRX_FIXTURE_KEY",request_max_retries=0,stream_max_retries=0}`, endpoint.URL+"/v1")},
		UserArgs: userArgs,
	})
	if err != nil {
		t.Fatal("native launch plan", err)
	}
	if mutation == "launcher-tui" || profileTUI {
		config := fmt.Sprintf("check_for_update_on_startup=false\nmodel=\"fixture\"\nmodel_provider=\"fixture\"\nservice_tier=\"flex\"\napproval_policy=\"never\"\nsandbox_mode=\"read-only\"\n[model_providers.fixture]\nname=\"fixture\"\nbase_url=%q\nwire_api=\"responses\"\nenv_key=\"HRX_FIXTURE_KEY\"\nrequest_max_retries=0\nstream_max_retries=0\n[projects.%q]\ntrust_level=\"trusted\"\n", endpoint.URL+"/v1", home)
		if err := os.WriteFile(filepath.Join(home, "codex", "config.toml"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		if profileTUI {
			profile := "developer_instructions=\"HRX profile instruction proof\"\nmodel_reasoning_effort=\"low\"\n" + strings.Replace(config, "model=\"fixture\"", "model=\"fixture-profile\"", 1)
			if err := os.WriteFile(filepath.Join(home, "codex", "fixture-profile.config.toml"), []byte(profile), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if mutation == "launcher-tui-profile-project" {
		if err := os.Mkdir(filepath.Join(home, ".codex"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("developer_instructions=\""+wantInstructions+"\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"-I", supervisor, "--summary", receipt, "--grace", "3", "--", binary,
		"app-server"}
	args = append(args, plan.ServerArgs...)
	args = append(args, "--listen", "unix://"+socket)
	command := exec.CommandContext(ctx, "python3", args...)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CODEX_HOME=" + filepath.Join(home, "codex"), "XDG_CONFIG_HOME=" + home, "XDG_STATE_HOME=" + home, "XDG_DATA_HOME=" + home, "XDG_CACHE_HOME=" + home, "TMPDIR=" + filepath.Join(home, "tmp"), "HRX_FIXTURE_KEY=fixture-only", "LANG=C.UTF-8"}
	command.Dir = home
	command.Stdout, command.Stderr = io.Discard, io.Discard
	var stderr nativeErrorOutput
	if rejectFeature {
		command.Stderr = &stderr
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = 10 * time.Second
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = command.Wait(); close(exited) }()
	defer func() {
		cancel()
		select {
		case <-exited:
			if command.ProcessState == nil || !command.ProcessState.Success() {
				t.Error("native supervisor exit unverified")
			}
		case <-time.After(15 * time.Second):
			t.Error("native supervisor join unverified")
			return
		}
		var report struct{ Subreaper, Complete, Forced bool }
		raw, err := os.ReadFile(receipt)
		if err != nil || json.Unmarshal(raw, &report) != nil || !report.Subreaper || !report.Complete || report.Forced {
			t.Error("native descendants not proven clean")
			return
		}
		t.Log("native baseline cleanup: subreaper=true complete=true forced=false")
		wantRequests := int64(0)
		if compact || profileTUI {
			wantRequests = 1
		}
		if requests.Load() != wantRequests {
			t.Errorf("native fixture provider requests=%d want=%d", requests.Load(), wantRequests)
		}
	}()
	if rejectFeature {
		select {
		case <-exited:
		case <-ctx.Done():
			t.Fatal("unknown feature did not fail before server startup")
		}
		var leader struct{ Status int }
		raw, err := os.ReadFile(receipt + ".main")
		if err != nil || json.Unmarshal(raw, &leader) != nil || leader.Status == 0 ||
			!strings.Contains(stderr.data.String(), "Unknown feature flag: "+unknownFeature) {
			t.Fatal("native CLI did not reject the unknown feature with the expected diagnostic")
		}
		t.Log("planner preserved native unknown-feature rejection before bootstrap")
		return
	}
	dialer := websocket.Dialer{HandshakeTimeout: time.Second, NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	dialCtx, cancelDial := context.WithTimeout(ctx, 10*time.Second)
	defer cancelDial()
	var conn *websocket.Conn
	for {
		var response *http.Response
		conn, response, err = dialer.DialContext(dialCtx, "ws://localhost/", nil)
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if err == nil {
			break
		}
		select {
		case <-dialCtx.Done():
			t.Fatal("native baseline socket not ready")
		case <-time.After(50 * time.Millisecond):
		}
	}
	if mutation == "launcher-tui" || profileTUI {
		nativeLauncherTUI(t, ctx, conn, home, binary, command.Env, profileTUI, providerSeen)
		return
	}
	if mutation == "launcher-runtime" {
		raw, err := json.Marshal(plan.Start)
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		nativeLauncherRuntime(t, ctx, conn, home, raw)
		return
	}
	wire, err := codexrpc.NewWebSocketWire(conn, codexrpc.DefaultWebSocketOptions())
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer stopCancel()
		if _, err := wire.Stop(stopCtx); err != nil {
			t.Error("native websocket cleanup", err)
		}
	}()
	client, err := codexrpc.New(wire, "native-baseline-fixture", codexrpc.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer stopCancel()
		if _, err := client.Stop(stopCtx); err != nil {
			t.Error("native client cleanup", err)
		}
	}()
	binding, err := client.Admit()
	if err != nil {
		t.Fatal(err)
	}
	g := &codexGate{client: client, binding: binding}
	if mutation == "gateway-start" || mutation == "gateway-resume" {
		raw, err := json.Marshal(plan.Start)
		if err != nil {
			t.Fatal(err)
		}
		if mutation == "gateway-resume" {
			thread := seedNativeThread(t, ctx, &dialer, raw)
			raw, err = json.Marshal(map[string]any{"threadId": thread, "excludeTurns": true})
			if err != nil {
				t.Fatal(err)
			}
		}
		nativeGatewaySocket(t, ctx, g, home, raw)
		return
	}
	b, err := collectBootstrap(ctx, g, plan.Start, "fixture", 128, 4<<20, 1<<20)
	if err != nil {
		t.Fatal("native bootstrap", err)
	}
	if b.native.Model != model || b.native.ModelProvider != "fixture" || b.native.Cwd != home || b.native.Approval.Kind != "never" || b.native.Sandbox.Type != "readOnly" {
		t.Fatal("native baseline changed explicit fixture intent")
	}
	if !b.native.ThreadSource.Present || b.native.ThreadSource.Value != "user" {
		t.Fatal("native baseline did not preserve TUI thread source")
	}
	parity, err := codexlaunch.Compare(plan, b.native, b.native.Identity)
	if err != nil {
		t.Fatal("native launch comparison", err)
	}
	if serviceTier == "fast" && model == "fixture" {
		// Pinned session::get_service_tier drops priority for unadvertised models.
		if b.native.ServiceTier.Present || parity.Fields["service_tier"] != codexlaunch.Unreported || parity.Status != "blocked" {
			t.Fatal("unsupported fast tier was falsely matched", parity)
		}
		if err := g.revoke(); err != nil {
			t.Fatal(err)
		}
		if _, ok := client.Current(); ok {
			t.Fatal("unsupported fast tier published authority")
		}
		t.Log("unadvertised fast tier returned null; parity refused before journal staging")
		return
	}
	for _, field := range []string{"model", "model_provider", "cwd", "approval_policy", "sandbox.mode", "thread_source", "service_tier"} {
		if parity.Fields[field] != codexlaunch.Matched {
			t.Fatalf("planned native field %s: %s", field, parity.Fields[field])
		}
	}
	if parity.Status != "blocked" || len(parity.Blockers) == 0 {
		t.Fatalf("fixture requires unresolved admission prerequisites: status=%s fields=%v blockers=%v", parity.Status, parity.Fields, parity.Blockers)
	}
	t.Logf("planner server/start matched seven explicit native fields; service tier requested=%s returned=%s; production admission remains blocked", serviceTier, b.native.ServiceTier.Value)
	through := b.prefix.consumed
	var compactDigest [32]byte
	journal, err := coveragejournal.Open(filepath.Join(home, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	store := &afterSyncStore{journal: journal, synced: make(chan struct{}), release: make(chan struct{})}
	defer store.unblock()
	budget, cancelBudget := context.WithTimeout(ctx, 5*time.Second)
	defer cancelBudget()
	c, err := newBaselineCoordinator(budget, g, b, store, 32, 8)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	reports := make(chan serveReport, 1)
	go func() { reports <- c.serve(runCtx, g, time.Second, 100*time.Millisecond) }()
	select {
	case <-store.synced:
	case <-budget.Done():
		t.Fatal("native baseline did not sync")
	}
	if _, ok := client.Current(); ok {
		t.Fatal("native baseline published before storage result")
	}
	if loseAfterSync {
		wire.Close()
	} else {
		store.unblock()
		waitCurrent(t, g)
		current, ok := client.Current()
		history := journal.History()
		if !ok || len(history) != 1 || current.Through != through || current.Binding != binding || current.Digest != stageDigest(history[0]) {
			t.Fatal("native baseline publication mismatch")
		}
		if mutation != "" {
			// A separate owner triggers the mutation without introducing an unmerged
			// RPC response ordinal into the controller's observed connection.
			defer nativeMutateThread(t, ctx, &dialer, b.native.Identity.ThreadID, mutation)()
			if compact {
				deadline := time.NewTimer(10 * time.Second)
				defer deadline.Stop()
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for {
					current, ok := client.Current()
					if ok && current.Through > through {
						history := journal.History()
						var pair pairEvidence
						if len(history) != 2 || history[1].Operation != "2" || current.Binding != binding ||
							current.Digest != stageDigest(history[1]) || current.Through != history[1].Prefix ||
							json.Unmarshal(history[1].Payload, &pair) != nil || pair.Schema != 1 ||
							pair.Thread != b.native.Identity.ThreadID || pair.Turn == "" || pair.Item == "" ||
							pair.Start <= through || pair.End <= pair.Start || pair.End != current.Through {
							t.Fatal("native compaction stage/publication mismatch")
						}
						compactDigest = current.Digest
						break
					}
					select {
					case report := <-reports:
						t.Fatalf("native compaction stopped before publication: %+v", report)
					case <-deadline.C:
						t.Fatal("native compaction did not publish a durable pair")
					case <-tick.C:
					}
				}
				stopRun()
			}
		} else {
			stopRun()
		}
	}
	report := serveResult(t, reports)
	if mutation != "" && !compact && !errors.Is(report.Cause, errThreadBoundary) {
		t.Fatalf("native %s did not invalidate through a lifecycle notification: %+v", mutation, report)
	}
	if report.Cause == nil || !report.ReceiverJoined {
		t.Fatal(report)
	}
	if loseAfterSync {
		if report.WriterJoined || report.CleanupError == nil {
			t.Fatal("blocked native baseline storage reported joined", report)
		}
	} else if !report.WriterJoined || report.CleanupError != nil {
		t.Fatal(report)
	}
	store.unblock()
	if err := c.writer.join(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := client.Current(); ok {
		t.Fatal("late native baseline restored authority")
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := coveragejournal.Open(filepath.Join(home, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	history := reopened.History()
	wantRecords := 1
	if compact {
		wantRecords = 2
	}
	if len(history) != wantRecords || history[0].Connection != binding.Nonce || history[0].Revision != binding.Revision || history[0].Prefix != through {
		t.Fatal("native baseline history binding lost")
	}
	if compact && stageDigest(history[1]) != compactDigest {
		t.Fatal("native compaction history changed after reopen")
	}
	t.Logf("native owned bootstrap prefix=%d durable baseline retained; current authority revoked; loss-after-sync=%v mutation-after-publish=%q", through, loseAfterSync, mutation)
}

// Scripted Responses output exercises native compaction without model inference.
func nativeCompactionResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	item := map[string]any{"id": "msg_fixture_summary", "type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": "Fixture compaction summary.", "annotations": []any{}}}}
	events := []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp_fixture_compact", "object": "response", "status": "in_progress", "output": []any{}}},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": map[string]any{"id": "resp_fixture_compact", "object": "response", "status": "completed", "output": []any{item}}},
	}
	for i, event := range events {
		event["sequence_number"] = i
		raw, _ := json.Marshal(event)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], raw)
	}
}

func nativeMutateThread(t *testing.T, parent context.Context, dialer *websocket.Dialer, thread, method string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	conn, response, err := dialer.DialContext(ctx, "ws://localhost/", nil)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		t.Fatal("mutation control connection", err)
	}
	wire, err := codexrpc.NewWebSocketWire(conn, codexrpc.DefaultWebSocketOptions())
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	stopWire := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer stopCancel()
		if _, err := wire.Stop(stopCtx); err != nil {
			t.Error("mutation control wire cleanup", err)
		}
	}
	t.Cleanup(stopWire)
	client, err := codexrpc.New(wire, "native-mutation-fixture", codexrpc.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	stopClient := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer stopCancel()
		if _, err := client.Stop(stopCtx); err != nil {
			t.Error("mutation control client cleanup", err)
		}
	}
	t.Cleanup(stopClient)
	if _, err := client.Call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "hrx-mutation-fixture", "version": "1"},
		"capabilities": map[string]bool{"experimentalApi": true},
	}); err != nil {
		t.Fatal("mutation control initialize", err)
	}
	if err := client.Notify(ctx, "initialized", nil); err != nil {
		t.Fatal(err)
	}
	params := map[string]string{"threadId": thread}
	if method == "thread/settings/update" {
		params["approvalPolicy"] = "on-request"
	}
	if _, err := client.Call(ctx, method, params); err != nil {
		t.Fatal("native mutation", method, err)
	}
	return func() {
		stopClient()
		stopWire()
	}
}
