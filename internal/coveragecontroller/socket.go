package coveragecontroller

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/meshcrawler/hrx/internal/codexrpc"
)

// serveSocket owns an already-created private Unix listener. The launcher must
// create its parent directory with mode 0700 and assign one listener per source;
// request paths, query strings and headers never select a source. There is no
// TCP fallback. Pinned app-server-client remote.rs uses /rpc for Unix sockets;
// the root path remains available to generic WebSocket diagnostic clients.
func (h *gateway) serveSocket(parent context.Context, source workSource, listener *net.UnixListener) error {
	if parent == nil || listener == nil {
		return codexrpc.ErrConfig
	}
	if source != terminalSource && source != boardSource && source != directMessageSource {
		return codexrpc.ErrRequest
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(h.ctx, cancel)
	defer stop()
	bounded := &peerListener{Listener: listener, ctx: ctx, slots: make(chan struct{}, 16)}
	var peers sync.WaitGroup
	var mu sync.Mutex
	closing := false
	upgrader := websocket.Upgrader{HandshakeTimeout: h.stageBudget, ReadBufferSize: 4096, WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	server := &http.Server{ReadHeaderTimeout: h.stageBudget, IdleTimeout: h.stageBudget, MaxHeaderBytes: 8192}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || (r.URL.Path != "/" && r.URL.Path != "/rpc") || r.URL.RawQuery != "" {
			http.Error(w, "invalid endpoint", http.StatusBadRequest)
			return
		}
		// Add happens before hijack; Serve/Close can no longer account for a hijacked
		// connection, so handlers retain ownership until the peer workers join.
		mu.Lock()
		if closing {
			mu.Unlock()
			http.Error(w, "endpoint closing", http.StatusServiceUnavailable)
			return
		}
		peers.Add(1)
		mu.Unlock()
		defer peers.Done()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		opts := codexrpc.DefaultWebSocketOptions()
		opts.FrameBytes = h.router.maxFrame
		wire, err := codexrpc.NewWebSocketWire(conn, opts)
		if err != nil {
			_ = conn.Close()
			return
		}
		_ = h.servePeer(ctx, source, wire)
	})
	stopServer := context.AfterFunc(ctx, func() { _ = server.Close(); _ = listener.Close() })
	defer stopServer()
	err := server.Serve(bounded)
	mu.Lock()
	closing = true
	mu.Unlock()
	cancel()
	_ = server.Close()
	_ = listener.Close()
	joined := make(chan struct{})
	go func() { peers.Wait(); close(joined) }()
	timer := time.NewTimer(h.shutdownBudget)
	defer timer.Stop()
	select {
	case <-joined:
	case <-timer.C:
		return errors.Join(err, codexrpc.ErrShutdownUnverified)
	}
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) || parent.Err() != nil || h.ctx.Err() != nil {
		return nil
	}
	return err
}

type peerListener struct {
	net.Listener
	ctx   context.Context
	slots chan struct{}
}

func (l *peerListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.ctx.Done():
		return nil, net.ErrClosed
	}
	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &peerConn{Conn: conn, release: func() { <-l.slots }}, nil
}

type peerConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *peerConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
