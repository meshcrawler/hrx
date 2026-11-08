package coveragecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/meshcrawler/hrx/internal/codexlaunch"
	"github.com/meshcrawler/hrx/internal/codexrpc"
	"github.com/meshcrawler/hrx/internal/coveragejournal"
)

func TestGatewayUnixSocketSourceIsolation(t *testing.T) {
	if os.Getenv("HRX_COVERAGE_NATIVE_TESTS") != "1" {
		t.Skip("socket lifecycle runs on GitLab")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g, native := actualGate(t)
	store := fakeStore()
	defer store.unblock()
	h, err := newGateway(ctx, g, store, "fixture", time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close(ctx)
	path := filepath.Join(t.TempDir(), "board.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverCtx, stop := context.WithCancel(ctx)
	defer stop()
	serverDone := make(chan error, 1)
	go func() { serverDone <- h.serveSocket(serverCtx, boardSource, listener) }()
	dialer := websocket.Dialer{HandshakeTimeout: time.Second, NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	conn, _, err := dialer.DialContext(ctx, "ws://localhost/rpc", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"id":11,"method":"initialize","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var reply map[string]json.RawMessage
	if json.Unmarshal(raw, &reply) != nil || string(reply["id"]) != "11" || reply["error"] == nil {
		t.Fatal(string(raw))
	}
	select {
	case raw := <-native.out:
		t.Fatal("board socket reached native before baseline", string(raw))
	default:
	}
	if h.state.Load() != 0 {
		t.Fatal("board refusal stopped owner")
	}
	stop()
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("socket handlers did not join")
	}
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("hijacked client remained open after shutdown")
	}
	if h.state.Load() != 0 {
		t.Fatal("auxiliary listener shutdown killed owner")
	}
}

// Uses the real pinned native server and real journal supplied by nativeBaseline.
// No model input is submitted. The client sees start only after durable staging.
func nativeGatewaySocket(t *testing.T, ctx context.Context, g *codexGate, home string, start json.RawMessage) {
	t.Helper()
	journal, err := coveragejournal.Open(filepath.Join(home, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	store := &afterSyncStore{journal: journal, synced: make(chan struct{}), release: make(chan struct{})}
	defer store.unblock()
	h, err := newGateway(ctx, g, store, "fixture", 5*time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close(ctx)
	failGateway := func(err error) {
		h.cancel()
		select {
		case <-h.done:
			t.Fatalf("native gateway transport=%v owner=%v cleanup=%v", err, h.report.Cause, h.report.CleanupError)
		case <-ctx.Done():
			t.Fatalf("native gateway transport=%v; owner shutdown not observed", err)
		}
	}
	path := filepath.Join(home, "terminal.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	socketCtx, stop := context.WithCancel(ctx)
	defer stop()
	serverDone := make(chan error, 1)
	go func() { serverDone <- h.serveSocket(socketCtx, terminalSource, listener) }()
	dialer := websocket.Dialer{HandshakeTimeout: time.Second, NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	conn, _, err := dialer.DialContext(ctx, "ws://localhost/rpc", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	readReply := func(id string) ([]byte, error) {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return nil, err
			}
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal(raw, &envelope); err != nil {
				return nil, err
			}
			if string(envelope["id"]) == id {
				return raw, nil
			}
		}
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"hrx-gateway-native","version":"1"},"capabilities":{"experimentalApi":true}}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := readReply("1"); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"method":"initialized"}`)); err != nil {
		t.Fatal(err)
	}
	method := "thread/start"
	var startFields map[string]json.RawMessage
	if json.Unmarshal(start, &startFields) != nil {
		t.Fatal("invalid startup fixture")
	}
	if startFields["threadId"] != nil {
		method = "thread/resume"
	}
	raw, _ := json.Marshal(map[string]any{"id": 2, "method": method, "params": start})
	if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
		t.Fatal(err)
	}
	replies := make(chan []byte, 1)
	failures := make(chan error, 1)
	go func() {
		reply, err := readReply("2")
		if err != nil {
			failures <- err
		} else {
			replies <- reply
		}
	}()
	select {
	case <-store.synced:
	case err := <-failures:
		failGateway(err)
	case <-replies:
		failGateway(errors.New("startup response arrived before durable stage"))
	case <-ctx.Done():
		t.Fatal("native gateway failed to stage")
	}
	if _, ok := g.client.Current(); ok {
		t.Fatal("native gateway published before storage completion")
	}
	select {
	case raw := <-replies:
		t.Fatal("start escaped while storage held", string(raw))
	default:
	}
	store.unblock()
	select {
	case raw := <-replies:
		var response map[string]json.RawMessage
		if json.Unmarshal(raw, &response) != nil || response["error"] != nil || response["result"] == nil {
			t.Fatal("native gateway start failed", string(raw))
		}
	case err := <-failures:
		failGateway(err)
	case <-ctx.Done():
		t.Fatal("native gateway did not release durable start")
	}
	if len(journal.History()) != 1 {
		t.Fatal("native gateway baseline missing")
	}
	_ = conn.Close()
	select {
	case <-h.done:
	case <-ctx.Done():
		t.Fatal("terminal loss did not revoke native gateway")
	}
	if _, ok := g.client.Current(); ok {
		t.Fatal("native gateway authority survived terminal loss")
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("native socket handlers did not join")
	}
	t.Log("real native Unix gateway withheld start until journal durability and revoked on terminal disconnect; no model input")
}

func seedNativeThread(t *testing.T, ctx context.Context, dialer *websocket.Dialer, start json.RawMessage) string {
	t.Helper()
	conn, _, err := dialer.DialContext(ctx, "ws://localhost/rpc", nil)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := codexrpc.NewWebSocketWire(conn, codexrpc.DefaultWebSocketOptions())
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	client, err := codexrpc.New(wire, "native-resume-seed", codexrpc.DefaultLimits())
	if err != nil {
		wire.Close()
		t.Fatal(err)
	}
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := client.Stop(stop); err != nil {
			t.Error(err)
		}
	}()
	if _, err := client.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "hrx-resume-seed", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}}); err != nil {
		t.Fatal(err)
	}
	if err := client.Notify(ctx, "initialized", nil); err != nil {
		t.Fatal(err)
	}
	response, err := client.Call(ctx, "thread/start", start)
	if err != nil {
		t.Fatal(err)
	}
	native, err := codexlaunch.ParseThreadStart(response.Result)
	if err != nil {
		t.Fatal(err)
	}
	// Match the gateway's real creation path: resume a named/read native thread,
	// rather than an unobserved empty thread immediately after creation.
	if _, err := client.Call(ctx, "thread/name/set", map[string]string{"threadId": native.ThreadID, "name": "fixture"}); err != nil {
		t.Fatal(err)
	}
	baseline, err := client.Call(ctx, "thread/read", map[string]any{"threadId": native.ThreadID, "includeTurns": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := native.ValidateFreshBaseline(baseline.Result, "fixture"); err != nil {
		t.Fatal(err)
	}
	return native.ThreadID
}

func nativeLauncherRuntime(t *testing.T, ctx context.Context, upstream *websocket.Conn, home string, start json.RawMessage) {
	t.Helper()
	socketRoot, err := os.MkdirTemp("/tmp", "hrx-cov-")
	if err != nil {
		upstream.Close()
		t.Fatal(err)
	}
	defer os.RemoveAll(socketRoot)
	runtime, err := NewRuntime(ctx, upstream, RuntimeOptions{SocketRoot: socketRoot, JournalRoot: filepath.Join(home, "journal"), Name: "fixture", StageTimeout: 5 * time.Second, ShutdownTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close(ctx)
	dial := func(endpoint string) *codexrpc.Client {
		path := strings.TrimPrefix(endpoint, "unix://")
		dialer := websocket.Dialer{HandshakeTimeout: time.Second, NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}}
		conn, _, err := dialer.DialContext(ctx, "ws://localhost/rpc", nil)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := codexrpc.NewWebSocketWire(conn, codexrpc.DefaultWebSocketOptions())
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		client, err := codexrpc.New(wire, "runtime-downstream", codexrpc.DefaultLimits())
		if err != nil {
			wire.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			stop, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			client.Stop(stop)
		})
		return client
	}
	if runtime.Terminal == runtime.Board || runtime.Board == runtime.DirectMessage {
		t.Fatal("ingress sockets overlap")
	}
	board := dial(runtime.Board)
	if _, err := board.Call(ctx, "initialize", map[string]any{}); !errors.Is(err, codexrpc.ErrRemote) {
		t.Fatal("early board client was not refused", err)
	}
	terminal := dial(runtime.Terminal)
	if _, err := terminal.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "runtime-fixture", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}}); err != nil {
		t.Fatal(err)
	}
	if err := terminal.Notify(ctx, "initialized", nil); err != nil {
		t.Fatal(err)
	}
	response, err := terminal.Call(ctx, "thread/start", start)
	if err != nil {
		t.Fatal(err)
	}
	native, err := codexlaunch.ParseThreadStart(response.Result)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := runtime.Ready(ctx)
	if err != nil || identity != native.Identity {
		t.Fatal("runtime returned wrong admitted identity", err)
	}
	if _, err := board.Call(ctx, "initialize", map[string]any{}); err != nil {
		t.Fatal("admitted queue handshake failed", err)
	}
	if _, err := board.Call(ctx, "thread/read", map[string]any{"threadId": identity.ThreadID, "includeTurns": true}); err != nil {
		t.Fatal("queue read bypassed or lost shared owner", err)
	}
	if _, err := terminal.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runtime.Done():
	case <-ctx.Done():
		t.Fatal("runtime cleanup did not finish")
	}
	if err := runtime.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(strings.TrimPrefix(runtime.Terminal, "unix://")); !os.IsNotExist(err) {
		t.Fatal("owned socket path survived cleanup", err)
	}
	journal, err := coveragejournal.Open(filepath.Join(home, "journal"))
	if err != nil {
		t.Fatal("journal remained locked after cleanup", err)
	}
	defer journal.Close()
	if len(journal.History()) != 1 {
		t.Fatal("durable evidence was deleted during cleanup")
	}
	t.Log("launcher runtime kept three ingress sockets isolated, bound native identity, joined cleanup and retained journal evidence")
}
