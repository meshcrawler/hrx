package codexrpc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
)

// All tests in this file use memory only. No net.Conn, socket, native process,
// DB or model is created. synctest makes timeout/heartbeat scenarios virtual.
type wsPacket struct {
	kind   int
	raw    []byte
	reader io.Reader
	header int64
	err    error
}

type mockWebSocket struct {
	stuckWriteGate                                        chan struct{}
	in                                                    chan wsPacket
	out                                                   chan wsPacket
	entered                                               chan int
	closed                                                chan struct{}
	once                                                  sync.Once
	mu                                                    sync.Mutex
	readLimit                                             int64
	readBy, writeBy                                       time.Time
	ping, pong                                            func(string) error
	closeHandler                                          func(int, string) error
	dataGate, controlGate, readGate, closeGate            chan struct{}
	autoPong                                              bool
	writeErr, closeErr, readDeadlineErr, writeDeadlineErr error
	readers, writers, maxReaders, maxWriters, closes      atomic.Int64
	limitSeen                                             atomic.Int64
}

func mockWS() *mockWebSocket {
	return &mockWebSocket{
		in: make(chan wsPacket, 128), out: make(chan wsPacket, 128),
		entered: make(chan int, 128), closed: make(chan struct{}),
	}
}

func mockWSOptions() WebSocketOptions {
	return WebSocketOptions{
		FrameBytes: 256, ReadTimeout: 20 * time.Second, WriteTimeout: 3 * time.Second,
		ControlTimeout: time.Second, PingInterval: 2 * time.Second, PongTimeout: 2 * time.Second,
	}
}

func (m *mockWebSocket) SetReadLimit(n int64) { m.mu.Lock(); m.readLimit = n; m.mu.Unlock() }
func (m *mockWebSocket) SetReadDeadline(by time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readBy = by
	return m.readDeadlineErr
}
func (m *mockWebSocket) SetWriteDeadline(by time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writeBy = by
	return m.writeDeadlineErr
}
func (m *mockWebSocket) SetPingHandler(f func(string) error)       { m.ping = f }
func (m *mockWebSocket) SetPongHandler(f func(string) error)       { m.pong = f }
func (m *mockWebSocket) SetCloseHandler(f func(int, string) error) { m.closeHandler = f }
func (m *mockWebSocket) NextReader() (int, io.Reader, error) {
	n := m.readers.Add(1)
	defer m.readers.Add(-1)
	max(&m.maxReaders, n)
	if m.readGate != nil {
		<-m.readGate
		return 0, nil, io.EOF
	}
	for {
		m.mu.Lock()
		by, limit := m.readBy, m.readLimit
		m.mu.Unlock()
		m.limitSeen.Store(limit)
		timer := time.NewTimer(time.Until(by))
		var p wsPacket
		select {
		case <-m.closed:
			timer.Stop()
			return 0, nil, io.EOF
		case <-timer.C:
			return 0, nil, context.DeadlineExceeded
		case p = <-m.in:
			timer.Stop()
		}
		if p.err != nil {
			return 0, nil, p.err
		}
		switch p.kind {
		case websocket.PingMessage:
			if err := m.ping(string(p.raw)); err != nil {
				return 0, nil, err
			}
			continue
		case websocket.PongMessage:
			if err := m.pong(string(p.raw)); err != nil {
				return 0, nil, err
			}
			continue
		case websocket.CloseMessage:
			return 0, nil, m.closeHandler(websocket.CloseNormalClosure, string(p.raw))
		}
		header := p.header
		if header == 0 {
			header = int64(len(p.raw))
		}
		if header > limit {
			return 0, nil, websocket.ErrReadLimit
		}
		if p.reader != nil {
			return p.kind, p.reader, nil
		}
		return p.kind, bytes.NewReader(p.raw), nil
	}
}
func (m *mockWebSocket) WriteMessage(kind int, raw []byte) error {
	m.mu.Lock()
	by := m.writeBy
	m.mu.Unlock()
	return m.write(kind, raw, by, m.dataGate)
}
func (m *mockWebSocket) WriteControl(kind int, raw []byte, by time.Time) error {
	return m.write(kind, raw, by, m.controlGate)
}
func (m *mockWebSocket) write(kind int, raw []byte, by time.Time, gate chan struct{}) error {
	n := m.writers.Add(1)
	defer m.writers.Add(-1)
	max(&m.maxWriters, n)
	m.entered <- kind
	if m.stuckWriteGate != nil {
		<-m.stuckWriteGate // Deliberately violates Close/deadline unblocking.
	}
	if by.IsZero() {
		return errors.New("unbounded mock write")
	}
	if gate != nil {
		timer := time.NewTimer(time.Until(by))
		defer timer.Stop()
		select {
		case <-m.closed:
			return io.EOF
		case <-timer.C:
			return context.DeadlineExceeded
		case <-gate:
		}
	}
	if m.writeErr != nil {
		return m.writeErr
	}
	select {
	case <-m.closed:
		return io.EOF
	default:
	}
	m.out <- wsPacket{kind: kind, raw: bytes.Clone(raw)}
	if kind == websocket.PingMessage && m.autoPong {
		m.in <- wsPacket{kind: websocket.PongMessage, raw: bytes.Clone(raw)}
	}
	return nil
}
func (m *mockWebSocket) Close() error {
	m.closes.Add(1)
	if m.closeGate != nil {
		<-m.closeGate
	}
	m.once.Do(func() { close(m.closed) })
	return m.closeErr
}

