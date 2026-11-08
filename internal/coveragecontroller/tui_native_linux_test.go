package coveragecontroller

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/meshcrawler/hrx/internal/codexlaunch"
)

// Called only by the GitLab opt-in native baseline suite. The existing PTY
// driver answers terminal position queries only; it never approves trust/work.
func nativeLauncherTUI(t *testing.T, parent context.Context, upstream *websocket.Conn, home, binary string, env []string, profile bool, providerSeen <-chan struct{}) {
	t.Helper()
	socketRoot, err := os.MkdirTemp("/tmp", "hrx-tui-")
	if err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	defer os.RemoveAll(socketRoot)
	initialInstructions := ""
	if profile {
		initialInstructions = "HRX profile instruction proof"
	}
	runtime, err := NewRuntime(parent, upstream, RuntimeOptions{ProfileDeveloperInstructions: codexlaunch.Optional[string]{Present: profile, Value: initialInstructions}, SocketRoot: socketRoot, JournalRoot: filepath.Join(home, "journal"), Name: "fixture", StageTimeout: 10 * time.Second, ShutdownTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := runtime.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	supervisor, err := filepath.Abs("../../tests/compatibility/subreaper.py")
	if err != nil {
		t.Fatal(err)
	}
	helper, err := filepath.Abs("../../tests/compatibility/creator_tui_pty.py")
	if err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(home, "tui-cleanup.json")
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	endpoint := runtime.Terminal
	captured := make(chan json.RawMessage, 1)
	methods := make(chan string, 32)
	if profile {
		endpoint = captureNativeTurn(t, ctx, socketRoot, endpoint, captured, methods)
	}
	args := []string{"-I", supervisor, "--summary", receipt, "--grace", "3", "--", "python3", "-I", helper, binary, "--remote", endpoint, "--no-alt-screen", "--cd", home}
	if profile {
		args = append(args, "--profile", "fixture-profile", "HRX native terminal prompt proof")
	}
	command := exec.CommandContext(ctx, "python3", args...)
	command.Env = append(append([]string{}, env...), "TERM=xterm-256color", "NO_COLOR=1", "HRX_TUI_DIAGNOSTIC="+filepath.Join(home, "tui-diagnostic.json"))
	command.Dir = home
	command.Stdout, command.Stderr = io.Discard, io.Discard
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
		case <-time.After(15 * time.Second):
			t.Error("native TUI supervisor did not join")
			return
		}
		var report struct{ Subreaper, Complete, Forced bool }
		raw, err := os.ReadFile(receipt)
		if command.ProcessState == nil || !command.ProcessState.Success() || err != nil || json.Unmarshal(raw, &report) != nil || !report.Subreaper || !report.Complete || report.Forced {
			t.Error("native TUI cleanup not proven")
		}
	}()
	readyCtx, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	identity, err := runtime.Ready(readyCtx)
	if err != nil {
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		cleanupErr := runtime.Close(stopCtx)
		stop()
		processExited := false
		select {
		case <-exited:
			processExited = true
			var diagnostic struct {
				ErrorLines []string `json:"error_lines"`
				Captured   int      `json:"captured_bytes"`
			}
			if raw, readErr := os.ReadFile(filepath.Join(home, "tui-diagnostic.json")); readErr == nil && json.Unmarshal(raw, &diagnostic) == nil {
				t.Logf("isolated terminal startup errors=%q captured_bytes=%d", diagnostic.ErrorLines, diagnostic.Captured)
			}
		default:
		}
		select {
		case <-runtime.gateway.done:
			t.Fatalf("native terminal admission=%v connected=%v step=%d frames=%d process_exited=%v cleanup=%v", err, runtime.gateway.terminal.Load(), runtime.gateway.b.step, runtime.gateway.g.client.Snapshot().Frames, processExited, cleanupErr)
		default:
			t.Fatalf("native terminal admission=%v owner cleanup unverified=%v", err, cleanupErr)
		}
	}
	native := runtime.gateway.b.native
	expectedModel := "fixture"
	if profile {
		expectedModel = "fixture-profile"
	}
	if identity != native.Identity || native.Cwd != home || native.Model != expectedModel || native.ModelProvider != "fixture" || native.Approval.Kind != "never" || native.Sandbox.Type != "readOnly" || !native.ServiceTier.Present || native.ServiceTier.Value != "flex" {
		t.Fatal("native terminal inherited configuration changed")
	}
	if profile {
		if !native.ReasoningEffort.Present || native.ReasoningEffort.Value != "low" {
			t.Fatal("profile reasoning effort changed")
		}
		select {
		case <-providerSeen:
		case <-runtime.Done():
			t.Fatalf("controller stopped before initial terminal prompt reached fixture provider: %v", runtime.gateway.report.Cause)
		case <-ctx.Done():
			for len(methods) > 0 {
				t.Logf("fixture terminal RPC: %.80s", <-methods)
			}
			select {
			case raw := <-captured:
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(raw, &fields)
				for key, value := range fields {
					if key != "threadId" && key != "input" && key != "clientUserMessageId" && !native.SameTurnSetting(key, value) {
						t.Logf("fixture terminal setting outside baseline: %s (null=%v)", key, string(value) == "null")
					}
				}
			default:
				t.Log("fixture terminal did not submit turn/start")
			}
			t.Fatal("initial terminal prompt not delivered")
		}
		t.Log("actual profile TUI delivered initial input through durable admission to the scripted loopback provider; provider handler checks payload parity")
	} else {
		t.Log("actual native TUI reached durable runtime admission with inherited fixture model/provider/cwd/approval/sandbox/tier; no model input")
	}
}

