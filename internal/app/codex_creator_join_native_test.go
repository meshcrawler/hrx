package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
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
)

// Dial only: initialization must occur after the exclusive reader takes ownership.
func creatorFixtureConnection(t *testing.T, parent context.Context, socket string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	dialer := websocket.Dialer{HandshakeTimeout: time.Second,
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}
	for {
		conn, response, err := dialer.DialContext(ctx, "ws://localhost/", nil)
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if err == nil {
			return conn
		}
		select {
		case <-ctx.Done():
			t.Fatal("private creator socket unavailable before deadline")
			return nil
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func creatorFixtureEnv(t *testing.T, home string) []string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	return []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home, "LANG=C.UTF-8",
		"CODEX_HOME=" + filepath.Join(home, "codex"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_DATA_HOME=" + filepath.Join(home, "data"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"TMPDIR=" + filepath.Join(home, "tmp"),
		"TERM=xterm-256color", "NO_COLOR=1", "HRX_FIXTURE_KEY=fixture-only",
	}
}

func creatorOwnedProcess(t *testing.T, parent context.Context, binary string, args []string, home, label string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	supervisor, err := filepath.Abs("../../tests/compatibility/subreaper.py")
	if err != nil {
		t.Fatal(err)
	}
	summary := filepath.Join(home, label+"-cleanup.json")
	ownedArgs := append([]string{"-I", supervisor, "--summary", summary, "--grace", "3", "--", binary}, args...)
	command := exec.CommandContext(ctx, "python3", ownedArgs...)
	command.Env, command.Dir = creatorFixtureEnv(t, home), home
	command.Stdout, command.Stderr = io.Discard, io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	// Allow the subreaper to reap forced children and publish failure evidence
	// before CommandContext's final process kill can destroy that boundary.
	command.WaitDelay = 10 * time.Second
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	return func() {
		cancel()
		select {
		case <-done:
			// CommandContext may return context.Canceled after graceful exit;
			// the native supervisor's actual exit status remains authoritative.
			if command.ProcessState == nil || !command.ProcessState.Success() {
				t.Error("native ownership supervisor did not exit successfully")
			}
		case <-time.After(15 * time.Second):
			t.Error("native ownership supervisor did not join; cleanup unknown")
			return
		}
		var receipt struct{ Subreaper, Complete, Forced bool }
		raw, err := os.ReadFile(summary)
		if err != nil || json.Unmarshal(raw, &receipt) != nil || !receipt.Subreaper || !receipt.Complete || receipt.Forced {
			t.Error("native descendant cleanup lacks an unforced complete subreaper receipt")
			return
		}
		t.Log("native descendant cleanup: subreaper=true complete=true forced=false")
	}
}

// Tests the native server's creator/join contract, NOT a real TUI attachment,
// production admission, durable coverage, or paid model semantics.
func TestNativeCodexCreatorSurvivesSecondClientJoin(t *testing.T) {
	testNativeCodexCreatorJoin(t, false)
}

func TestNativeCodexCreatorSurvivesTUIJoin(t *testing.T) {
	testNativeCodexCreatorJoin(t, true)
}

func testNativeCodexCreatorJoin(t *testing.T, useTUI bool) {
	if os.Getenv("HRX_NATIVE_SESSION_TESTS") != "1" {
		t.Skip("GitLab native contract: private server and loopback model only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("HRX_FIXTURE_KEY", "fixture-only")
	if err := os.MkdirAll(os.Getenv("CODEX_HOME"), 0700); err != nil {
		t.Fatal(err)
	}
	prompt := "HRX TUI fixture marker " + rand.Text() + ". Reply without tools."
	var requests atomic.Int64
	var sawPrompt, wrongModel, unexpectedRequest atomic.Bool
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
			unexpectedRequest.Store(true)
			http.Error(w, "unexpected fixture endpoint", http.StatusBadRequest)
			return
		}
		requests.Add(1)
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
		if strings.Contains(string(body), prompt) {
			sawPrompt.Store(true)
		}
		var request struct{ Model string }
		if json.Unmarshal(body, &request) != nil || request.Model != "fixture" {
			wrongModel.Store(true)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"creator-fixture\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer gateway.Close()
	if useTUI {
		t.Setenv("TERM", "xterm-256color")
		config := fmt.Sprintf("check_for_update_on_startup = false\nmodel = \"fixture\"\nmodel_provider = \"fixture\"\n[model_providers.fixture]\nname = \"fixture\"\nbase_url = %q\nwire_api = \"responses\"\nenv_key = \"HRX_FIXTURE_KEY\"\nrequest_max_retries = 0\nstream_max_retries = 0\n[projects.%q]\ntrust_level = \"trusted\"\n", gateway.URL+"/v1", home)
		if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	socket := filepath.Join(home, "server.sock")
	args := []string{"app-server", "-c", `model_provider="fixture"`, "-c",
		fmt.Sprintf(`model_providers.fixture={name="fixture",base_url=%q,wire_api="responses",env_key="HRX_FIXTURE_KEY",request_max_retries=0,stream_max_retries=0}`, gateway.URL+"/v1"),
		"--listen", "unix://" + socket}
	stopServer := creatorOwnedProcess(t, ctx, binary, args, home, "server")
	defer stopServer()
	var clients []*codexrpc.Client
	var wires []*codexrpc.WebSocketWire
	// Call replies and Next notifications share wire ordinals but are delivered
	// separately. Retain both until the pre-work baseline prefix is accounted for.
	ordinals := make(map[*codexrpc.Client]map[uint64]bool)
	generations := make(map[*codexrpc.Client]string)
	remember := func(client *codexrpc.Client, ordinal uint64, generation string) {
		t.Helper()
		seen := ordinals[client]
		if ordinal == 0 || generation != generations[client] || seen[ordinal] || len(seen) >= 1024 {
			t.Fatal("invalid, duplicate, foreign or excessive creator evidence")
		}
		seen[ordinal] = true
	}
	defer func() {
		for i := len(clients) - 1; i >= 0; i-- {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := clients[i].Stop(stopCtx)
			wireReport, wireErr := wires[i].Stop(stopCtx)
			stopCancel()
			if err != nil {
				t.Error("observer transport cleanup unverified:", err)
			}
			if wireErr != nil || !wireReport.ReaderStopped || !wireReport.WritersStopped || !wireReport.HeartbeatStopped || !wireReport.TransportClosed {
				t.Error("observer WebSocket worker cleanup unverified")
			}
		}
	}()
	connect := func(generation string) *codexrpc.Client {
		t.Helper()
		conn := creatorFixtureConnection(t, ctx, socket)
		wire, err := codexrpc.NewWebSocketWire(conn, codexrpc.DefaultWebSocketOptions())
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		client, err := codexrpc.New(wire, generation, codexrpc.DefaultLimits())
		if err != nil {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = wire.Stop(stopCtx)
			stopCancel()
			t.Fatal(err)
		}
		clients = append(clients, client)
		wires = append(wires, wire)
		ordinals[client] = make(map[uint64]bool)
		generations[client] = generation
		initialized, err := client.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "hrx-creator-fixture", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}})
		if err != nil || initialized.Ordinal == 0 || initialized.Generation != generation {
			t.Fatal("owned initialize response lacks exact ordinal/generation", err)
		}
		remember(client, initialized.Ordinal, initialized.Generation)
		if err := client.Notify(ctx, "initialized", nil); err != nil {
			t.Fatal("owned initialized notification failed", err)
		}
		return client
	}
	type threadResult struct {
		Thread struct {
			ID, SessionID, Cwd, Model, HistoryMode, Name string
			Turns                                        []json.RawMessage
		}
		Model string
	}
	call := func(c *codexrpc.Client, method string, params any, result any) codexrpc.Response {
		t.Helper()
		r, err := c.Call(ctx, method, params)
		if err != nil {
			var remote struct {
				Code    int
				Message string
			}
			_ = json.Unmarshal(r.RemoteError, &remote)
			category := "unclassified"
			for _, fixed := range []string{"list_turns is not supported yet", "paginated_history is not supported yet", "unavailable without sqlite state", "not supported yet", "not materialized", "not found", "persisted history", "active writer", "unknown model", "unsupported", "invalid params", "rollout"} {
				if strings.Contains(strings.ToLower(remote.Message), fixed) {
					category = fixed
					break
				}
			}
			t.Fatalf("%s failed: %v; remote_code=%d category=%s", method, err, remote.Code, category)
		}
		if result != nil {
			if err := json.Unmarshal(r.Result, result); err != nil {
				t.Fatal("invalid response shape:", err)
			}
		}
		remember(c, r.Ordinal, r.Generation)
		return r
	}
	creator := connect("creator")
	var started threadResult
	// Match the pinned TUI's persistent thread/start history mode. The legacy
	// default is a different startup contract and may not materialize a fresh root.
	startResponse := call(creator, "thread/start", map[string]any{"model": "fixture", "modelProvider": "fixture", "cwd": home,
		"approvalPolicy": "never", "sandbox": "read-only", "ephemeral": false, "historyMode": "paginated"}, &started)
	native, err := codexlaunch.ParseThreadStart(startResponse.Result)
	if err != nil {
		t.Fatal("strict native start evidence rejected", err)
	}
	if native.Model != "fixture" || native.ModelProvider != "fixture" || native.Cwd != home ||
		native.Approval.Kind != "never" || native.Sandbox.Type != "readOnly" {
		t.Fatal("native start changed explicit fixture routing or permissions")
	}
	if started.Thread.ID == "" || started.Thread.SessionID == "" {
		t.Fatal("missing native thread/session identity")
	}
	if started.Thread.HistoryMode != "paginated" {
		t.Fatal("native start did not accept paginated history")
	}
	defer func() {
		if !t.Failed() {
			return
		}
		// Inspect only this fixture's storage, read-only, without exposing paths,
		// native messages or transcript contents. The original failure stays fatal.
		diagnosticCtx, diagnosticCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer diagnosticCancel()
		const script = `import json, pathlib, sqlite3, sys
root = pathlib.Path(sys.argv[1])
report = {"state_files": 0, "matching_rows": 0, "paginated_rows": 0, "legacy_rows": 0, "other_mode_rows": 0, "read_errors": 0, "rollout_files": 0}
for path in list(root.rglob("state_*.sqlite"))[:16]:
    report["state_files"] += 1
    try:
        with sqlite3.connect(path.as_uri() + "?mode=ro", uri=True, timeout=1) as db:
            rows = db.execute("SELECT history_mode FROM threads WHERE id = ?", (sys.argv[2],)).fetchall()
        report["matching_rows"] += len(rows)
        for (mode,) in rows:
            report[{"paginated": "paginated_rows", "legacy": "legacy_rows"}.get(mode, "other_mode_rows")] += 1
    except sqlite3.Error:
        report["read_errors"] += 1
report["rollout_files"] = sum(1 for _ in root.rglob("rollout-*.jsonl"))
print(json.dumps(report, sort_keys=True))
`
		diagnostic := exec.CommandContext(diagnosticCtx, "python3", "-c", script, home, started.Thread.ID)
		diagnostic.Env = creatorFixtureEnv(t, home)
		output, err := diagnostic.Output()
		if err != nil {
			t.Log("fixture storage diagnostic unavailable")
			return
		}
		t.Log("fixture storage diagnostic:", strings.TrimSpace(string(output)))
	}()
	// The pinned store explicitly saves newly named paginated threads before
	// their first turn. Naming is a visible metadata mutation, not inference.
	const fixtureName = "HRX creator admission fixture"
	call(creator, "thread/name/set", map[string]any{"threadId": started.Thread.ID, "name": fixtureName}, nil)
	// Reading alone materialized a rollout but left no SQLite metadata in the
	// retained failing diagnostic. Require the complete named empty baseline.
	var baseline threadResult
	baselineResponse := call(creator, "thread/read", map[string]any{"threadId": started.Thread.ID, "includeTurns": true}, &baseline)
	if err := native.ValidateFreshBaseline(baselineResponse.Result, fixtureName); err != nil {
		t.Fatal("strict native fresh baseline rejected", err)
	}
	baselineOrdinal := baselineResponse.Ordinal
	if baseline.Thread.ID != started.Thread.ID || baseline.Thread.SessionID != started.Thread.SessionID ||
		baseline.Thread.HistoryMode != "paginated" || baseline.Thread.Name != fixtureName ||
		baseline.Thread.Turns == nil || len(baseline.Thread.Turns) != 0 {
		t.Fatal("baseline did not preserve the empty paginated native session")
	}
	if baselineOrdinal > 1024 {
		t.Fatal("baseline prefix exceeded fixture evidence bound")
	}
	for ordinal := uint64(1); ordinal <= baselineOrdinal; ordinal++ {
		for !ordinals[creator][ordinal] {
			message, err := creator.Next(ctx)
			if err != nil {
				t.Fatal("creator prefix lost before baseline admission", err)
			}
			remember(creator, message.Ordinal, message.Generation)
			if message.Kind == codexrpc.ServerRequest || message.Method == "turn/started" || message.Method == "turn/completed" {
				t.Fatal("unexpected work or server request before baseline admission")
			}
		}
	}
	t.Logf("exclusive creator consumed contiguous prefix 1..%d before UI/queue work", baselineOrdinal)
	if requests.Load() != 0 {
		t.Fatal("creation/baseline unexpectedly invoked the model")
	}
	var turn struct{ Turn struct{ ID string } }
	if useTUI {
		helper, err := filepath.Abs("../../tests/compatibility/creator_tui_pty.py")
		if err != nil {
			t.Fatal(err)
		}
		stopTUI := creatorOwnedProcess(t, ctx, "python3", []string{"-I", helper, binary,
			"--remote", "unix://" + socket, "--no-alt-screen", "resume", started.Thread.ID, prompt}, home, "tui")
		defer stopTUI()
	} else {
		joiner := connect("joiner")
		var joined threadResult
		call(joiner, "thread/resume", map[string]any{"threadId": started.Thread.ID, "model": "conflicting-fixture-model"}, &joined)
		if joined.Thread.ID != started.Thread.ID || joined.Thread.SessionID != started.Thread.SessionID || joined.Model != "fixture" {
			t.Fatal("subscribed loaded thread was replaced or conflicting model accepted")
		}
		if requests.Load() != 0 {
			t.Fatal("creation/join unexpectedly invoked the model")
		}
		call(joiner, "turn/start", map[string]any{"threadId": started.Thread.ID,
			"input": []map[string]string{{"type": "text", "text": "Fixture turn, no tools."}}}, &turn)
		if turn.Turn.ID == "" {
			t.Fatal("missing native turn identity")
		}
	}
	seenStart := false
	for {
		message, err := creator.Next(ctx)
		if err != nil {
			t.Fatal("creator stream lost before correlated completion:", err)
		}
		var event struct {
			Params struct {
				ThreadID string
				Turn     struct{ ID, Status string }
			}
		}
		if err := json.Unmarshal(message.Raw, &event); err != nil {
			t.Fatal(err)
		}
		if useTUI && turn.Turn.ID == "" && message.Method == "turn/started" && event.Params.ThreadID == started.Thread.ID {
			turn.Turn.ID = event.Params.Turn.ID
		}
		if event.Params.ThreadID != started.Thread.ID || event.Params.Turn.ID == "" || event.Params.Turn.ID != turn.Turn.ID {
			continue
		}
		if message.Method == "turn/started" {
			seenStart = true
		}
		if message.Method == "turn/completed" {
			if !seenStart || event.Params.Turn.Status != "completed" || requests.Load() == 0 {
				t.Fatal("missing ordered creator start, successful completion or fixture request")
			}
			break
		}
	}
	if creator.Snapshot().State != codexrpc.TransportOpen {
		t.Fatal("creator transport lost at acceptance")
	}
	if wrongModel.Load() || unexpectedRequest.Load() {
		t.Fatal("join invoked an unexpected fixture model or endpoint")
	}
	if useTUI {
		if !sawPrompt.Load() {
			t.Fatal("TUI prompt did not reach the expected fixture model")
		}
		var after threadResult
		call(creator, "thread/read", map[string]any{"threadId": started.Thread.ID, "includeTurns": true}, &after)
		if after.Thread.ID != started.Thread.ID || after.Thread.SessionID != started.Thread.SessionID {
			t.Fatal("TUI join replaced the native session")
		}
		if len(after.Thread.Turns) != 1 {
			t.Fatal("TUI fixture did not produce exactly one durable turn")
		}
		var stored struct {
			ID, Status string
			Items      []struct {
				Type    string
				Content []struct{ Type, Text string }
			}
		}
		if json.Unmarshal(after.Thread.Turns[0], &stored) != nil || stored.ID != turn.Turn.ID || stored.Status != "completed" {
			t.Fatal("durable TUI turn does not match creator-observed completion")
		}
		markerItems := 0
		for _, item := range stored.Items {
			if item.Type != "userMessage" {
				continue
			}
			for _, content := range item.Content {
				if content.Type == "text" && strings.Contains(content.Text, prompt) {
					markerItems++
				}
			}
		}
		if markerItems != 1 {
			t.Fatal("unique TUI prompt is not bound to the creator-observed durable turn")
		}
		t.Log("actual TUI prompt observed by creator with unchanged native session")
	}
}
