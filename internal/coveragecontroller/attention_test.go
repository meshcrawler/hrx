package coveragecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/codexlaunch"
	"github.com/meshcrawler/hrx/internal/codexrpc"
	"github.com/meshcrawler/hrx/internal/coveragejournal"
)

type attentionMemory struct {
	mu     sync.Mutex
	stages []coveragejournal.Stage
	failOn string
}

func (s *attentionMemory) Append(_ context.Context, stage coveragejournal.Stage) (coveragejournal.Durability, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.HasSuffix(stage.Operation, s.failOn) && s.failOn != "" {
		return coveragejournal.Indeterminate, errors.New("injected journal failure")
	}
	s.stages = append(s.stages, stage)
	return coveragejournal.Durable, nil
}
func (s *attentionMemory) History() []coveragejournal.Stage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]coveragejournal.Stage(nil), s.stages...)
}
func attentionGateway(t *testing.T) (*gateway, *attentionMemory) {
	t.Helper()
	g, w := actualGate(t)
	t.Cleanup(func() { w.Close(); g.client.Stop(context.Background()) })
	router, err := newWorkRouter(g, 4, 32768)
	if err != nil {
		t.Fatal(err)
	}
	store := &attentionMemory{}
	h := &gateway{g: g, router: router, store: store, stageBudget: time.Second,
		b: &bootstrap{native: codexlaunch.Native{Identity: codexlaunch.Identity{ThreadID: "thread", SessionID: "session"}}}, cancelCause: func(error) {}}
	h.state.Store(1)
	return h, store
}
func attentionCall(t *testing.T, h *gateway, text string) attentionOutcome {
	return attentionAttempt(t, h, text, 1)
}
func attentionAttempt(t *testing.T, h *gateway, text string, attempt uint64) attentionOutcome {
	t.Helper()
	params, _ := json.Marshal(attentionRequest{Protocol: attentionProtocol, Runtime: h.attentionBinding(), Thread: "thread", Delivery: "delivery-" + strings.Repeat("a", 64), Text: text, Attempt: attempt})
	raw, err := h.attention(context.Background(), boardSource, json.RawMessage(`"request"`), params)
	if err != nil {
		t.Fatal(err)
	}
	var reply struct{ Result attentionOutcome }
	if json.Unmarshal(raw, &reply) != nil {
		t.Fatal(string(raw))
	}
	return reply.Result
}
func TestAttentionAdmissionJournalAndReplayNeverRepeatNativeWork(t *testing.T) {
	h, store := attentionGateway(t)
	calls := 0
	go func() {
		req := <-h.router.requests
		calls++
		req.reply <- workResult{response: codexrpc.Response{Ordinal: 9, Result: json.RawMessage(`{}`)}}
	}()
	first := attentionCall(t, h, "body-free notification")
	encoded, _ := json.Marshal(first)
	if strings.Contains(string(encoded), "recovery_") {
		t.Fatal("normal outcome changed wire shape")
	}
	second := attentionCall(t, h, "body-free notification")
	if first.Status != "admitted" || first != second || calls != 1 || len(store.History()) != 3 {
		t.Fatal(first, second, calls)
	}
	if store.History()[0].Operation == store.History()[1].Operation {
		t.Fatal("outcome overwrote preparation")
	}
}
func TestAttentionValidationFailureIsDefiniteNonadmission(t *testing.T) {
	h, store := attentionGateway(t)
	h.router.maxFrame = 1
	result := attentionCall(t, h, "body-free notification")
	if result.Status != "not_admitted" || result.Ordinal != 0 || len(store.History()) != 3 {
		t.Fatal(result)
	}
	select {
	case <-h.router.requests:
		t.Fatal("invalid request reached native owner")
	default:
	}
}
func TestAttentionPreparedHistoryIsUncertainAndNeverResubmitted(t *testing.T) {
	h, store := attentionGateway(t)
	h.router.maxFrame = 1
	_ = attentionCall(t, h, "body-free notification")
	store.stages = store.stages[:1]
	store.stages[0].Operation = strings.TrimSuffix(store.stages[0].Operation, "prepared_no_write_v1") + "prepared"
	result := attentionCall(t, h, "body-free notification")
	if result.Status != "uncertain" || len(store.History()) != 1 {
		t.Fatal(result)
	}
}
func TestAttentionReplayRetainsOriginalAdmissionBinding(t *testing.T) {
	h, store := attentionGateway(t)
	go func() {
		req := <-h.router.requests
		req.reply <- workResult{response: codexrpc.Response{Ordinal: 9, Result: json.RawMessage(`{}`)}}
	}()
	first := attentionCall(t, h, "body-free notification")
	var saved attentionOutcome
	_ = json.Unmarshal(store.stages[2].Payload, &saved)
	saved.AdmissionBinding = strings.Repeat("b", 64)
	store.stages[2].Payload, _ = json.Marshal(saved)
	result := attentionCall(t, h, "body-free notification")
	if result.Runtime != first.Runtime || result.AdmissionBinding != strings.Repeat("b", 64) || result.Status != "admitted" {
		t.Fatal(result)
	}
}

func TestAttentionLostNegativeResponseReconcilesBeforeExplicitNextAttempt(t *testing.T) {
	h, store := attentionGateway(t)
	h.router.maxFrame = 1
	first := attentionAttempt(t, h, "body-free notification", 1)
	replay := attentionAttempt(t, h, "body-free notification", 1)
	if first.Status != "not_admitted" || first != replay || len(store.History()) != 3 {
		t.Fatal(first, replay)
	}
	h.router.maxFrame = 32768
	go func() {
		req := <-h.router.requests
		req.reply <- workResult{response: codexrpc.Response{Ordinal: 9, Result: json.RawMessage(`{}`)}}
	}()
	success := attentionAttempt(t, h, "body-free notification", 2)
	if success.Status != "admitted" || success.Attempt != 2 || len(store.History()) != 6 {
		t.Fatal(success)
	}
}
func TestAttentionRecoversAfterMoreThan32DefiniteRefusals(t *testing.T) {
	h, store := attentionGateway(t)
	h.router.maxFrame = 1
	for attempt := uint64(1); attempt <= 40; attempt++ {
		if result := attentionAttempt(t, h, "body-free notification", attempt); result.Status != "not_admitted" {
			t.Fatal(result)
		}
	}
	h.router.maxFrame = 32768
	go func() {
		req := <-h.router.requests
		req.reply <- workResult{response: codexrpc.Response{Ordinal: 9, Result: json.RawMessage(`{}`)}}
	}()
	success := attentionAttempt(t, h, "body-free notification", 41)
	if success.Status != "admitted" || success.Attempt != 41 || len(store.History()) != 123 {
		t.Fatal(success)
	}
}
