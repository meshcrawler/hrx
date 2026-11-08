package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/meshcrawler/hrx/internal/codexlaunch"
)

type freshStartCapture struct{ params, result json.RawMessage }

type freshProxyReporter interface {
	Helper()
	Fatal(...any)
	Error(...any)
	Cleanup(func())
}

// A diagnostic proxy, not an admission implementation. It retains only one
// bounded startup request/response and refuses turn submission in this fixture.
func freshStartProxy(t freshProxyReporter, parent context.Context, upstream, socket string) (<-chan freshStartCapture, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	receipts := make(chan freshStartCapture, 1)
	failures := make(chan error, 1)
	var protocolFault atomic.Bool
	report := func(err error) {
		select {
		case failures <- err:
		default:
		}
	}
	fail := func(err error) {
		protocolFault.Store(true)
		report(err)
	}
	var accepted atomic.Bool
	var attemptedWork atomic.Bool
	var mu sync.Mutex
	var connections []*websocket.Conn
	var workers sync.WaitGroup
	server := &http.Server{ReadHeaderTimeout: time.Second}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workers.Add(1)
		defer workers.Done()
		if !accepted.CompareAndSwap(false, true) {
			fail(errors.New("fresh TUI opened a second proxy connection"))
			http.Error(w, "single connection fixture", http.StatusConflict)
			return
		}
		client, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			fail(errors.New("fresh TUI websocket upgrade failed"))
			return
		}
		defer client.Close()
		mu.Lock()
		connections = append(connections, client)
		mu.Unlock()
		dialer := websocket.Dialer{HandshakeTimeout: time.Second, NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", upstream)
		}}
		native, response, err := dialer.DialContext(ctx, "ws://localhost/", nil)
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if err != nil {
			fail(errors.New("fresh TUI upstream dial failed"))
			return
		}
		defer native.Close()
		mu.Lock()
		connections = append(connections, native)
		mu.Unlock()
		client.SetReadLimit(1 << 20)
		native.SetReadLimit(1 << 20)
		var captureMu sync.Mutex
		var requestID string
		var params json.RawMessage
		finished := make(chan struct{}, 2)
		copyFrames := func(dst, src *websocket.Conn, fromTUI bool) {
			defer func() { finished <- struct{}{} }()
			for n := 0; n < 2048; n++ {
				kind, raw, err := src.ReadMessage()
				if err != nil {
					report(errors.New("fresh TUI proxy stream ended"))
					return
				}
				var envelope struct {
					ID, Params, Result, Error json.RawMessage
					Method                    string
				}
				if kind != websocket.TextMessage || json.Unmarshal(raw, &envelope) != nil {
					fail(errors.New("invalid fresh TUI proxy frame"))
					return
				}
				if fromTUI && envelope.Method == "turn/start" {
					attemptedWork.Store(true)
					fail(errors.New("fresh TUI attempted work before capture completed"))
					return
				}
				captureMu.Lock()
				if fromTUI && envelope.Method == "thread/start" {
					if requestID != "" || len(envelope.ID) == 0 {
						captureMu.Unlock()
						fail(errors.New("duplicate or unbound fresh TUI start"))
						return
					}
					requestID, params = string(envelope.ID), append(json.RawMessage(nil), envelope.Params...)
				}
				matched := !fromTUI && requestID != "" && string(envelope.ID) == requestID
				capture := freshStartCapture{params, append(json.RawMessage(nil), envelope.Result...)}
				captureMu.Unlock()
				if matched && (len(envelope.Error) != 0 || len(envelope.Result) == 0) {
					fail(errors.New("native rejected fresh TUI start"))
					return
				}
				if err := dst.WriteMessage(kind, raw); err != nil {
					report(errors.New("fresh TUI proxy write failed"))
					return
				}
				if matched {
					select {
					case receipts <- capture:
					default:
						fail(errors.New("duplicate fresh TUI start receipt"))
						return
					}
				}
			}
			fail(errors.New("fresh TUI proxy frame budget exhausted"))
		}
		go copyFrames(native, client, true)
		go copyFrames(client, native, false)
		<-finished
		client.Close()
		native.Close()
		<-finished
	})
	listener, err := net.Listen("unix", socket)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		cancel()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer stopCancel()
		if err := server.Shutdown(stopCtx); err != nil {
			t.Error("fresh proxy HTTP shutdown", err)
		}
		mu.Lock()
		for _, conn := range connections {
			conn.Close()
		}
		mu.Unlock()
		done := make(chan struct{})
		go func() { workers.Wait(); close(done) }()
		select {
		case <-done:
		case <-stopCtx.Done():
			t.Error("fresh proxy workers did not join")
		}
		if attemptedWork.Load() {
			t.Error("fresh TUI attempted turn submission")
		}
		// Receipt consumption ends the test body's select, but protocol violations
		// arriving later must still fail the test after every worker has joined.
		if protocolFault.Load() {
			t.Error("fresh TUI proxy observed a protocol violation")
		}
	})
	return receipts, failures
}