func newMockWSWire(t *testing.T, m *mockWebSocket, opts WebSocketOptions) *WebSocketWire {
	t.Helper()
	w, err := newWebSocketWire(m, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil && m.closeErr == nil {
			t.Error(err)
		}
	})
	return w
}

func mockRead(w *WebSocketWire) <-chan error {
	done := make(chan error, 1)
	go func() { _, err := w.Read(context.Background(), 256); done <- err }()
	return done
}

func TestWebSocketMockConfigAndOwnership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		if _, err := NewWebSocketWire(nil, DefaultWebSocketOptions()); !errors.Is(err, ErrConfig) {
			t.Fatal(err)
		}
		bad := []WebSocketOptions{{}, mockWSOptions(), mockWSOptions(), mockWSOptions(), mockWSOptions()}
		bad[1].FrameBytes = 65 << 20
		bad[2].PongTimeout = bad[2].ReadTimeout
		bad[3].ControlTimeout = bad[3].PongTimeout + 1
		bad[4].WriteTimeout = 0
		for _, opts := range bad {
			m := mockWS()
			if _, err := newWebSocketWire(m, opts); !errors.Is(err, ErrConfig) {
				t.Fatal(err)
			}
			if m.pong != nil || m.closes.Load() != 0 {
				t.Fatal("invalid constructor stole ownership")
			}
		}
		m := mockWS()
		w := newMockWSWire(t, m, mockWSOptions())
		first := mockRead(w)
		synctest.Wait()
		if _, err := w.Read(context.Background(), 256); !errors.Is(err, ErrBusy) {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := receive(t, first); err == nil {
			t.Fatal("reader did not stop")
		}
		if m.maxReaders.Load() != 1 || m.closes.Load() != 1 {
			t.Fatal("ownership violated")
		}
	})
}

type countingWSReader struct {
	io.Reader
	n int
}

func (r *countingWSReader) Read(p []byte) (int, error) {
	n, e := r.Reader.Read(p)
	r.n += n
	return n, e
}

