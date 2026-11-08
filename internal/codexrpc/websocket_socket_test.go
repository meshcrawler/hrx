package codexrpc

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// These tests MUST NOT be run on developer machines. The existing GitLab Go
// race/vet job runs them, including real Gorilla framing and TCP I/O.
func socketCI(t *testing.T) {
	t.Helper()
	if os.Getenv("GITLAB_CI") != "true" {
		t.Skip("real socket acceptance runs only on GitLab")
	}
}

func socketOptions() WebSocketOptions {
	return WebSocketOptions{
		FrameBytes: 256, ReadTimeout: 800 * time.Millisecond,
		WriteTimeout: 400 * time.Millisecond, ControlTimeout: 200 * time.Millisecond,
		PingInterval: 100 * time.Millisecond, PongTimeout: 400 * time.Millisecond,
	}
}

func socketPair(t *testing.T, serve func(*websocket.Conn), compress bool) *websocket.Conn {
	t.Helper()
	socketCI(t)
	handlerDone := make(chan struct{})
	upgrader := websocket.Upgrader{EnableCompression: compress}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		conn, err := upgrader.Upgrade(rw, r, nil)
		if err != nil {
			t.Error("fixture upgrade failed")
			return
		}
		defer conn.Close()
		serve(conn)
	}))
	t.Cleanup(server.Close)
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second, EnableCompression: compress}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal("fixture dial failed")
	}
	t.Cleanup(func() {
		_ = conn.Close()
		select {
		case <-handlerDone:
		case <-time.After(3 * time.Second):
			t.Error("fixture handler failed to join")
		}
	})
	return conn
}

func drainSocket(conn *websocket.Conn) {
	for {
		_, r, err := conn.NextReader()
		if err != nil {
			return
		}
		if _, err := io.Copy(io.Discard, r); err != nil {
			return
		}
	}
}

func socketWire(t *testing.T, conn *websocket.Conn) *WebSocketWire {
	t.Helper()
	w, err := NewWebSocketWire(conn, socketOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if report, err := w.Stop(ctx); err != nil {
			t.Errorf("socket cleanup: %+v %v", report, err)
		}
	})
	return w
}