// Records expected fixture failures without suppressing failures of the regression itself.
type freshProxyFailureRecorder struct {
	*testing.T
	cleanups []func()
	errors   []string
	once     sync.Once
}

func (r *freshProxyFailureRecorder) Cleanup(f func()) { r.cleanups = append(r.cleanups, f) }
func (r *freshProxyFailureRecorder) Error(args ...any) {
	r.errors = append(r.errors, fmt.Sprint(args...))
}
func (r *freshProxyFailureRecorder) finish() {
	r.once.Do(func() {
		for i := len(r.cleanups) - 1; i >= 0; i-- {
			r.cleanups[i]()
		}
	})
}

func TestFreshStartProxyReportsViolationAfterReceipt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	home := t.TempDir()
	upstream := filepath.Join(home, "upstream.sock")
	listener, err := net.Listen("unix", upstream)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"id":1,"result":{}}`)); err != nil {
			return
		}
		_, _, _ = conn.ReadMessage()
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	recorder := &freshProxyFailureRecorder{T: t}
	t.Cleanup(recorder.finish)
	socket := filepath.Join(home, "proxy.sock")
	receipts, failures := freshStartProxy(recorder, ctx, upstream, socket)
	dialer := websocket.Dialer{HandshakeTimeout: time.Second, NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	client, _, err := dialer.DialContext(ctx, "ws://localhost/", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.WriteMessage(websocket.TextMessage, []byte(`{"id":1,"method":"thread/start","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-receipts:
	case err := <-failures:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("startup receipt not produced")
	}
	second, response, err := dialer.DialContext(ctx, "ws://localhost/", nil)
	if second != nil {
		second.Close()
	}
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusConflict {
		t.Fatal("duplicate connection was not refused")
	}
	// Deliberately do not drain failures: this is the post-receipt blind spot.
	recorder.finish()
	if !slices.Equal(recorder.errors, []string{"fresh TUI proxy observed a protocol violation"}) {
		t.Fatalf("late violation cleanup reports = %v", recorder.errors)
	}
}

func TestNativeCodexFreshTUIStartRequest(t *testing.T) {
	if os.Getenv("HRX_NATIVE_SESSION_TESTS") != "1" {
		t.Skip("GitLab native TUI capture only")
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"inherited-user-config", nil},
		{"selected-profile", []string{"--profile", "fixture-profile"}},
		{"explicit", []string{"--model", "fixture", "-c", `model_provider="fixture"`, "-c", `service_tier="flex"`, "--ask-for-approval", "never", "--sandbox", "read-only", "--search"}},
		{"config-only", []string{"-c", `model="fixture"`, "-c", `model_provider="fixture"`, "-c", `service_tier="flex"`, "-c", `approval_policy="never"`, "-c", `sandbox_mode="read-only"`, "-c", `web_search="disabled"`}},
		{"typed-precedence", []string{"--model", "fixture", "--ask-for-approval", "never", "--sandbox", "read-only", "--search", "-c", `model="overridden-model"`, "-c", `model_provider="fixture"`, "-c", `service_tier="flex"`, "-c", `approval_policy="on-request"`, "-c", `sandbox_mode="workspace-write"`, "-c", `web_search="disabled"`}},
		{"generated-instructions", []string{"--model", "fixture", "-c", `model_provider="fixture"`, "-c", `service_tier="flex"`, "--ask-for-approval", "never", "--sandbox", "read-only", "--search", "--enable", "terminal_visualization_instructions", "-c", `developer_instructions="Fixture developer instruction."`}},
		{"generated-instructions-config", []string{"--model", "fixture", "-c", `model_provider="fixture"`, "-c", `service_tier="flex"`, "--ask-for-approval", "never", "--sandbox", "read-only", "--search", "-c", `features.terminal_visualization_instructions=true`, "-c", `developer_instructions="Fixture developer instruction."`}},
		{"generated-instructions-table", []string{"--model", "fixture", "-c", `model_provider="fixture"`, "-c", `service_tier="flex"`, "--ask-for-approval", "never", "--sandbox", "read-only", "--search", "-c", `features={terminal_visualization_instructions=true}`, "-c", `developer_instructions="Fixture developer instruction."`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testNativeCodexFreshTUIStartRequest(t, tc.args)
		})
	}
}