func TestWebSocketMockFrameBounds(t *testing.T) {
	for _, scenario := range []string{"header", "inflated", "exact", "binary", "utf8", "read-error"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := mockWS()
				w := newMockWSWire(t, m, mockWSOptions())
				r := &countingWSReader{Reader: strings.NewReader(strings.Repeat("x", 4096))}
				p := wsPacket{kind: websocket.TextMessage, raw: []byte("abcd")}
				want := ErrFrameLimit
				switch scenario {
				case "header":
					p.header = 4096
					p.reader = r
				case "inflated":
					p.header = 1
					p.reader = r
				case "exact":
					want = nil
				case "binary":
					p.kind = websocket.BinaryMessage
					want = ErrProtocol
				case "utf8":
					p.raw = []byte{0xff}
					want = ErrProtocol
				case "read-error":
					p.err = errors.New("private remote payload")
					want = ErrRead
				}
				m.in <- p
				raw, err := w.Read(context.Background(), 4)
				if !errors.Is(err, want) {
					t.Fatalf("error %v, want %v", err, want)
				}
				if m.limitSeen.Load() != 4 {
					t.Fatal("limit not applied before header")
				}
				if scenario == "header" && r.n != 0 {
					t.Fatal("allocated/read oversized advertised payload")
				}
				if scenario == "inflated" && r.n != 5 {
					t.Fatalf("decoded bound: read %d", r.n)
				}
				if want != nil && len(raw) != 0 {
					t.Fatal("partial payload escaped")
				}
				if want == nil && string(raw) != "abcd" {
					t.Fatal(string(raw))
				}
				if err != nil && strings.Contains(err.Error(), "private") {
					t.Fatal("remote error leaked")
				}
			})
		})
	}
}

func TestWebSocketMockSilentHalfOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := mockWS()
		w := newMockWSWire(t, m, mockWSOptions())
		read := mockRead(w)
		synctest.Wait()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if err := receive(t, read); !errors.Is(err, ErrLiveness) {
			t.Fatal(err)
		}
		report, err := w.Stop(context.Background())
		if err != nil || report.Pings != 1 || report.Pongs != 0 || !report.HeartbeatStopped {
			t.Fatalf("%+v %v", report, err)
		}
	})
}

func TestWebSocketMockQuietPongsStayHealthy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := mockWS()
		m.autoPong = true
		w := newMockWSWire(t, m, mockWSOptions())
		read := mockRead(w)
		synctest.Wait()
		time.Sleep(65 * time.Second)
		synctest.Wait()
		select {
		case <-w.Done():
			t.Fatal("quiet healthy connection closed")
		default:
		}
		report, err := w.Stop(context.Background())
		if err != nil || report.Pings < 20 || report.Pings != report.Pongs {
			t.Fatalf("%+v %v", report, err)
		}
		receive(t, read)
		for len(m.out) > 0 {
			if p := <-m.out; p.kind != websocket.PingMessage {
				t.Fatal("application/RPC keepalive sent")
			}
		}
	})
}

func TestWebSocketMockWrongAndLatePongsDoNotExtendHealth(t *testing.T) {
	for _, scenario := range []string{"wrong", "late", "app-traffic"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := mockWS()
				w := newMockWSWire(t, m, mockWSOptions())
				done := make(chan error, 1)
				go func() {
					for {
						if _, err := w.Read(context.Background(), 256); err != nil {
							done <- err
							return
						}
					}
				}()
				synctest.Wait()
				time.Sleep(2 * time.Second)
				synctest.Wait()
				ping := receive(t, m.out)
				if scenario == "late" {
					time.Sleep(3 * time.Second)
				} else {
					for i := 0; i < 3; i++ {
						p := wsPacket{kind: websocket.PongMessage, raw: []byte("wrong")}
						if scenario == "app-traffic" {
							p = wsPacket{kind: websocket.TextMessage, raw: []byte(`{"method":"busy"}`)}
						}
						m.in <- p
						time.Sleep(500 * time.Millisecond)
					}
				}
				time.Sleep(time.Second)
				synctest.Wait()
				// A formerly valid challenge received after its deadline cannot
				// revive a failed generation.
				m.in <- wsPacket{kind: websocket.PongMessage, raw: ping.raw}
				time.Sleep(3 * time.Second)
				synctest.Wait()
				if err := receive(t, done); !errors.Is(err, ErrLiveness) {
					t.Fatal(err)
				}
				report, err := w.Stop(context.Background())
				if err != nil || report.Pongs != 0 {
					t.Fatalf("%+v %v", report, err)
				}
			})
		})
	}
}

