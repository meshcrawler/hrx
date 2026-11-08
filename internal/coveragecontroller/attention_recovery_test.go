package coveragecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/meshcrawler/hrx/internal/codexrpc"
)

const recoveryText = "body-free notification"

var recoveryDelivery = "delivery-" + strings.Repeat("a", 64)

func recoveryQueued(id, text string) string {
	raw, _ := json.Marshal(map[string]any{"id": id, "clientUserMessageId": recoveryDelivery,
		"input": []map[string]any{{"type": "text", "text": text, "text_elements": []any{}}}})
	return string(raw)
}
func recoveryHistory(thread, view, text string) string {
	raw, _ := json.Marshal(map[string]any{"thread": map[string]any{"id": thread, "turns": []any{
		map[string]any{"id": "turn", "itemsView": view, "items": []any{
			map[string]any{"id": "user-item", "type": "userMessage", "clientId": recoveryDelivery, "content": []any{
				map[string]any{"type": "text", "text": text, "text_elements": []any{}},
			}},
		}},
	}}})
	return string(raw)
}

func TestAttentionExactInputPinnedWireShape(t *testing.T) {
	// Pin 83086188: protocol/v2/turn.rs:439-448 and generated
	// schema/typescript/v2/UserInput.ts use snake_case text_elements. The
	// enum's rename_all affects variant names, not fields within Text.
	for _, tc := range []struct {
		name, fields string
		want         bool
	}{
		{"pinned", `,"text_elements":[]`, true},
		{"legacy-camel", `,"textElements":[]`, true},
		{"omitted-default", ``, true},
		{"mixed-aliases", `,"text_elements":[],"textElements":[]`, false},
		{"pinned-null", `,"text_elements":null`, false},
		{"pinned-nonempty", `,"text_elements":[{"start":0}]`, false},
		{"camel-null", `,"textElements":null`, false},
		{"camel-nonempty", `,"textElements":[{"start":0}]`, false},
		{"extra", `,"text_elements":[],"auxiliary":true`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := json.RawMessage(`[{"type":"text","text":"` + recoveryText + `"` + tc.fields + `}]`)
			if got := exactAttentionInput(raw, recoveryText); got != tc.want {
				t.Fatal("exact input shape result", got, "wanted", tc.want)
			}
			d := testResumeDrain(t)
			if got := d.exactInput(recoveryDelivery, raw); got != tc.want {
				t.Fatal("journal-bound queue/history result", got, "wanted", tc.want)
			}
			changed := json.RawMessage(strings.Replace(string(raw), recoveryText, "different text", 1))
			if d.exactInput(recoveryDelivery, changed) {
				t.Fatal("wire compatibility weakened exact payload digest")
			}
		})
	}
}
func recoveryReply(t *testing.T, h *gateway, method string, raw string, ordinal uint64) {
	t.Helper()
	req := <-h.router.requests
	if req.method != method {
		t.Errorf("unexpected native method %s, wanted %s", req.method, method)
	}
	req.reply <- workResult{response: codexrpc.Response{Ordinal: ordinal, Result: json.RawMessage(raw)}}
}
func seedAmbiguous(t *testing.T, h *gateway, store *attentionMemory, status string) {
	t.Helper()
	h.router.maxFrame = 1
	_ = attentionCall(t, h, recoveryText)
	h.router.maxFrame = 32768
	store.stages = store.stages[:2] // durable prepare + submitting; no final result
	if status != "submitting" {
		var outcome attentionOutcome
		_ = json.Unmarshal(store.stages[1].Payload, &outcome)
		outcome.Status = status
		store.stages[1].Payload, _ = json.Marshal(outcome)
	}
}