// Isolated native fixture only: retain one initial parameter object in memory,
// forwarding every frame unchanged. Diagnostics print field names, never values.
func captureNativeTurn(t *testing.T, ctx context.Context, root, endpoint string, captured chan<- json.RawMessage, methods chan<- string) string {
	t.Helper()
	path := filepath.Join(root, "capture.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var peers sync.WaitGroup
	var peerMu sync.Mutex
	closing := false
	server := &http.Server{ReadHeaderTimeout: 3 * time.Second}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peerMu.Lock()
		if closing {
			peerMu.Unlock()
			http.Error(w, "closing", 503)
			return
		}
		peers.Add(1)
		peerMu.Unlock()
		defer peers.Done()
		upgrader := websocket.Upgrader{}
		downstream, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer downstream.Close()
		downstream.SetReadLimit(4 << 20)
		dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(endpoint, "unix://"))
		}}
		upstream, _, err := dialer.DialContext(ctx, "ws://localhost/rpc", nil)
		if err != nil {
			return
		}
		defer upstream.Close()
		upstream.SetReadLimit(4 << 20)
		stop := context.AfterFunc(ctx, func() { downstream.Close(); upstream.Close() })
		defer stop()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer downstream.Close()
			defer upstream.Close()
			for {
				kind, raw, err := downstream.ReadMessage()
				if err != nil {
					return
				}
				var request struct {
					Method string
					Params json.RawMessage
				}
				if json.Unmarshal(raw, &request) == nil {
					select {
					case methods <- request.Method:
					default:
					}
					if request.Method == "turn/start" {
						select {
						case captured <- request.Params:
						default:
						}
					}
				}
				if upstream.WriteMessage(kind, raw) != nil {
					return
				}
			}
		}()
		for {
			kind, raw, err := upstream.ReadMessage()
			if err != nil {
				break
			}
			if downstream.WriteMessage(kind, raw) != nil {
				break
			}
		}
		downstream.Close()
		upstream.Close()
		<-done
	})
	served := make(chan struct{})
	go func() { defer close(served); _ = server.Serve(listener) }()
	t.Cleanup(func() {
		peerMu.Lock()
		closing = true
		peerMu.Unlock()
		_ = server.Close()
		_ = listener.Close()
		<-served
		joined := make(chan struct{})
		go func() { peers.Wait(); close(joined) }()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("fixture capture peers did not join")
		}
	})
	return "unix://" + path
}