func TestWebSocketSocketQuietPongsAndFoundation(t *testing.T) {
	conn := socketPair(t, func(server *websocket.Conn) {
		_ = server.WriteMessage(websocket.TextMessage, []byte(`{"method":"first"}`))
		_ = server.WriteMessage(websocket.TextMessage, []byte(`{"method":"second"}`))
		drainSocket(server) // Gorilla default ping handler echoes exact payload.
	}, false)
	w := socketWire(t, conn)
	c, err := New(w, "socket-fixture", DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := c.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	if first, second := next(t, c), next(t, c); first.Method != "first" ||
		second.Method != "second" || second.Ordinal != first.Ordinal+1 {
		t.Fatal("wire order changed")
	}
	// More than two read deadlines with NO application messages. Only matched
	// WebSocket pongs can keep this connection open.
	time.Sleep(1800 * time.Millisecond)
	if c.Snapshot().State != TransportOpen {
		t.Fatal("quiet control-healthy connection failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	report, err := w.Stop(ctx)
	if err != nil || report.Pongs < 5 {
		t.Fatalf("%+v %v", report, err)
	}
}

func TestWebSocketSocketSilentHalfOpenDespiteData(t *testing.T) {
	conn := socketPair(t, func(server *websocket.Conn) {
		server.SetPingHandler(func(string) error { return nil }) // Deliberate fault.
		stop := make(chan struct{})
		go func() {
			defer close(stop)
			drainSocket(server)
		}()
		defer func() { _ = server.Close(); <-stop }()
		timer := time.NewTicker(20 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case <-stop:
				return
			case <-timer.C:
				_ = server.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
				if server.WriteMessage(websocket.TextMessage, []byte(`{"method":"busy"}`)) != nil {
					return
				}
			}
		}
	}, false)
	w := socketWire(t, conn)
	failed := make(chan error, 1)
	go func() {
		for {
			if _, err := w.Read(context.Background(), 256); err != nil {
				failed <- err
				return
			}
		}
	}()
	if err := receive(t, failed); !errors.Is(err, ErrLiveness) {
		t.Fatal(err)
	}
}

func TestWebSocketSocketHeaderAndInflationLimits(t *testing.T) {
	for _, scenario := range []string{"header", "compressed"} {
		t.Run(scenario, func(t *testing.T) {
			conn := socketPair(t, func(server *websocket.Conn) {
				if scenario == "header" {
					// Malicious peer advertises 4 GiB and sends NO payload.
					// The client must reject at the header, without allocating.
					header := make([]byte, 10)
					header[0], header[1] = 0x81, 127
					binary.BigEndian.PutUint64(header[2:], 1<<32)
					_, _ = server.NetConn().Write(header)
				} else {
					_ = server.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", 8192)))
				}
				drainSocket(server)
			}, scenario == "compressed")
			w := socketWire(t, conn)
			if raw, err := w.Read(context.Background(), 256); !errors.Is(err, ErrFrameLimit) || len(raw) != 0 {
				t.Fatalf("bound not enforced: bytes=%d error=%v", len(raw), err)
			}
		})
	}
}

func TestWebSocketSocketCancellationUnblocksPendingRead(t *testing.T) {
	conn := socketPair(t, drainSocket, false)
	w := socketWire(t, conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := w.Read(ctx, 256); done <- err }()
	deadline := time.Now().Add(time.Second)
	for {
		w.mu.Lock()
		reading := w.reading
		w.mu.Unlock()
		if reading {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("read never started")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := receive(t, done); err == nil {
		t.Fatal("canceled read succeeded")
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	report, err := w.Stop(stopCtx)
	if err != nil || !report.ReaderStopped || !report.HeartbeatStopped || !report.TransportClosed {
		t.Fatalf("%+v %v", report, err)
	}
}

func TestWebSocketSocketBlockedWriteFailureJoinsAllIO(t *testing.T) {
	socketCI(t)
	release := make(chan struct{})
	ready := make(chan struct{})
	conn := socketPair(t, func(server *websocket.Conn) {
		tcp, ok := server.NetConn().(*net.TCPConn)
		if !ok {
			t.Error("fixture is not TCP")
		} else if err := tcp.SetReadBuffer(1024); err != nil {
			t.Error("fixture read-buffer setup failed")
		}
		close(ready)
		<-release // Intentionally never read any data/control frames.
	}, false)
	t.Cleanup(func() { close(release) })
	receive(t, ready)
	tcp, ok := conn.NetConn().(*net.TCPConn)
	if !ok {
		t.Fatal("fixture client is not TCP")
	}
	if err := tcp.SetWriteBuffer(1024); err != nil {
		t.Fatal("fixture write-buffer setup failed")
	}
	opts := socketOptions()
	opts.FrameBytes, opts.WriteTimeout = 4<<20, 150*time.Millisecond
	w, err := NewWebSocketWire(conn, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if report, err := w.Stop(ctx); err != nil {
			t.Errorf("blocked socket cleanup: %+v %v", report, err)
		}
	})
	read := make(chan error, 1)
	go func() { _, err := w.Read(context.Background(), opts.FrameBytes); read <- err }()
	// Tiny kernel buffers + a non-reading peer force a real partial/blocked
	// data write. Its 150ms bound beats the pending heartbeat control deadline.
	if err := w.Write(context.Background(), []byte(strings.Repeat("x", opts.FrameBytes))); !errors.Is(err, ErrWrite) {
		t.Fatalf("blocked write did not fail closed: %v", err)
	}
	receive(t, read)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	report, err := w.Stop(ctx)
	if err != nil || report.Cause != ErrWrite || !report.ReaderStopped ||
		!report.WritersStopped || !report.HeartbeatStopped || !report.TransportClosed {
		t.Fatalf("blocked write cleanup: %+v %v", report, err)
	}
}
