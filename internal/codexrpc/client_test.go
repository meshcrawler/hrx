package codexrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type packet struct {
	raw []byte
	err error
}

// This is an in-memory transport only: no sockets, native CLI, DB or provider.
type fakeWire struct {
	in           chan packet
	out          chan []byte
	closed       chan struct{}
	readEntered  chan struct{}
	writeEntered chan struct{}
	readGate     chan struct{} // deliberately broken Read contract for shutdown test
	writeGate    chan struct{}
	writeErr     error
	afterWrite   func()
	closeErr     error
	once         sync.Once
	readers      atomic.Int64
	writers      atomic.Int64
	maxReaders   atomic.Int64
	maxWriters   atomic.Int64
	readLimit    atomic.Int64
	closes       atomic.Int64
}

func newWire() *fakeWire {
	return &fakeWire{
		in: make(chan packet, 32), out: make(chan []byte, 32), closed: make(chan struct{}),
		readEntered: make(chan struct{}, 32), writeEntered: make(chan struct{}, 32),
	}
}

func max(counter *atomic.Int64, value int64) {
	for prior := counter.Load(); value > prior; prior = counter.Load() {
		if counter.CompareAndSwap(prior, value) {
			return
		}
	}
}

func (w *fakeWire) Read(ctx context.Context, limit int) ([]byte, error) {
	n := w.readers.Add(1)
	defer w.readers.Add(-1)
	max(&w.maxReaders, n)
	w.readLimit.Store(int64(limit))
	w.readEntered <- struct{}{}
	if w.readGate != nil {
		<-w.readGate
		return nil, io.EOF
	}
	select {
	case p := <-w.in:
		return p.raw, p.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-w.closed:
		return nil, io.EOF
	}
}

func (w *fakeWire) Write(ctx context.Context, raw []byte) error {
	n := w.writers.Add(1)
	defer w.writers.Add(-1)
	max(&w.maxWriters, n)
	w.writeEntered <- struct{}{}
	if w.writeGate != nil {
		select {
		case <-w.writeGate:
		case <-ctx.Done():
			return ctx.Err()
		case <-w.closed:
			return io.EOF
		}
	}
	if w.writeErr != nil {
		return w.writeErr
	}
	select {
	case w.out <- bytes.Clone(raw):
		if w.afterWrite != nil {
			w.afterWrite()
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-w.closed:
		return io.EOF
	}
}

func (w *fakeWire) Close() error {
	w.closes.Add(1)
	w.once.Do(func() { close(w.closed) })
	return w.closeErr
}

func client(t *testing.T, w *fakeWire, generation string, limits Limits) *Client {
	t.Helper()
	c, err := New(w, generation, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := c.Stop(ctx); err != nil && w.readGate == nil && w.closeErr == nil {
			t.Error(err)
		}
	})
	return c
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("mock synchronization timed out")
		var zero T
		return zero
	}
}

func untilDone(t *testing.T, c *Client) {
	t.Helper()
	receive(t, c.Done())
}

