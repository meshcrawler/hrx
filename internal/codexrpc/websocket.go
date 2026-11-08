package codexrpc

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

var (
	ErrLiveness   = errors.New("observer websocket pong deadline expired")
	ErrFrameLimit = errors.New("observer websocket message exceeds limit")
)

// WebSocketOptions bounds transport work, not native subscription or coverage.
// ReadTimeout is renewed by reads and matching pongs, never by arbitrary pongs.
// Independently, each ping MUST receive its matching pong within PongTimeout.
type WebSocketOptions struct {
	FrameBytes                                int
	ReadTimeout, WriteTimeout, ControlTimeout time.Duration
	PingInterval, PongTimeout                 time.Duration
}

func DefaultWebSocketOptions() WebSocketOptions {
	return WebSocketOptions{
		FrameBytes: 4 << 20, ReadTimeout: 45 * time.Second,
		WriteTimeout: 10 * time.Second, ControlTimeout: 5 * time.Second,
		PingInterval: 15 * time.Second, PongTimeout: 10 * time.Second,
	}
}

// Kept private: production accepts only Gorilla's concrete connection, whose
// Close/deadline/concurrency contracts have been inspected. Tests inject memory
// doubles, not a second public transport whose guarantees are unknown.
type websocketConn interface {
	NextReader() (int, io.Reader, error)
	SetReadLimit(int64)
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	SetPingHandler(func(string) error)
	SetPongHandler(func(string) error)
	SetCloseHandler(func(int, string) error)
	WriteMessage(int, []byte) error
	WriteControl(int, []byte, time.Time) error
	Close() error
}

// WebSocketWire is a dormant adapter for an already-owned connection. It has no
// dialer, initialization RPC, thread selection, subscription, resume or journal.
// Its one heartbeat worker sends control frames only. Read is the sole socket
// reader; it must be continuously called by the exclusive foundation owner.
type WebSocketWire struct {
	conn            websocketConn
	opts            WebSocketOptions
	ctx             context.Context
	cancel          context.CancelFunc
	challengePrefix string

	mu                 sync.Mutex
	cause              error
	reading            bool
	writers            int
	opsClosed, closeOK bool
	pending            string
	pongBy             time.Time
	matched            bool
	pings, pongs       uint64

	writeToken                              chan struct{}
	pong                                    chan struct{}
	done, opsDone, heartbeatDone, closeDone chan struct{}
}

var _ Wire = (*WebSocketWire)(nil)

// NewWebSocketWire transfers exclusive connection/handler ownership on success.
// Invalid arguments leave the connection untouched and caller-owned. The caller
// MUST NOT subsequently read, write, change handlers/deadlines, or reuse conn.
// Immediately hand this Wire to New with FrameBytes no larger than this cap.
// Heartbeats start now, so an abandoned/non-drained handoff fails closed too.
func NewWebSocketWire(conn *websocket.Conn, opts WebSocketOptions) (*WebSocketWire, error) {
	if conn == nil {
		return nil, ErrConfig
	}
	return newWebSocketWire(conn, opts)
}

func newWebSocketWire(conn websocketConn, opts WebSocketOptions) (*WebSocketWire, error) {
	if conn == nil || opts.FrameBytes <= 0 || opts.FrameBytes > 64<<20 ||
		opts.ReadTimeout <= 0 || opts.WriteTimeout <= 0 || opts.ControlTimeout <= 0 ||
		opts.PingInterval <= 0 || opts.PongTimeout <= 0 ||
		opts.ControlTimeout > opts.PongTimeout ||
		opts.PingInterval >= opts.ReadTimeout ||
		opts.PongTimeout >= opts.ReadTimeout-opts.PingInterval {
		return nil, ErrConfig
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &WebSocketWire{
		conn: conn, opts: opts, ctx: ctx, cancel: cancel,
		challengePrefix: "hrx-observer/" + rand.Text() + "/",
		writeToken:      make(chan struct{}, 1), pong: make(chan struct{}, 1),
		done: make(chan struct{}), opsDone: make(chan struct{}),
		heartbeatDone: make(chan struct{}), closeDone: make(chan struct{}),
	}
	w.writeToken <- struct{}{}
	conn.SetReadLimit(int64(opts.FrameBytes))
	conn.SetPongHandler(w.onPong)
	conn.SetPingHandler(func(payload string) error {
		return w.control(w.ctx, websocket.PongMessage, []byte(payload), time.Time{})
	})
	conn.SetCloseHandler(func(code int, _ string) error {
		_ = w.control(w.ctx, websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Time{})
		return ErrRead
	})
	go w.heartbeat()
	return w, nil
}

func (w *WebSocketWire) Done() <-chan struct{} { return w.done }

// First terminal cause wins. Publish it before closing the socket; never log or
// return remote payload/error text. Close runs once, outside the state lock.
func (w *WebSocketWire) fail(cause error) {
	w.mu.Lock()
	if w.cause != nil {
		w.mu.Unlock()
		return
	}
	w.cause = cause
	close(w.done)
	w.finishOpsLocked()
	w.mu.Unlock()
	w.cancel()
	go func() {
		err := w.conn.Close()
		w.mu.Lock()
		w.closeOK = err == nil
		w.mu.Unlock()
		close(w.closeDone)
	}()
}

func (w *WebSocketWire) finishOpsLocked() {
	if w.cause != nil && !w.reading && w.writers == 0 && !w.opsClosed {
		w.opsClosed = true
		close(w.opsDone)
	}
}

func (w *WebSocketWire) begin(read bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cause != nil {
		return w.cause
	}
	if read {
		if w.reading {
			return ErrBusy
		}
		w.reading = true
	} else {
		w.writers++
	}
	return nil
}

func (w *WebSocketWire) end(read bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if read {
		w.reading = false
	} else {
		w.writers--
	}
	w.finishOpsLocked()
}

// Join cancellation callbacks before dropping an operation receipt. A callback
// from an old successful operation must not close a later healthy operation.
func (w *WebSocketWire) watch(ctx context.Context, cause error) func() {
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(finished)
		w.fail(cause)
	})
	return func() {
		if !stop() {
			<-finished
		}
	}
}