func testNativeCodexFreshTUIStartRequest(t *testing.T, launchArgs []string) {
	t.Helper()
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex")
	if err := os.Mkdir(codexHome, 0700); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "fresh startup forbids inference", http.StatusForbidden)
	}))
	defer provider.Close()
	defer func() {
		if requests.Load() != 0 {
			t.Error("fresh TUI startup unexpectedly invoked provider")
		}
	}()
	config := fmt.Sprintf("check_for_update_on_startup = false\nmodel = \"fixture\"\nmodel_provider = \"fixture\"\n[model_providers.fixture]\nname = \"fixture\"\nbase_url = %q\nwire_api = \"responses\"\nenv_key = \"HRX_FIXTURE_KEY\"\nrequest_max_retries = 0\nstream_max_retries = 0\n[projects.%q]\ntrust_level = \"trusted\"\n", provider.URL+"/v1", home)
	if len(launchArgs) == 0 {
		config = "service_tier = \"flex\"\napproval_policy = \"never\"\nsandbox_mode = \"read-only\"\nweb_search = \"cached\"\n" + config
	}
	selectedProfile := slices.Contains(launchArgs, "--profile")
	if selectedProfile {
		// Pinned resolve_profile_v2_config_path selects a separate user config,
		// not a legacy [profiles.NAME] table inside config.toml.
		profile := "service_tier = \"flex\"\napproval_policy = \"never\"\nsandbox_mode = \"read-only\"\nweb_search = \"disabled\"\nmodel_reasoning_effort = \"low\"\n" +
			strings.Replace(config, "model = \"fixture\"", "model = \"fixture-profile-model\"", 1)
		if err := os.WriteFile(filepath.Join(codexHome, "fixture-profile.config.toml"), []byte(profile), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	socket := filepath.Join(home, "native.sock")
	stopServer := creatorOwnedProcess(t, ctx, binary, []string{"app-server", "--listen", "unix://" + socket}, home, "fresh-server")
	defer stopServer()
	probe := creatorFixtureConnection(t, ctx, socket)
	probe.Close()
	proxySocket := filepath.Join(home, "capture.sock")
	receipts, failures := freshStartProxy(t, ctx, socket, proxySocket)
	userArgs := append([]string{"--cd", home}, launchArgs...)
	plan, err := codexlaunch.Build(codexlaunch.Input{Project: home, UserArgs: userArgs})
	if selectedProfile {
		var unsupported *codexlaunch.UnsupportedOption
		if !errors.As(err, &unsupported) || unsupported.Option != "--profile" {
			t.Fatal("planner must explicitly refuse unresolved profile selection", err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	helper, err := filepath.Abs("../../tests/compatibility/creator_tui_pty.py")
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-I", helper, binary, "--remote", "unix://" + proxySocket, "--no-alt-screen"}, userArgs...)
	stopTUI := creatorOwnedProcess(t, ctx, "python3", args, home, "fresh-tui")
	defer stopTUI()
	select {
	case receipt := <-receipts:
		var start codexlaunch.ThreadStart
		if json.Unmarshal(receipt.params, &start) != nil {
			t.Fatal("invalid captured fresh start params")
		}
		raw, _ := json.Marshal(plan.Start)
		var expected map[string]json.RawMessage
		var actual map[string]json.RawMessage
		json.Unmarshal(raw, &expected)
		json.Unmarshal(receipt.params, &actual)
		if selectedProfile {
			// Independent fixture expectations: the planner refuses profiles, so
			// this measures native behavior without claiming planner parity.
			expected = make(map[string]json.RawMessage)
			for field, value := range map[string]any{
				"model": "fixture-profile-model", "modelProvider": "fixture", "serviceTier": "flex",
				"cwd": home, "approvalPolicy": "never", "sandbox": "read-only",
				"threadSource": "user", "historyMode": "paginated", "ephemeral": false,
			} {
				expected[field], _ = json.Marshal(value)
			}
			if start.Config["web_search"] != "disabled" || start.Config["model_reasoning_effort"] != "low" {
				t.Fatal("selected profile did not forward search and reasoning effort")
			}
		}
		if len(launchArgs) == 0 {
			// Independent fixture values, never defaults inferred by the planner.
			for field, value := range map[string]string{
				"serviceTier":    "flex",
				"approvalPolicy": "never", "sandbox": "read-only",
			} {
				if _, set := expected[field]; set {
					t.Fatalf("planner invented inherited field: %s", field)
				}
				expected[field], _ = json.Marshal(value)
			}
			// StartupLaunchChoices::configure (pinned startup_launch.rs) delegates
			// model/provider to the server unless this invocation selected them.
			for _, field := range []string{"model", "modelProvider"} {
				if _, set := expected[field]; set {
					t.Fatalf("planner invented inherited field: %s", field)
				}
				expected[field] = json.RawMessage("null")
			}
			if !slices.Contains(plan.Blockers(), codexlaunch.BlockConfigLayers) {
				t.Fatal("planner allowed unresolved inherited configuration")
			}
		}
		for _, field := range []string{"model", "modelProvider", "serviceTier", "cwd", "approvalPolicy", "sandbox", "threadSource", "historyMode", "ephemeral"} {
			if string(actual[field]) != string(expected[field]) {
				// Only fixture scalar fields, bounded; never log the request/config body.
				t.Errorf("fresh TUI/fixture field mismatch: %s: got %.128q want %.128q", field, actual[field], expected[field])
			}
		}
		if !selectedProfile && start.Config["web_search"] != plan.Start.Config["web_search"] {
			t.Fatal("fresh TUI/planner search override mismatch")
		}
		if t.Failed() {
			return
		}
		if slices.Contains(launchArgs, "terminal_visualization_instructions") ||
			slices.Contains(launchArgs, `features.terminal_visualization_instructions=true`) ||
			slices.Contains(launchArgs, `features={terminal_visualization_instructions=true}`) {
			// Pinned Codex 83086188, tui/src/terminal_visualization_instructions.rs.
			const expectedInstructions = "Fixture developer instruction.\n\n" +
				"- This surface is a terminal. When the formatting rules require a visual, include one in the final answer using compact ASCII diagrams, trees, timelines, or tables.\n" +
				"- Use tables for exact mappings or comparisons rather than collapsing known mappings into prose.\n" +
				"- Use trees for hierarchy or one-to-many relationships, and diagrams or timelines for sequence, change, or state transferred between records across event order.\n" +
				"- Use only ASCII characters in visuals."
			var instructions string
			if json.Unmarshal(actual["developerInstructions"], &instructions) != nil || instructions != expectedInstructions {
				t.Fatal("native generated developer instructions differ from pinned source")
			}
			if plan.Start.DeveloperInstructions == nil || *plan.Start.DeveloperInstructions != instructions {
				t.Fatal("planner lost native terminal developer instructions")
			}
			if !slices.Contains(plan.Blockers(), codexlaunch.BlockDeveloperInstructions) {
				t.Fatal("planner allowed unresolved generated instructions")
			}
			t.Log("native generated developer instructions captured; planner parity remains explicitly blocked")
		}
		native, err := codexlaunch.ParseThreadStart(receipt.result)
		wantModel := "fixture"
		if selectedProfile {
			wantModel = "fixture-profile-model"
		}
		if err != nil || native.Model != wantModel || native.ModelProvider != "fixture" || native.Cwd != home || native.Identity.ThreadID == "" {
			t.Fatal("fresh TUI native response rejected", err)
		}
		t.Log("real fresh TUI start request matched fixture expectations; full config parity remains unverified")
	case err := <-failures:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("fresh TUI did not produce a correlated startup receipt")
	}
}
