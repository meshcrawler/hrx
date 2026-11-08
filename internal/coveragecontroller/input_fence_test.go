package coveragecontroller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/codexrpc"
)

func TestInputFenceExclusiveWaitsAndPreventsInterleaving(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var fence inputFence
	prior, err := fence.acquire(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	exclusive := make(chan func(), 1)
	go func() {
		release, err := fence.acquire(ctx, true)
		if err != nil {
			t.Error(err)
		}
		exclusive <- release
	}()
	// Observe the writer waiting without sleeps or scheduler assumptions.
	for {
		fence.mu.Lock()
		waiting, changed := fence.writers != 0, fence.changed
		fence.mu.Unlock()
		if waiting {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	next := make(chan func(), 1)
	go func() {
		release, err := fence.acquire(ctx, false)
		if err != nil {
			t.Error(err)
		}
		next <- release
	}()
	select {
	case <-exclusive:
		t.Fatal("exclusive overlapped prior input")
	case <-next:
		t.Fatal("new input bypassed waiting recovery")
	default:
	}
	prior()
	var recovery func()
	select {
	case recovery = <-exclusive:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Receiver/event/answer work has no lease; it remains runnable while held.
	events, _ := newEventQueue(1, 1024)
	if err := events.push(frame{raw: []byte(`{"method":"fixture"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := events.next(ctx); err != nil {
		t.Fatal("receiver work blocked by input fence", err)
	}
	select {
	case <-next:
		t.Fatal("new input interleaved exclusive recovery")
	default:
	}
	recovery()
	recovery() // release is idempotent; it cannot release a successor's lease.
	select {
	case release := <-next:
		release()
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestGatewayInputFenceCoversDispatchAndAttention(t *testing.T) {
	h, store := attentionGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := h.exclusiveInput(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, source := range []workSource{terminalSource, boardSource, directMessageSource} {
		bounded, stop := context.WithTimeout(ctx, time.Millisecond)
		_, err := h.handle(bounded, source, []byte(`{"id":1,"method":"thread/read","params":{"threadId":"thread"}}`))
		stop()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("dispatch bypassed exclusive lease", source, err)
		}
	}
	bounded, stop := context.WithTimeout(ctx, time.Millisecond)
	_, err = h.attention(bounded, boardSource, []byte(`1`), nil)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) || len(store.History()) != 0 {
		t.Fatal("attention reached mutex/journal inside exclusive recovery", err)
	}
	select {
	case <-h.router.requests:
		t.Fatal("input reached native router during recovery")
	default:
	}
}

func TestInputFenceCanceledWriterDoesNotStrandReaders(t *testing.T) {
	var fence inputFence
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fence.acquire(ctx, true); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	release, err := fence.acquire(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestInputFenceLeavesServerRequestAnswersLive(t *testing.T) {
	h, _ := attentionGateway(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := h.exclusiveInput(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	book := newApprovalBook(h.g.binding.Generation, 1)
	h.g.approvals = book
	raw := []byte(`{"id":0,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread","turnId":"turn","itemId":"item"}}`)
	message := codexrpc.Message{Raw: raw, Generation: h.g.binding.Generation, Ordinal: 1, Kind: codexrpc.ServerRequest}
	if err := book.capture(message); err != nil {
		t.Fatal(err)
	}
	id, _ := h.g.identity()
	if err := book.reduce(frame{connection: id, ordinal: 1, kind: serverRequestFrame, raw: raw}, "thread"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.answer(ctx, terminalSource, []byte(`{"id":0,"result":{"decision":"accept"}}`)) }()
	select {
	case answer := <-book.replies:
		answer.done <- nil // Existing sole native owner handles this channel.
	case <-ctx.Done():
		t.Fatal("input fence blocked server-request answer", ctx.Err())
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestOwnedPendingVerifierIsHostBoundAndFailClosed(t *testing.T) {
	h, _ := attentionGateway(t)
	h.ready = make(chan struct{})
	request := OwnedPendingRequest{RuntimeBinding: h.attentionBinding(), ThreadID: "thread", DeliveryID: recoveryDelivery}
	calls := 0
	h.verifyPending = func(ctx context.Context, actual OwnedPendingRequest) error {
		calls++
		if actual != request {
			t.Fatal("verifier target changed")
		}
		if _, bounded := ctx.Deadline(); !bounded {
			t.Fatal("unbounded verifier")
		}
		return nil
	}
	if err := h.verifyOwnedPending(context.Background(), request); err == nil || calls != 0 {
		t.Fatal("verification before Ready")
	}
	close(h.ready)
	wrong := request
	wrong.RuntimeBinding = "stale"
	if err := h.verifyOwnedPending(context.Background(), wrong); err == nil || calls != 0 {
		t.Fatal("client changed verifier binding")
	}
	if err := h.verifyOwnedPending(context.Background(), request); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	h.verifyPending = func(context.Context, OwnedPendingRequest) error { return errors.New("denied") }
	if err := h.verifyOwnedPending(context.Background(), request); !errors.Is(err, errAttention) {
		t.Fatal(err)
	}
	h.verifyPending = nil
	if err := h.verifyOwnedPending(context.Background(), request); !errors.Is(err, errAttention) {
		t.Fatal("missing verifier authorized continuation", err)
	}
}
