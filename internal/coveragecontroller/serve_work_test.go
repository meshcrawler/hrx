package coveragecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/codexrpc"
	"github.com/meshcrawler/hrx/internal/coveragejournal"
)

func workReply(t *testing.T, r *workRequest) workResult {
	t.Helper()
	select {
	case result := <-r.reply:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("work reply missing")
		return workResult{}
	}
}
func workWireRequest(t *testing.T, w *controllerWire) json.RawMessage {
	t.Helper()
	select {
	case raw := <-w.out:
		var request struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Fatal(err)
		}
		return request.ID
	case <-time.After(5 * time.Second):
		t.Fatal("work not written")
		return nil
	}
}
func TestServeWorkConsumesResponsesAndRenewsForRepeatedCalls(t *testing.T) {
	c, g, w, s := actualCoordinator(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := make(chan *workRequest)
	reports := make(chan serveReport, 1)
	go func() { reports <- c.serveWork(ctx, g, time.Second, time.Second, requests) }()
	sendPair(w)
	<-s.entered
	s.unblock()
	waitCurrent(t, g)
	original, _ := g.client.Current()
	for i := 0; i < 2; i++ {
		req, err := newWorkRequest(ctx, "thread/read", json.RawMessage(`{"threadId":"thread"}`), 2048)
		if err != nil {
			t.Fatal(err)
		}
		requests <- req
		id := workWireRequest(t, w)
		// Notification is delivered separately from the response. Both must be
		// reduced before release and before a second RPC can use the renewed fence.
		w.in <- []byte(`{"method":"thread/tokenUsage/updated","params":{}}`)
		payload := `{"id":` + string(id) + `,"result":{"ok":true}}`
		if i == 1 {
			payload = `{"id":` + string(id) + `,"error":{"code":-1,"message":"expected remote failure"}}`
		}
		w.in <- []byte(payload)
		result := workReply(t, req)
		if result.response.Ordinal == 0 || (i == 0 && result.err != nil) || (i == 1 && result.err == nil) {
			t.Fatal(result)
		}
		current, ok := g.client.Current()
		if !ok || current != original {
			t.Fatal("work rewrote historical publication", current)
		}
	}
	cancel()
	report := serveResult(t, reports)
	if !report.WorkerJoined || report.CleanupError != nil {
		t.Fatal(report)
	}
}

func TestServeWorkCancellationRevokesAndJoins(t *testing.T) {
	c, g, w, s := actualCoordinator(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := make(chan *workRequest)
	reports := make(chan serveReport, 1)
	go func() { reports <- c.serveWork(ctx, g, time.Second, time.Second, requests) }()
	sendPair(w)
	<-s.entered
	s.unblock()
	waitCurrent(t, g)
	caller, stopCaller := context.WithCancel(ctx)
	req, _ := newWorkRequest(caller, "thread/read", json.RawMessage(`{"threadId":"thread"}`), 2048)
	requests <- req
	workWireRequest(t, w)
	stopCaller()
	result := workReply(t, req)
	if result.err == nil {
		t.Fatal("canceled work succeeded")
	}
	report := serveResult(t, reports)
	if !report.WorkerJoined || report.CleanupError != nil {
		t.Fatal(report)
	}
	if _, ok := g.client.Current(); ok {
		t.Fatal("canceled in-flight work retained authority")
	}
}

func TestWorkRequestOwnsParametersAndRejectsInvalidInput(t *testing.T) {
	raw := json.RawMessage(`{"a":1}`)
	req, err := newWorkRequest(context.Background(), "test", raw, 100)
	if err != nil {
		t.Fatal(err)
	}
	raw[5] = '9'
	if string(req.params) != `{"a":1}` {
		t.Fatal("parameters alias caller memory")
	}
	if _, err := newWorkRequest(context.Background(), "test", raw, 2); !errors.Is(err, codexrpc.ErrRequest) {
		t.Fatal(err)
	}
}

// Each append needs a separate permit so the second compaction can stall after
// an initial durable baseline has authorized work.
type permittedStore struct {
	entered chan struct{}
	permit  chan struct{}
}

func (s *permittedStore) Append(ctx context.Context, _ coveragejournal.Stage) (coveragejournal.Durability, error) {
	s.entered <- struct{}{}
	select {
	case <-s.permit:
		return coveragejournal.Durable, nil
	case <-ctx.Done():
		return coveragejournal.Indeterminate, ctx.Err()
	}
}
func TestServeWorkHoldsReplyUntilCompactionIsDurable(t *testing.T) {
	g, w := actualGate(t)
	id, _ := g.identity()
	stream, err := newStream(id, "thread", 8, 8192, 2048, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	store := &permittedStore{make(chan struct{}, 2), make(chan struct{}, 2)}
	c, err := newCoordinator(g, stream, store)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := make(chan *workRequest)
	reports := make(chan serveReport, 1)
	go func() { reports <- c.serveWork(ctx, g, time.Second, time.Second, requests) }()
	sendPair(w)
	<-store.entered
	store.permit <- struct{}{}
	waitCurrent(t, g)
	req, _ := newWorkRequest(ctx, "thread/read", json.RawMessage(`{"threadId":"thread"}`), 2048)
	requests <- req
	requestID := workWireRequest(t, w)
	w.in <- nativeItem(3, "item/started", "second").raw
	w.in <- []byte(`{"id":` + string(requestID) + `,"result":{}}`)
	// The completion arrives after the RPC response and must still be drained.
	w.in <- nativeItem(5, "item/completed", "second").raw
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("receiver stopped draining during RPC")
	}
	select {
	case result := <-req.reply:
		t.Fatal("released before durability", result)
	case <-time.After(20 * time.Millisecond):
	}
	store.permit <- struct{}{}
	if result := workReply(t, req); result.err != nil || result.response.Ordinal != 4 {
		t.Fatal(result)
	}
	cancel()
	report := serveResult(t, reports)
	if report.CleanupError != nil || !report.WorkerJoined {
		t.Fatal(report)
	}
}

func TestServeWorkScopesTerminalAndQueueInputToOwnedThread(t *testing.T) {
	c, g, w, s := actualCoordinator(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := make(chan *workRequest)
	reports := make(chan serveReport, 1)
	go func() { reports <- c.serveWork(ctx, g, time.Second, time.Second, requests) }()
	sendPair(w)
	<-s.entered
	s.unblock()
	waitCurrent(t, g)
	for _, tc := range []struct{ method, params string }{
		{"turn/start", `{"threadId":"other"}`},
		{"thread/queue/add", `{"threadId":"other"}`},
		{"thread/queue/add", `{"threadId":"other","threadId":"thread"}`},
		{"thread/queue/add", `{}`},
		{"thread/settings/update", `{"threadId":"thread"}`},
		{"turn/start", `{"threadId":"thread","approvalPolicy":"never"}`},
		{"turn/start", `{"threadId":"thread","model":"other"}`},
		{"thread/queue/add", `{"threadId":"thread","config":{}}`},
	} {
		req, err := newWorkRequest(ctx, tc.method, json.RawMessage(tc.params), 2048)
		if err != nil {
			t.Fatal(err)
		}
		requests <- req
		if result := workReply(t, req); !errors.Is(result.err, codexrpc.ErrRequest) {
			t.Fatal(tc, result)
		}
		select {
		case raw := <-w.out:
			t.Fatal("rejected input reached wire", string(raw))
		default:
		}
	}
	for _, method := range []string{"turn/start", "thread/queue/add"} {
		req, _ := newWorkRequest(ctx, method, json.RawMessage(`{"threadId":"thread","clientUserMessageId":"message","input":[]}`), 2048)
		requests <- req
		id := workWireRequest(t, w)
		w.in <- []byte(`{"id":` + string(id) + `,"result":{}}`)
		if result := workReply(t, req); result.err != nil {
			t.Fatal(result)
		}
	}
	cancel()
	report := serveResult(t, reports)
	if report.CleanupError != nil {
		t.Fatal(report)
	}
}