func (w *WebSocketWire) terminal(fallback error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cause != nil {
		return w.cause
	}
	return fallback
}

func (w *WebSocketWire) Read(ctx context.Context, limit int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, ErrConfig
	}
	if err := w.begin(true); err != nil {
		return nil, err
	}
	defer w.end(true)
	unwatch := w.watch(ctx, ErrCanceled)
	defer unwatch()
	bound := min(limit, w.opts.FrameBytes)
	// Only this reader touches Gorilla read-side methods, including handlers.
	// Header-level limit precedes NextReader and any payload allocation.
	w.conn.SetReadLimit(int64(bound))
	if err := w.conn.SetReadDeadline(time.Now().Add(w.opts.ReadTimeout)); err != nil {
		w.fail(ErrRead)
		return nil, w.terminal(ErrRead)
	}
	kind, r, err := w.conn.NextReader()
	if err != nil {
		cause := ErrRead
		if errors.Is(err, websocket.ErrReadLimit) {
			cause = ErrFrameLimit
		}
		w.fail(cause)
		return nil, w.terminal(cause)
	}
	if kind != websocket.TextMessage {
		w.fail(ErrProtocol)
		return nil, w.terminal(ErrProtocol)
	}
	// Also bound decompressed/fragmented output, independently of Gorilla's
	// wire-length limit. Never ReadMessage/allocate from an advertised length.
	raw, err := io.ReadAll(io.LimitReader(r, int64(bound)+1))
	if len(raw) > bound || errors.Is(err, websocket.ErrReadLimit) {
		w.fail(ErrFrameLimit)
		return nil, w.terminal(ErrFrameLimit)
	}
	if err != nil {
		w.fail(ErrRead)
		return nil, w.terminal(ErrRead)
	}
	if !utf8.Valid(raw) {
		w.fail(ErrProtocol)
		return nil, w.terminal(ErrProtocol)
	}
	if err := ctx.Err(); err != nil {
		w.fail(ErrCanceled)
		return nil, err
	}
	if err := w.terminal(nil); err != nil {
		return nil, err
	}
	return raw, nil
}

func (w *WebSocketWire) Write(ctx context.Context, raw []byte) error {
	if len(raw) > w.opts.FrameBytes || !utf8.Valid(raw) {
		return ErrRequest
	}
	return w.write(ctx, websocket.TextMessage, raw, w.opts.WriteTimeout, time.Time{})
}

func (w *WebSocketWire) control(ctx context.Context, kind int, raw []byte, deadline time.Time) error {
	return w.write(ctx, kind, raw, w.opts.ControlTimeout, deadline)
}