func TestWebSocketMockCancellationAndWriteFailure(t *testing.T) {
	for _, scenario := range []string{"read-cancel", "write-cancel", "write-timeout", "write-error", "control-timeout", "read-deadline-error", "write-deadline-error"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := mockWS()
				if strings.HasPrefix(scenario, "write-") {
					m.dataGate = make(chan struct{})
				}
				switch scenario {
				case "write-error":
					m.dataGate = nil
					m.writeErr = errors.New("secret")
				case "write-deadline-error":
					m.dataGate = nil
					m.writeDeadlineErr = errors.New("secret")
				case "control-timeout":
					m.controlGate = make(chan struct{})
				case "read-deadline-error":
					m.readDeadlineErr = errors.New("secret")
				}
				w := newMockWSWire(t, m, mockWSOptions())
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				if strings.HasPrefix(scenario, "write-") {
					go func() { done <- w.Write(ctx, []byte("data")) }()
				} else {
					go func() { _, err := w.Read(ctx, 256); done <- err }()
				}
				synctest.Wait()
				if strings.HasSuffix(scenario, "cancel") {
					cancel()
				} else {
					time.Sleep(6 * time.Second)
				}
				synctest.Wait()
				err := receive(t, done)
				if err == nil || strings.Contains(err.Error(), "secret") {
					t.Fatal(err)
				}
				report, stopErr := w.Stop(context.Background())
				if stopErr != nil || !report.ReaderStopped || !report.WritersStopped || !report.TransportClosed {
					t.Fatalf("%+v %v", report, stopErr)
				}
			})
		})
	}
}

func TestWebSocketMockSerializedDataAndControlShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := mockWS()
		m.dataGate = make(chan struct{})
		w := newMockWSWire(t, m, mockWSOptions())
		read := mockRead(w)
		first := make(chan error, 1)
		second := make(chan error, 1)
		go func() { first <- w.Write(context.Background(), []byte("one")) }()
		synctest.Wait()
		go func() { second <- w.Write(context.Background(), []byte("two")) }()
		// Reader attempts a pong while data is pending; heartbeat attempts
		// its own ping at t=2. Both use the SAME bounded writer token.
		m.in <- wsPacket{kind: websocket.PingMessage, raw: []byte("peer")}
		synctest.Wait()
		report, err := w.Stop(context.Background())
		if err != nil || !report.ReaderStopped || !report.WritersStopped || !report.HeartbeatStopped || !report.TransportClosed {
			t.Fatalf("%+v %v", report, err)
		}
		receive(t, read)
		receive(t, first)
		receive(t, second)
		if m.maxWriters.Load() != 1 || m.maxReaders.Load() != 1 || m.closes.Load() != 1 {
			t.Fatal("unserialized I/O or duplicate Close")
		}
	})
}

func TestWebSocketMockPendingHeartbeatShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := mockWS()
		m.controlGate = make(chan struct{})
		w := newMockWSWire(t, m, mockWSOptions())
		read := mockRead(w)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if m.writers.Load() != 1 {
			t.Fatal("heartbeat not pending")
		}
		report, err := w.Stop(context.Background())
		if err != nil || !report.HeartbeatStopped || !report.WritersStopped {
			t.Fatalf("%+v %v", report, err)
		}
		receive(t, read)
	})
}

func TestWebSocketMockSuccessfulDataAndControlSerialization(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := mockWS()
		m.autoPong = true
		w := newMockWSWire(t, m, mockWSOptions())
		read := mockRead(w)
		written := make(chan error, 20)
		for i := 0; i < 20; i++ {
			go func() { written <- w.Write(context.Background(), []byte("data")) }()
		}
		for i := 0; i < 20; i++ {
			if err := receive(t, written); err != nil {
				t.Fatal(err)
			}
		}
		m.in <- wsPacket{kind: websocket.PingMessage, raw: []byte("peer")}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		report, err := w.Stop(context.Background())
		if err != nil || report.Pongs != 2 || m.maxWriters.Load() != 1 {
			t.Fatalf("%+v maxWriters=%d error=%v", report, m.maxWriters.Load(), err)
		}
		data, ping, pong := 0, 0, 0
		for len(m.out) > 0 {
			switch (<-m.out).kind {
			case websocket.TextMessage:
				data++
			case websocket.PingMessage:
				ping++
			case websocket.PongMessage:
				pong++
			}
		}
		if data != 20 || ping != 2 || pong != 1 {
			t.Fatalf("data=%d ping=%d pong=%d", data, ping, pong)
		}
		receive(t, read)
	})
}