func next(t *testing.T, c *Client) Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m, err := c.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func responseFor(t *testing.T, request []byte, result string) []byte {
	t.Helper()
	var decoded struct{ ID string }
	if err := json.Unmarshal(request, &decoded); err != nil || decoded.ID == "" {
		t.Fatalf("invalid mock request: %v", err)
	}
	raw, err := json.Marshal(map[string]any{"id": decoded.ID, "result": json.RawMessage(result)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRPCDemultiplexesNotificationsWithoutDroppingUnknownFields(t *testing.T) {
	w := newWire()
	c := client(t, w, "connection-a", DefaultLimits())
	result := make(chan outcome, 1)
	go func() {
		r, err := c.Call(context.Background(), "thread/read", map[string]any{"threadId": "fixture"})
		result <- outcome{r, err}
	}()
	request := receive(t, w.out)
	start := []byte(` { "method":"item/started", "params":{"threadId":"fixture","item":{"type":"contextCompaction","id":"item-a"}},"futureField":17 } `)
	end := []byte(`{"method":"item/completed","params":{"threadId":"fixture","item":{"type":"contextCompaction","id":"item-a"}}}`)
	w.in <- packet{raw: start}
	w.in <- packet{raw: responseFor(t, request, `{"thread":{"id":"fixture"}}`)}
	w.in <- packet{raw: end}
	got := receive(t, result)
	if got.err != nil || string(got.response.Result) != `{"thread":{"id":"fixture"}}` ||
		got.response.Generation != "connection-a" || got.response.Ordinal != 2 {
		t.Fatalf("RPC: %+v", got)
	}
	first, last := next(t, c), next(t, c)
	if !bytes.Equal(first.Raw, start) || !bytes.Equal(last.Raw, end) ||
		first.Ordinal != 1 || last.Ordinal != 3 || first.Generation != "connection-a" {
		t.Fatal("lost frame, wire order, or generation")
	}
	if w.maxReaders.Load() != 1 || w.readLimit.Load() != int64(DefaultLimits().FrameBytes) {
		t.Fatal("reader ownership/read-limit contract lost")
	}
	if c.Snapshot().State != TransportOpen {
		t.Fatal("healthy transport changed state")
	}
}

func TestServerRequestAndRPCErrorUseTheSameReaderAndWriter(t *testing.T) {
	w := newWire()
	c := client(t, w, "requests", DefaultLimits())
	result := make(chan outcome, 1)
	go func() {
		r, err := c.Call(context.Background(), "thread/read", nil)
		result <- outcome{r, err}
	}()
	request := receive(t, w.out)
	w.in <- packet{raw: []byte(`{"id":7,"method":"item/commandExecution/requestApproval","params":{"fixture":true}}`)}
	message := next(t, c)
	if message.Kind != ServerRequest {
		t.Fatal("server request misclassified")
	}
	if err := c.Reply(context.Background(), message, json.RawMessage(`{"decision":"decline"}`), nil); err != nil {
		t.Fatal(err)
	}
	var reply map[string]json.RawMessage
	if err := json.Unmarshal(receive(t, w.out), &reply); err != nil || string(reply["id"]) != "7" {
		t.Fatal("reply did not preserve server request ID")
	}
	var control struct{ ID string }
	if err := json.Unmarshal(request, &control); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"id": control.ID, "error": map[string]any{"code": -32600, "message": "private-error-text"}})
	w.in <- packet{raw: raw}
	got := receive(t, result)
	if !errors.Is(got.err, ErrRemote) || !bytes.Contains(got.response.RemoteError, []byte("private-error-text")) ||
		strings.Contains(got.err.Error(), "private-error-text") || c.Snapshot().State != TransportOpen {
		t.Fatal("remote RPC rejection confused with transport loss or leaked text")
	}
}

func TestSeparateQueueConnectionCannotConsumeObserverFrames(t *testing.T) {
	ow, qw := newWire(), newWire()
	observer := client(t, ow, "observer", DefaultLimits())
	queue := client(t, qw, "queue", DefaultLimits())
	result := make(chan error, 1)
	go func() {
		_, err := queue.Call(context.Background(), "thread/queue/add", map[string]any{"fixture": true})
		result <- err
	}()
	request := receive(t, qw.out)
	ow.in <- packet{raw: []byte(`{"method":"turn/started","params":{"threadId":"fixture"}}`)}
	qw.in <- packet{raw: responseFor(t, request, `{"queued":true}`)}
	if err := receive(t, result); err != nil {
		t.Fatal(err)
	}
	if next(t, observer).Method != "turn/started" {
		t.Fatal("observer frame stolen")
	}
	if _, err := queue.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	ow.in <- packet{raw: []byte(`{"method":"turn/completed","params":{"threadId":"fixture"}}`)}
	if next(t, observer).Method != "turn/completed" || observer.Snapshot().State != TransportOpen {
		t.Fatal("queue shutdown changed observer ownership")
	}
	select {
	case <-ow.out:
		t.Fatal("queue coexistence sent an observer RPC")
	default:
	}
}

func TestDisconnectInvalidatesBufferedPrefixAndPendingRPC(t *testing.T) {
	w := newWire()
	c := client(t, w, "before-loss", DefaultLimits())
	result := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "thread/read", nil)
		result <- err
	}()
	receive(t, w.out)
	w.in <- packet{raw: []byte(`{"method":"item/started","params":{"fixture":true}}`)}
	w.in <- packet{err: io.EOF}
	untilDone(t, c)
	if !errors.Is(receive(t, result), ErrRead) {
		t.Fatal("pending RPC not failed on disconnect")
	}
	snapshot := c.Snapshot()
	if snapshot.State != TransportLost || snapshot.Discarded != 1 || snapshot.BufferedMessages != 0 {
		t.Fatalf("loss did not invalidate prefix: %+v", snapshot)
	}
	if _, err := c.Next(context.Background()); !errors.Is(err, ErrRead) {
		t.Fatal("old frame survived terminal loss")
	}
	if report, err := c.Stop(context.Background()); err != nil || report.Snapshot.State != TransportLost {
		t.Fatal("shutdown reset transport loss")
	}
}