func TestAttentionPreparedNoWriteProofAndExplicitNextAttempt(t *testing.T) {
	h, store := attentionGateway(t)
	h.attentionTestHook = func(ctx context.Context, p AttentionTestPoint) error {
		if p.Phase != AttentionPrepared || p.ThreadID != "thread" || p.DeliveryID != recoveryDelivery || p.Attempt != 1 || p.ResponseOrdinal != 0 || p.PayloadSHA256 == "" {
			t.Error("invalid prepared seam facts")
		}
		return errors.New("test-owned pre-submit cancellation")
	}
	params, _ := json.Marshal(attentionRequest{Protocol: attentionProtocol, Runtime: h.attentionBinding(), Thread: "thread", Delivery: recoveryDelivery, Text: recoveryText, Attempt: 1})
	if _, err := h.attention(context.Background(), boardSource, json.RawMessage(`1`), params); err == nil {
		t.Fatal("hook error ignored")
	}
	if len(store.History()) != 1 {
		t.Fatal("native submission began before prepared hook returned")
	}
	h.attentionTestHook = nil
	result := attentionCall(t, h, recoveryText)
	if result.Status != "not_admitted" || result.Ordinal != 0 || len(store.History()) != 2 {
		t.Fatal(result)
	}
	select {
	case req := <-h.router.requests:
		t.Fatalf("proof unnecessarily queried native: %s", req.method)
	default:
	}
	go recoveryReply(t, h, "thread/queue/add", `{}`, 9)
	if next := attentionAttempt(t, h, recoveryText, 2); next.Status != "admitted" {
		t.Fatal(next)
	}
}

func TestAttentionLostQueueResponseReconcilesCompleteQueueWithoutResend(t *testing.T) {
	h, store := attentionGateway(t)
	h.attentionTestHook = func(ctx context.Context, p AttentionTestPoint) error {
		if p.Phase == AttentionQueueResponse {
			if p.ResponseOrdinal != 7 {
				t.Error("response seam lacks consumed native ordinal")
			}
			return errors.New("test-owned crash after queue response")
		}
		return nil
	}
	go recoveryReply(t, h, "thread/queue/add", `{}`, 7)
	params, _ := json.Marshal(attentionRequest{Protocol: attentionProtocol, Runtime: h.attentionBinding(), Thread: "thread", Delivery: recoveryDelivery, Text: recoveryText, Attempt: 1})
	if _, err := h.attention(context.Background(), boardSource, json.RawMessage(`1`), params); err == nil {
		t.Fatal("hook error ignored")
	}
	if len(store.History()) != 2 {
		t.Fatal("admitted committed past response pause")
	}
	h.attentionTestHook = nil
	go func() {
		recoveryReply(t, h, "thread/queue/list", fmt.Sprintf(`{"data":[%s],"nextCursor":"page-2"}`, recoveryQueued("queued", recoveryText)), 8)
		req := <-h.router.requests
		var p struct {
			Thread string `json:"threadId"`
			Cursor string `json:"cursor"`
			Limit  int    `json:"limit"`
		}
		if req.method != "thread/queue/list" || json.Unmarshal(req.params, &p) != nil || p.Thread != "thread" || p.Cursor != "page-2" || p.Limit != 64 {
			t.Error("queue pagination not bound to original thread")
		}
		req.reply <- workResult{response: codexrpc.Response{Ordinal: 9, Result: json.RawMessage(`{"data":[],"nextCursor":null}`)}}
	}()
	first := attentionCall(t, h, recoveryText)
	if first.Status != "admitted" || first.Ordinal != 0 || first.RecoveryOrdinal != 8 || first.RecoverySource != "queue" || first.RecoveryBinding != h.attentionBinding() || len(store.History()) != 3 || !strings.HasSuffix(store.History()[2].Operation, "recovered_admitted") {
		t.Fatal(first)
	}
	if replay := attentionCall(t, h, recoveryText); replay != first {
		t.Fatal(replay)
	}
}