func (w *WebSocketWire) write(ctx context.Context, kind int, raw []byte, timeout time.Duration, by time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.begin(false); err != nil {
		return err
	}
	defer w.end(false)
	deadline := time.Now().Add(timeout)
	if !by.IsZero() && by.Before(deadline) {
		deadline = by
	}
	callCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	unwatch := w.watch(callCtx, ErrWrite)
	defer unwatch()
	select {
	case <-w.writeToken:
	case <-callCtx.Done():
		w.fail(ErrWrite)
		return w.terminal(ErrWrite)
	case <-w.done:
		return w.terminal(ErrWrite)
	}
	defer func() { w.writeToken <- struct{}{} }()
	if err := w.terminal(nil); err != nil {
		return err
	}
	if err := callCtx.Err(); err != nil {
		w.fail(ErrWrite)
		return w.terminal(ErrWrite)
	}
	deadline, _ = callCtx.Deadline()
	var err error
	if kind == websocket.TextMessage {
		err = w.conn.SetWriteDeadline(deadline)
		if err == nil {
			err = w.conn.WriteMessage(kind, raw)
		}
	} else {
		err = w.conn.WriteControl(kind, raw, deadline)
	}
	if err != nil || callCtx.Err() != nil {
		w.fail(ErrWrite)
		return w.terminal(ErrWrite)
	}
	return w.terminal(nil)
}

func (w *WebSocketWire) onPong(payload string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cause != nil {
		return w.cause
	}
	if w.pending == "" || payload != w.pending || !time.Now().Before(w.pongBy) || w.matched {
		return nil // Unsolicited, duplicate, stale or late pongs never extend health.
	}
	// Handler executes on the single Read goroutine, not the heartbeat worker.
	if err := w.conn.SetReadDeadline(time.Now().Add(w.opts.ReadTimeout)); err != nil {
		return ErrRead
	}
	w.matched = true
	w.pongs++
	select {
	case w.pong <- struct{}{}:
	default:
	}
	return nil
}

func (w *WebSocketWire) heartbeat() {
	defer close(w.heartbeatDone)
	for sequence := uint64(1); sequence != 0; sequence++ {
		timer := time.NewTimer(w.opts.PingInterval)
		select {
		case <-w.done:
			timer.Stop()
			return
		case <-timer.C:
		}
		token := w.challengePrefix + strconv.FormatUint(sequence, 16)
		w.mu.Lock()
		if w.cause != nil {
			w.mu.Unlock()
			return
		}
		w.pending, w.matched = token, false
		w.pongBy = time.Now().Add(w.opts.PongTimeout)
		by := w.pongBy
		w.pings++
		w.mu.Unlock()
		if err := w.control(w.ctx, websocket.PingMessage, []byte(token), by); err != nil {
			w.fail(ErrWrite)
			return
		}
		timer = time.NewTimer(time.Until(by))
		select {
		case <-w.done:
			timer.Stop()
			return
		case <-w.pong:
			timer.Stop()
		case <-timer.C:
			// A timely pong may have arrived before the timer but the worker
			// was scheduled later. The handler's timestamp decides, not select.
		}
		w.mu.Lock()
		ok := w.matched
		w.pending = ""
		select {
		case <-w.pong:
		default:
		}
		w.mu.Unlock()
		if !ok {
			w.fail(ErrLiveness)
			return
		}
	}
	w.fail(ErrLiveness) // Never wrap/reuse a control challenge.
}

// WebSocketShutdown reports actual transport cleanup, NOT observer coverage.
// Failed Close, an expired context, or unjoined activity is never certified.
type WebSocketShutdown struct {
	ReaderStopped, WritersStopped, HeartbeatStopped, TransportClosed bool
	Cause                                                            error
	Pings, Pongs                                                     uint64
}

func (w *WebSocketWire) Stop(ctx context.Context) (WebSocketShutdown, error) {
	w.fail(ErrStopped)
	wait := func(done <-chan struct{}) bool {
		select {
		case <-done:
			return true
		default:
		}
		select {
		case <-done:
			return true
		case <-ctx.Done():
			return false
		}
	}
	wait(w.opsDone)
	heartbeat, closed := wait(w.heartbeatDone), wait(w.closeDone)
	w.mu.Lock()
	report := WebSocketShutdown{
		ReaderStopped: !w.reading, WritersStopped: w.writers == 0,
		HeartbeatStopped: heartbeat, TransportClosed: closed && w.closeOK,
		Cause: w.cause, Pings: w.pings, Pongs: w.pongs,
	}
	w.mu.Unlock()
	if ctx.Err() != nil || !report.ReaderStopped || !report.WritersStopped ||
		!heartbeat || !report.TransportClosed {
		return report, ErrShutdownUnverified
	}
	return report, nil
}

// Close implements Wire. Real Gorilla Close unblocks read/write I/O. If a
// broken connection violates that contract, this join may not return: callers
// must use Client.Stop/Stop with a deadline and retain failed cleanup receipts.
func (w *WebSocketWire) Close() error {
	_, err := w.Stop(context.Background())
	return err
}