func TestReconnectIsANewUncertifiedGenerationAndNeverReplays(t *testing.T) {
	first := client(t, newWire(), "old-generation", DefaultLimits())
	first.wire.(*fakeWire).in <- packet{raw: []byte(`{"id":"approval","method":"fixture/approval"}`)}
	oldRequest := next(t, first)
	first.Stop(context.Background())
	w := newWire()
	second := client(t, w, "new-generation", DefaultLimits())
	oldRequest.Generation = "new-generation" // caller cannot forge the private owner
	if err := second.Reply(context.Background(), oldRequest, json.RawMessage(`{}`), nil); !errors.Is(err, ErrRequest) {
		t.Fatal("old-generation request accepted on successor")
	}
	select {
	case <-w.out:
		t.Fatal("reconnect automatically initialized/resumed/subscribed")
	default:
	}
	w.in <- packet{err: errors.New("fixture reconnect failed")}
	untilDone(t, second)
	if second.Snapshot().State != TransportLost || second.Snapshot().Frames != 0 {
		t.Fatal("failed reconnect fabricated restored coverage/history")
	}
}

func TestBufferLimitsFailClosedRatherThanDroppingNotifications(t *testing.T) {
	for _, by := range []string{"count", "bytes"} {
		t.Run(by, func(t *testing.T) {
			limits := DefaultLimits()
			limits.Messages = 1
			if by == "bytes" {
				limits.BufferedBytes = 1
			}
			w := newWire()
			c := client(t, w, "overflow", limits)
			w.in <- packet{raw: []byte(`{"method":"fixture/a"}`)}
			if by == "count" {
				w.in <- packet{raw: []byte(`{"method":"fixture/b"}`)}
			}
			untilDone(t, c)
			if c.Snapshot().Cause != ErrOverflow || c.Snapshot().BufferedBytes != 0 {
				t.Fatal("overflow did not terminate/invalidate")
			}
			if _, err := c.Next(context.Background()); !errors.Is(err, ErrOverflow) {
				t.Fatal("overflow was silently ignored")
			}
		})
	}
}

func TestMalformedOrUnownedFramesCannotRestoreAStream(t *testing.T) {
	frames := []string{
		`{"method":"a","method":"b"}`, `{"method":"a"} {}`, `[]`,
		`{"id":null,"method":"a"}`, `{"method":"a","result":{}}`,
		`{"id":"not-issued","result":{}}`, `{"id":"x","result":{},"error":{}}`,
		"{\"method\":\"fixture/a\",\"params\":\"\xff\"}",
		strings.Repeat("x", 1025),
	}
	for i, raw := range frames {
		t.Run(strconvForTest(i), func(t *testing.T) {
			w := newWire()
			c := client(t, w, "invalid", Limits{1024, 4, 4096})
			w.in <- packet{raw: []byte(raw)}
			untilDone(t, c)
			if !errors.Is(c.Snapshot().Cause, ErrProtocol) {
				t.Fatal("malformed/unknown response accepted")
			}
		})
	}
}

func strconvForTest(i int) string {
	raw, _ := json.Marshal(i)
	return string(raw)
}

func TestPendingRPCCancellationIsUncertainAndNotRetried(t *testing.T) {
	w := newWire()
	c := client(t, w, "cancel", DefaultLimits())
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := c.Call(ctx, "thread/start", nil)
		result <- err
	}()
	receive(t, w.out) // cancellation after admission could have a native effect
	cancel()
	untilDone(t, c)
	err := receive(t, result)
	if !errors.Is(err, ErrCanceled) && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if c.Snapshot().Cause != ErrCanceled {
		t.Fatal("cancellation claimed a known outcome")
	}
	select {
	case <-w.out:
		t.Fatal("ambiguous RPC automatically retried")
	default:
	}
}

func TestConcurrentRPCIsRejectedAndWritesAreSerialized(t *testing.T) {
	w := newWire()
	w.writeGate = make(chan struct{})
	c := client(t, w, "serialized", DefaultLimits())
	result := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "thread/read", nil)
		result <- err
	}()
	receive(t, w.writeEntered)
	if _, err := c.Call(context.Background(), "thread/read", nil); !errors.Is(err, ErrBusy) {
		t.Fatal("concurrent RPC not refused")
	}
	notified := make(chan error, 1)
	go func() { notified <- c.Notify(context.Background(), "initialized", nil) }()
	close(w.writeGate)
	request := receive(t, w.out)
	w.in <- packet{raw: responseFor(t, request, `{}`)}
	if err := receive(t, result); err != nil {
		t.Fatal(err)
	}
	if err := receive(t, notified); err != nil {
		t.Fatal(err)
	}
	var notification map[string]json.RawMessage
	if err := json.Unmarshal(receive(t, w.out), &notification); err != nil ||
		len(notification["id"]) != 0 || len(notification["params"]) != 0 {
		t.Fatal("initialized notification wire shape changed")
	}
	if w.maxWriters.Load() != 1 {
		t.Fatal("overlapping writes")
	}
}