func TestAttentionAmbiguousHistoryPositiveIsDurableWithoutResend(t *testing.T) {
	for _, status := range []string{"submitting", "prepared", "uncertain"} {
		t.Run(status, func(t *testing.T) {
			h, store := attentionGateway(t)
			seedAmbiguous(t, h, store, status)
			originalBinding := strings.Repeat("b", 64)
			for i := range store.stages {
				var saved attentionOutcome
				_ = json.Unmarshal(store.stages[i].Payload, &saved)
				saved.AdmissionBinding = originalBinding
				store.stages[i].Payload, _ = json.Marshal(saved)
			}
			go func() {
				recoveryReply(t, h, "thread/queue/list", `{"data":[],"nextCursor":null}`, 8)
				req := <-h.router.requests
				var p struct {
					Thread  string `json:"threadId"`
					Include bool   `json:"includeTurns"`
				}
				if req.method != "thread/read" || json.Unmarshal(req.params, &p) != nil || p.Thread != "thread" || !p.Include {
					t.Error("not an exact full-history read")
				}
				req.reply <- workResult{response: codexrpc.Response{Ordinal: 9, Result: json.RawMessage(recoveryHistory("thread", "full", recoveryText))}}
			}()
			result := attentionCall(t, h, recoveryText)
			if result.Status != "admitted" || result.Ordinal != 0 || result.RecoveryOrdinal != 9 || result.RecoverySource != "history" || result.RecoveryBinding != h.attentionBinding() || result.AdmissionBinding != originalBinding || result.Runtime != h.attentionBinding() || len(store.History()) != 3 {
				t.Fatal(result)
			}
		})
	}
}

