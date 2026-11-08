package coveragecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/coveragejournal"
)

func continuationRouteFixture(t *testing.T) (*gateway, *attentionMemory, attentionRequest) {
	h, store := attentionGateway(t)
	h.combinedResumeDrain = true
	h.ready = make(chan struct{})
	close(h.ready)
	request, auth := continuationRequestFor(h)
	original := attentionOutcome{Protocol: attentionProtocol, Runtime: strings.Repeat("b", 64), AdmissionBinding: strings.Repeat("b", 64), Thread: request.Thread, Delivery: request.Delivery, Digest: auth.Digest, Status: "admitted", Attempt: 1, Ordinal: 7}
	payload, _ := json.Marshal(original)
	_, _ = store.Append(context.Background(), coveragejournal.Stage{Operation: "source.admitted", Payload: payload})
	h.verifyPending = func(context.Context, OwnedPendingRequest) error {
		h.input.mu.Lock()
		exclusive := h.input.owned && h.input.readers == 0
		h.input.mu.Unlock()
		if !exclusive {
			t.Error("host verifier ran without exclusive ingress fence")
		}
		if !h.attentionMu.TryLock() {
			t.Error("verifier ran after attention mutex acquisition")
		} else {
			h.attentionMu.Unlock()
		}
		return nil
	}
	return h, store, attentionRequest{Protocol: attentionProtocol, Runtime: request.Runtime, Thread: request.Thread, Delivery: request.Delivery, Text: request.Text, Attempt: 1}
}
func callContinuationRoute(t *testing.T, h *gateway, request attentionRequest) (attentionContinuationOutcome, error) {
	t.Helper()
	params, _ := json.Marshal(request)
	raw, _ := json.Marshal(map[string]any{"id": 1, "method": "pitput/attention/continue", "params": json.RawMessage(params)})
	reply, err := h.handle(context.Background(), boardSource, raw)
	if err != nil {
		return attentionContinuationOutcome{}, err
	}
	var result struct{ Result attentionContinuationOutcome }
	if err = json.Unmarshal(reply, &result); err != nil {
		t.Fatal(err)
	}
	return result.Result, nil
}
func TestContinuationRouteHostVerifiedReplayStartsOneEmptyTurn(t *testing.T) {
	h, store, request := continuationRouteFixture(t)
	primitive, _ := continuationRequestFor(h)
	go func() {
		recoveryReply(t, h, "thread/read", continuationHistoryJSON(primitive), 6)
		serveContinuationChecks(t, h, primitive)
		serveOneEmptyContinuation(t, h)
	}()
	result, err := callContinuationRoute(t, h, request)
	if err != nil || result.Status != "started" || result.NewTurnID != "new-turn" || len(store.History()) != 4 {
		t.Fatal(result, err)
	}
	replay, err := callContinuationRoute(t, h, request)
	if err != nil || replay != result {
		t.Fatal(replay, err)
	}
	select {
	case req := <-h.router.requests:
		t.Fatal("known continuation resubmitted", req.method)
	default:
	}
}
func TestContinuationRouteCompletedOrRunningOriginalNeedsNoContinuation(t *testing.T) {
	for _, status := range []string{"completed", "inProgress"} {
		t.Run(status, func(t *testing.T) {
			h, store, request := continuationRouteFixture(t)
			primitive, _ := continuationRequestFor(h)
			raw := strings.Replace(continuationHistoryJSON(primitive), `"status":"interrupted"`, `"status":"`+status+`"`, 1)
			go recoveryReply(t, h, "thread/read", raw, 6)
			result, err := callContinuationRoute(t, h, request)
			if err != nil || result.Status != "not_required" || result.NewTurnID != "" || result.Ordinal != 0 || len(store.History()) != 1 {
				t.Fatal(result, err)
			}
		})
	}
}
func TestContinuationRouteRefusesMissingHostAuthorityAndUnboundAdmission(t *testing.T) {
	for _, which := range []string{"nil", "denied-after-ack", "noncombined", "stale", "wrong-attempt", "wrong-text", "fresh-admission", "source-negative"} {
		t.Run(which, func(t *testing.T) {
			h, store, request := continuationRouteFixture(t)
			switch which {
			case "nil":
				h.verifyPending = nil
			case "denied-after-ack":
				h.verifyPending = func(context.Context, OwnedPendingRequest) error { return errors.New("source already acknowledged") }
			case "noncombined":
				h.combinedResumeDrain = false
			case "stale":
				request.Runtime = strings.Repeat("c", 64)
			case "wrong-attempt":
				request.Attempt = 2
			case "wrong-text":
				request.Text = "other"
			case "fresh-admission", "source-negative":
				var saved attentionOutcome
				_ = json.Unmarshal(store.stages[0].Payload, &saved)
				if which == "fresh-admission" {
					saved.AdmissionBinding = request.Runtime
				} else {
					saved.Status = "not_admitted"
				}
				store.stages[0].Payload, _ = json.Marshal(saved)
			}
			if _, err := callContinuationRoute(t, h, request); err == nil {
				t.Fatal("unbound continuation accepted")
			}
			if len(store.History()) != 1 {
				t.Fatal("rejected continuation journaled")
			}
			select {
			case req := <-h.router.requests:
				t.Fatal("rejected continuation queried native", req.method)
			default:
			}
		})
	}
}
func TestContinuationRouteRejectsClientAuthorizationBooleanAndTerminalSource(t *testing.T) {
	h, _, request := continuationRouteFixture(t)
	params, _ := json.Marshal(request)
	var fields map[string]any
	_ = json.Unmarshal(params, &fields)
	fields["ServicePendingUnfinished"] = true
	malformed, _ := json.Marshal(fields)
	if _, err := h.continuationRoute(context.Background(), boardSource, []byte(`1`), malformed); err == nil {
		t.Fatal("client supplied host authorization")
	}
	if _, err := h.continuationRoute(context.Background(), terminalSource, []byte(`1`), params); err == nil {
		t.Fatal("terminal requested continuation")
	}
}
func TestContinuationRouteExclusiveBarrierBlocksOrdinaryInput(t *testing.T) {
	h, _, request := continuationRouteFixture(t)
	primitive, _ := continuationRequestFor(h)
	entered, release := make(chan struct{}), make(chan struct{})
	h.verifyPending = func(ctx context.Context, _ OwnedPendingRequest) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan error, 1)
	go func() { _, err := callContinuationRoute(t, h, request); done <- err }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	_, err := h.handle(ctx, terminalSource, []byte(`{"id":2,"method":"thread/read","params":{"threadId":"thread"}}`))
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("ordinary input interleaved", err)
	}
	go func() {
		recoveryReply(t, h, "thread/read", continuationHistoryJSON(primitive), 6)
		serveContinuationChecks(t, h, primitive)
		serveOneEmptyContinuation(t, h)
	}()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestResumeDrainOnlyHistoricalInterruptedMayReachContinuationBaseline(t *testing.T) {
	d := testResumeDrain(t)
	raw := json.RawMessage(drainRead("interrupted", recoveryText, true))
	if _, err := d.history(raw); err == nil {
		t.Fatal("ordinary mode accepted interrupted admitted history")
	}
	d = testResumeDrain(t)
	d.allowInterruptedHistory = true
	delete(d.expected, recoveryDelivery) // Entire lifecycle predates this attachment.
	if done, err := d.history(raw); err != nil || !done {
		t.Fatal("host verifier failed to admit historical idle baseline", done, err)
	}
	d = testResumeDrain(t)
	d.allowInterruptedHistory = true
	d.expected[recoveryDelivery] = true
	d.turns["turn"] = true
	if done, err := d.history(raw); err != nil || done {
		t.Fatal("observed queued interrupted work bypassed drain", done, err)
	}
}
func TestContinuationRouteUnknownResultNeverResends(t *testing.T) {
	h, store, request := continuationRouteFixture(t)
	primitive, _ := continuationRequestFor(h)
	store.failOn = ".result"
	go func() {
		recoveryReply(t, h, "thread/read", continuationHistoryJSON(primitive), 6)
		serveContinuationChecks(t, h, primitive)
		serveOneEmptyContinuation(t, h)
	}()
	if _, err := callContinuationRoute(t, h, request); err == nil {
		t.Fatal("journal failure ignored")
	}
	store.failOn = ""
	result, err := callContinuationRoute(t, h, request)
	if err != nil || result.Status != "uncertain" || result.NewTurnID != "" {
		t.Fatal(result, err)
	}
	select {
	case req := <-h.router.requests:
		t.Fatal("unknown continuation repeated native request", req.method)
	default:
	}
}