func TestShutdownJoinsReaderAndInflightWriterExactlyOnce(t *testing.T) {
	w := newWire()
	w.writeGate = make(chan struct{})
	c := client(t, w, "shutdown", DefaultLimits())
	result := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), "thread/read", nil)
		result <- err
	}()
	receive(t, w.writeEntered)
	report, err := c.Stop(context.Background())
	if err != nil || !report.ReaderStopped || !report.WritersStopped || !report.TransportClosed {
		t.Fatalf("shutdown not complete: %+v %v", report, err)
	}
	receive(t, result)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { c.Stop(context.Background()) })
	}
	wg.Wait()
	if w.closes.Load() != 1 {
		t.Fatal("transport closed more than once")
	}
}

func TestShutdownDeadlineOrCloseErrorCannotCertifyCleanup(t *testing.T) {
	t.Run("expired acceptance is not success even if already joined", func(t *testing.T) {
		c := client(t, newWire(), "deadline", DefaultLimits())
		if _, err := c.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := c.Stop(ctx); !errors.Is(err, ErrShutdownUnverified) {
			t.Fatal("expired shutdown acceptance became success")
		}
	})
	t.Run("reader ignores cancellation and close", func(t *testing.T) {
		w := newWire()
		w.readGate = make(chan struct{})
		c := client(t, w, "broken-reader", DefaultLimits())
		receive(t, w.readEntered)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		report, err := c.Stop(ctx)
		if !errors.Is(err, ErrShutdownUnverified) || report.ReaderStopped {
			t.Fatal("unjoined reader counted as stopped")
		}
		close(w.readGate)
		if _, err := c.Stop(context.Background()); err != nil {
			t.Fatal("could not verify later actual termination")
		}
	})
	t.Run("close error", func(t *testing.T) {
		w := newWire()
		w.closeErr = errors.New("private transport detail")
		c := client(t, w, "close-error", DefaultLimits())
		report, err := c.Stop(context.Background())
		if !errors.Is(err, ErrShutdownUnverified) || report.TransportClosed ||
			strings.Contains(err.Error(), "private") {
			t.Fatal("failed close certified or leaked error detail")
		}
	})
}

func TestWriteFailureAndHandlingACKDoNotProveLifecycleCompletion(t *testing.T) {
	w := newWire()
	c := client(t, w, "no-coverage-claim", DefaultLimits())
	w.in <- packet{raw: []byte(`{"method":"fixture/handlingACK","params":{"acknowledged":true}}`)}
	if next(t, c).Method != "fixture/handlingACK" || c.Snapshot().State != TransportOpen {
		t.Fatal("unexpected handling-ACK semantics")
	}
	// A fresh wire with a failing writer is terminal without exposing its error.
	failed := newWire()
	failed.writeErr = errors.New("private credential-like detail")
	broken := client(t, failed, "write-failure", DefaultLimits())
	_, err := broken.Call(context.Background(), "thread/read", nil)
	untilDone(t, broken)
	if !errors.Is(err, ErrWrite) || broken.Snapshot().Cause != ErrWrite || strings.Contains(err.Error(), "private") {
		t.Fatal("write failure not classified safely")
	}
}

func TestInvalidConfigurationDoesNotTakeTransportOwnership(t *testing.T) {
	w := newWire()
	if _, err := New(w, "bad generation", DefaultLimits()); !errors.Is(err, ErrConfig) {
		t.Fatal("invalid generation accepted")
	}
	if w.closes.Load() != 0 || w.readers.Load() != 0 {
		t.Fatal("invalid constructor mutated wire")
	}
}

func TestRequestIDExhaustionCannotReuseAnID(t *testing.T) {
	c := client(t, newWire(), "exhausted", DefaultLimits())
	c.mu.Lock()
	c.serial = ^uint64(0)
	c.mu.Unlock()
	if _, err := c.Call(context.Background(), "thread/read", nil); !errors.Is(err, ErrProtocol) {
		t.Fatal("exhausted ID sequence accepted")
	}
	if c.Snapshot().State != TransportLost {
		t.Fatal("exhausted sequence did not invalidate transport")
	}
}

func TestCancellationAtWriteReturnRemainsUncertain(t *testing.T) {
	w := newWire()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.afterWrite = cancel
	c := client(t, w, "cancel-at-write-return", DefaultLimits())
	_, err := c.Call(ctx, "thread/start", nil)
	if !errors.Is(err, ErrCanceled) && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	receive(t, w.out)
	untilDone(t, c)
	if c.Snapshot().Cause != ErrCanceled {
		t.Fatal("lost cancellation classification", c.Snapshot().Cause)
	}
	select {
	case <-w.out:
		t.Fatal("ambiguous write retried")
	default:
	}
}