func TestAttentionRecoveryNoMatchAndInvalidEvidenceRemainUncertain(t *testing.T) {
	cases := []struct{ name, queue, history string }{
		{"absent", `{"data":[],"nextCursor":null}`, `{"thread":{"id":"thread","turns":[]}}`},
		{"wrong-thread", `{"data":[],"nextCursor":null}`, recoveryHistory("foreign", "full", recoveryText)},
		{"summary-is-not-full", `{"data":[],"nextCursor":null}`, recoveryHistory("thread", "summary", recoveryText)},
		{"different-history-payload", `{"data":[],"nextCursor":null}`, recoveryHistory("thread", "full", "other")},
		{"different-queue-payload", fmt.Sprintf(`{"data":[%s],"nextCursor":null}`, recoveryQueued("queued", "other")), ""},
		{"duplicate-queue-admission", fmt.Sprintf(`{"data":[%s,%s],"nextCursor":null}`, recoveryQueued("one", recoveryText), recoveryQueued("two", recoveryText)), ""},
		{"duplicate-item-id", fmt.Sprintf(`{"data":[%s,%s],"nextCursor":null}`, recoveryQueued("same", recoveryText), recoveryQueued("same", recoveryText)), ""},
		{"incomplete-queue", `{"data":[]}`, ""},
		{"malformed-queue", `{"data":[],"data":[],"nextCursor":null}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, store := attentionGateway(t)
			seedAmbiguous(t, h, store, "submitting")
			go func() {
				recoveryReply(t, h, "thread/queue/list", tc.queue, 8)
				if tc.history != "" {
					recoveryReply(t, h, "thread/read", tc.history, 9)
				}
			}()
			result := attentionCall(t, h, recoveryText)
			if result.Status != "uncertain" || len(store.History()) != 2 {
				t.Fatal(result)
			}
			select {
			case req := <-h.router.requests:
				t.Fatalf("recovery emitted extra method %s", req.method)
			default:
			}
		})
	}
}

func TestAttentionRecoveryPaginationLimitNeverAssumesAbsent(t *testing.T) {
	h, store := attentionGateway(t)
	seedAmbiguous(t, h, store, "submitting")
	go func() {
		for i := 0; i < attentionRecoveryPages; i++ {
			recoveryReply(t, h, "thread/queue/list", fmt.Sprintf(`{"data":[],"nextCursor":"page-%d"}`, i), uint64(i+1))
		}
	}()
	if result := attentionCall(t, h, recoveryText); result.Status != "uncertain" || len(store.History()) != 2 {
		t.Fatal(result)
	}
}

func TestAttentionRecoveryCannotEscapeJournalFailure(t *testing.T) {
	h, store := attentionGateway(t)
	seedAmbiguous(t, h, store, "submitting")
	store.failOn = "recovered_admitted"
	go recoveryReply(t, h, "thread/queue/list", fmt.Sprintf(`{"data":[%s],"nextCursor":null}`, recoveryQueued("queued", recoveryText)), 8)
	params, _ := json.Marshal(attentionRequest{Protocol: attentionProtocol, Runtime: h.attentionBinding(), Thread: "thread", Delivery: recoveryDelivery, Text: recoveryText, Attempt: 1})
	if _, err := h.attention(context.Background(), boardSource, json.RawMessage(`1`), params); err == nil || len(store.History()) != 2 {
		t.Fatal("recovery escaped failed durable commit")
	}
}

func TestAttentionRecoveryQueueReadStillRequiresOwnedThreadAndFixedFields(t *testing.T) {
	c, _, _, _ := actualCoordinator(t)
	for _, tc := range []struct {
		params string
		valid  bool
	}{
		{`{"threadId":"thread","cursor":"page","limit":64}`, true},
		{`{"threadId":"foreign","limit":64}`, false},
		{`{"threadId":"thread","input":[]}`, false},
		{`{"threadId":"thread","threadId":"foreign"}`, false},
	} {
		req, err := newWorkRequest(context.Background(), "thread/queue/list", json.RawMessage(tc.params), 32768)
		if err != nil {
			t.Fatal(err)
		}
		if valid := c.validateWork(req) == nil; valid != tc.valid {
			t.Fatalf("queue read scope validation=%v", valid)
		}
	}
	h, _ := attentionGateway(t)
	if _, err := h.router.dispatch(context.Background(), boardSource, []byte(`{"id":1,"method":"thread/queue/list","params":{"threadId":"thread"}}`)); !errors.Is(err, codexrpc.ErrRequest) {
		t.Fatal("auxiliary ingress bypassed internal recovery read scope", err)
	}
}

func TestAttentionRecoveryRejectsUnavailableOrOverBoundResponses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response codexrpc.Response
		err      error
	}{
		{"native-error", codexrpc.Response{Ordinal: 8, RemoteError: json.RawMessage(`{"code":-1}`)}, nil},
		{"no-ordinal", codexrpc.Response{Result: json.RawMessage(`{"data":[],"nextCursor":null}`)}, nil},
		{"over-bound", codexrpc.Response{Ordinal: 8, Result: json.RawMessage(`{"padding":"` + strings.Repeat("a", 32768) + `"}`)}, nil},
		{"fence-refused", codexrpc.Response{}, codexrpc.ErrPublicationRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, store := attentionGateway(t)
			seedAmbiguous(t, h, store, "submitting")
			go func() { req := <-h.router.requests; req.reply <- workResult{response: tc.response, err: tc.err} }()
			if result := attentionCall(t, h, recoveryText); result.Status != "uncertain" || len(store.History()) != 2 {
				t.Fatal(result)
			}
		})
	}
}

func TestAttentionRecoveryRejectsCursorCyclesEvenAfterPositiveMatch(t *testing.T) {
	h, store := attentionGateway(t)
	seedAmbiguous(t, h, store, "submitting")
	go func() {
		recoveryReply(t, h, "thread/queue/list", fmt.Sprintf(`{"data":[%s],"nextCursor":"same"}`, recoveryQueued("queued", recoveryText)), 8)
		recoveryReply(t, h, "thread/queue/list", `{"data":[],"nextCursor":"same"}`, 9)
	}()
	if result := attentionCall(t, h, recoveryText); result.Status != "uncertain" || len(store.History()) != 2 {
		t.Fatal(result)
	}
}

func TestAttentionQueueRecoveryAllowsUnrelatedOptionalClientID(t *testing.T) {
	for _, optional := range []string{"", `,"clientUserMessageId":null`, `,"clientUserMessageId":""`} {
		t.Run(optional, func(t *testing.T) {
			h, store := attentionGateway(t)
			seedAmbiguous(t, h, store, "submitting")
			ordinary := `{"id":"ordinary"` + optional + `,"input":[{"type":"text","text":"ordinary foreground work"}]}`
			go recoveryReply(t, h, "thread/queue/list", fmt.Sprintf(`{"data":[%s,%s],"nextCursor":null}`, ordinary, recoveryQueued("target", recoveryText)), 12)
			result := attentionCall(t, h, recoveryText)
			if result.Status != "admitted" || result.RecoverySource != "queue" || result.RecoveryOrdinal != 12 {
				t.Fatal(result)
			}
			select {
			case req := <-h.router.requests:
				t.Fatalf("extra native method %s", req.method)
			default:
			}
		})
	}
}