func TestWebSocketMockPongReplyAndPrecanceledOperations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := mockWS()
		w := newMockWSWire(t, m, mockWSOptions())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := w.Write(ctx, []byte("never")); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if _, err := w.Read(ctx, 256); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		read := mockRead(w)
		m.in <- wsPacket{kind: websocket.PingMessage, raw: []byte("peer")}
		pong := receive(t, m.out)
		if pong.kind != websocket.PongMessage || string(pong.raw) != "peer" {
			t.Fatal(pong)
		}
		m.in <- wsPacket{kind: websocket.TextMessage, raw: []byte("ok")}
		if err := receive(t, read); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		// A canceled old context must not close a later operation.
		nextCtx, nextCancel := context.WithCancel(context.Background())
		m.in <- wsPacket{kind: websocket.TextMessage, raw: []byte("next")}
		if _, err := w.Read(nextCtx, 256); err != nil {
			t.Fatal(err)
		}
		nextCancel()
		synctest.Wait()
		select {
		case <-w.Done():
			t.Fatal("successful operation left a live cancel callback")
		default:
		}
	})
}

func TestWebSocketMockCleanupReceipts(t *testing.T) {
	for _, scenario := range []string{"close-error", "unjoined-reader", "unjoined-writer", "unjoined-close", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := mockWS()
				switch scenario {
				case "close-error":
					m.closeErr = errors.New("private")
				case "unjoined-reader":
					m.readGate = make(chan struct{})
				case "unjoined-writer":
					m.stuckWriteGate = make(chan struct{})
				case "unjoined-close":
					m.closeGate = make(chan struct{})
				}
				w, err := newWebSocketWire(m, mockWSOptions())
				if err != nil {
					t.Fatal(err)
				}
				read := mockRead(w)
				var write chan error
				if scenario == "unjoined-writer" {
					write = make(chan error, 1)
					go func() { write <- w.Write(context.Background(), []byte("stuck")) }()
				}
				synctest.Wait()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if scenario == "expired" {
					cancel()
				}
				report, err := w.Stop(ctx)
				if !errors.Is(err, ErrShutdownUnverified) {
					t.Fatalf("%+v %v", report, err)
				}
				switch scenario {
				case "close-error":
					if report.TransportClosed {
						t.Fatal("failed close certified")
					}
				case "unjoined-reader":
					if report.ReaderStopped {
						t.Fatal("unjoined reader certified")
					}
					close(m.readGate)
				case "unjoined-writer":
					if report.WritersStopped {
						t.Fatal("unjoined writer certified")
					}
					close(m.stuckWriteGate)
					receive(t, write)
				case "unjoined-close":
					if report.TransportClosed {
						t.Fatal("unjoined close certified")
					}
					close(m.closeGate)
				}
				receive(t, read)
				_, _ = w.Stop(context.Background())
			})
		})
	}
}

func TestWebSocketMockFoundationLossAndShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := mockWS()
		w := newMockWSWire(t, m, mockWSOptions())
		c, err := New(w, "mock-adapter", DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		pending := make(chan error, 1)
		go func() { _, err := c.Call(context.Background(), "test/pending", nil); pending <- err }()
		synctest.Wait()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if err := receive(t, pending); !errors.Is(err, ErrRead) {
			t.Fatal(err)
		}
		report, err := c.Stop(context.Background())
		if err != nil || !report.ReaderStopped || !report.WritersStopped || !report.TransportClosed {
			t.Fatalf("%+v %v", report, err)
		}
		if report.Snapshot.State != TransportLost || report.Snapshot.Cause != ErrRead {
			t.Fatal(report)
		}
	})
}
