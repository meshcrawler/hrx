package coveragecontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/meshcrawler/hrx/internal/codexrpc"
)

const continuationResponse = `{"turn":{"id":"new-turn","status":"inProgress","itemsView":"notLoaded","items":[]}}`

func continuationRequestFor(h *gateway) (attentionContinuationRequest, attentionContinuationAuthorization) {
	digest := sha256.Sum256([]byte(recoveryText))
	request := attentionContinuationRequest{Runtime: h.attentionBinding(), Thread: "thread", Delivery: recoveryDelivery,
		Digest: hex.EncodeToString(digest[:]), OriginalTurnID: "original-turn", Text: recoveryText}
	authorization := attentionContinuationAuthorization{Runtime: request.Runtime, Delivery: request.Delivery,
		Digest: request.Digest, OriginalTurnID: request.OriginalTurnID, ServicePendingUnfinished: true, ExclusiveInputHeld: true}
	return request, authorization
}
func continuationHistoryDocument(request attentionContinuationRequest) map[string]any {
	return map[string]any{"thread": map[string]any{"id": request.Thread, "status": map[string]any{"type": "idle"}, "turns": []any{
		map[string]any{"id": request.OriginalTurnID, "status": "interrupted", "itemsView": "full", "items": []any{
			map[string]any{"type": "userMessage", "clientId": request.Delivery, "content": []any{
				map[string]any{"type": "text", "text": request.Text, "text_elements": []any{}},
			}},
		}},
	}}}
}
func continuationHistoryJSON(request attentionContinuationRequest) string {
	raw, _ := json.Marshal(continuationHistoryDocument(request))
	return string(raw)
}
func serveContinuationChecks(t *testing.T, h *gateway, request attentionContinuationRequest) {
	t.Helper()
	recoveryReply(t, h, "thread/queue/list", `{"data":[],"nextCursor":null}`, 8)
	req := <-h.router.requests
	var params map[string]json.RawMessage
	if req.method != "thread/read" || json.Unmarshal(req.params, &params) != nil || string(params["includeTurns"]) != "true" || string(params["threadId"]) != `"thread"` {
		t.Error("not a fenced full-history read")
	}
	req.reply <- workResult{response: codexrpc.Response{Ordinal: 9, Result: json.RawMessage(continuationHistoryJSON(request))}}
}
func serveOneEmptyContinuation(t *testing.T, h *gateway) {
	t.Helper()
	req := <-h.router.requests
	var params map[string]json.RawMessage
	if req.method != "turn/start" || json.Unmarshal(req.params, &params) != nil || len(params) != 2 || string(params["threadId"]) != `"thread"` || string(params["input"]) != `[]` {
		t.Error("continuation resent notification, changed settings or used another method")
	}
	req.reply <- workResult{response: codexrpc.Response{Ordinal: 10, Result: json.RawMessage(continuationResponse)}}
}

func TestAttentionContinuationExactlyOneEmptyTurnAndStableResultReplay(t *testing.T) {
	h, store := attentionGateway(t)
	request, auth := continuationRequestFor(h)
	go func() { serveContinuationChecks(t, h, request); serveOneEmptyContinuation(t, h) }()
	result, err := h.continueAttention(context.Background(), request, auth)
	if err != nil || result.Status != "started" || result.NewTurnID != "new-turn" || result.OriginalTurnID != "original-turn" || result.Ordinal != 10 || result.Protocol == attentionProtocol {
		t.Fatal(result, err)
	}
	stages := store.History()
	if len(stages) != 3 || stages[0].Operation != continuationKey(request)+".prepared" || stages[1].Operation != continuationKey(request)+".submitting" || stages[2].Operation != continuationKey(request)+".result" {
		t.Fatal("continuation intent/result not durably ordered")
	}
	replay, err := h.continueAttention(context.Background(), request, auth)
	if err != nil || replay != result || len(store.History()) != 3 {
		t.Fatal(replay, err)
	}
	// Reopen history under another owner: native result identity remains stable.
	resumed, _ := attentionGateway(t)
	resumed.store = store
	resumedRequest, resumedAuthorization := continuationRequestFor(resumed)
	resumedResult, err := resumed.continueAttention(context.Background(), resumedRequest, resumedAuthorization)
	if err != nil || resumedResult.NewTurnID != result.NewTurnID || resumedResult.Ordinal != result.Ordinal ||
		resumedResult.SubmissionBinding != result.SubmissionBinding || resumedResult.Runtime != resumedRequest.Runtime {
		t.Fatal("restart lost original continuation result", resumedResult, err)
	}
	select {
	case req := <-resumed.router.requests:
		t.Fatal("restart repeated native work", req.method)
	default:
	}
	select {
	case req := <-h.router.requests:
		t.Fatal("known result repeated native work", req.method)
	default:
	}
}

func TestAttentionContinuationRequiresFreshBoundAuthorizationAndExclusiveInput(t *testing.T) {
	for _, which := range []string{"absent", "no-exclusive-barrier", "stale-runtime", "other-delivery", "other-digest", "other-original-turn", "bad-request-digest", "other-thread", "stopped-owner"} {
		t.Run(which, func(t *testing.T) {
			h, store := attentionGateway(t)
			request, auth := continuationRequestFor(h)
			switch which {
			case "absent":
				auth.ServicePendingUnfinished = false
			case "no-exclusive-barrier":
				auth.ExclusiveInputHeld = false
			case "stale-runtime":
				auth.Runtime = "old"
			case "other-delivery":
				auth.Delivery = "other"
			case "other-digest":
				auth.Digest = "other"
			case "other-original-turn":
				auth.OriginalTurnID = "other"
			case "bad-request-digest":
				request.Digest = "other"
				auth.Digest = "other"
			case "other-thread":
				request.Thread = "foreign"
			case "stopped-owner":
				h.state.Store(2)
			}
			if _, err := h.continueAttention(context.Background(), request, auth); !errors.Is(err, errAttention) {
				t.Fatal("unproven authorization accepted", err)
			}
			if len(store.History()) != 0 {
				t.Fatal("unauthorized continuation journaled")
			}
			select {
			case req := <-h.router.requests:
				t.Fatal("unauthorized continuation queried native", req.method)
			default:
			}
		})
	}
}

func TestAttentionContinuationRefusesChangedOrIncompleteNativeHistory(t *testing.T) {
	for _, which := range []string{"nonidle", "foreign-thread", "missing-original-turn", "completed-original-turn", "later-turn", "later-input", "wrong-client-id", "wrong-content", "partial-items", "missing-items", "duplicate-turn", "no-turns"} {
		t.Run(which, func(t *testing.T) {
			h, store := attentionGateway(t)
			request, auth := continuationRequestFor(h)
			doc := continuationHistoryDocument(request)
			thread := doc["thread"].(map[string]any)
			turns := thread["turns"].([]any)
			turn := turns[0].(map[string]any)
			items := turn["items"].([]any)
			item := items[0].(map[string]any)
			switch which {
			case "nonidle":
				thread["status"] = map[string]any{"type": "active"}
			case "foreign-thread":
				thread["id"] = "other"
			case "missing-original-turn":
				turn["id"] = "other"
			case "completed-original-turn":
				turn["status"] = "completed"
			case "later-turn":
				thread["turns"] = append(turns, map[string]any{"id": "later", "status": "completed", "itemsView": "full", "items": []any{}})
			case "later-input":
				turn["items"] = append(items, map[string]any{"type": "userMessage", "clientId": "other", "content": []any{}})
			case "wrong-client-id":
				item["clientId"] = "other"
			case "wrong-content":
				item["content"] = []any{map[string]any{"type": "text", "text": "other"}}
			case "partial-items":
				turn["itemsView"] = "summary"
			case "missing-items":
				delete(turn, "items")
			case "duplicate-turn":
				thread["turns"] = append(turns, turn)
			case "no-turns":
				thread["turns"] = []any{}
			}
			raw, _ := json.Marshal(doc)
			go func() {
				recoveryReply(t, h, "thread/queue/list", `{"data":[],"nextCursor":null}`, 8)
				recoveryReply(t, h, "thread/read", string(raw), 9)
			}()
			if _, err := h.continueAttention(context.Background(), request, auth); !errors.Is(err, errAttention) {
				t.Fatal("unsafe native history accepted", err)
			}
			if len(store.History()) != 0 {
				t.Fatal("unsafe history created intent")
			}
		})
	}
}

func TestAttentionContinuationRequiresEmptyQueue(t *testing.T) {
	for _, queue := range []string{`{"data":[{"id":"queued"}],"nextCursor":null}`, `{"data":[],"nextCursor":"more"}`, `{"data":[]}`, `{"data":null,"nextCursor":null}`} {
		t.Run(queue, func(t *testing.T) {
			h, store := attentionGateway(t)
			request, auth := continuationRequestFor(h)
			go recoveryReply(t, h, "thread/queue/list", queue, 8)
			if _, err := h.continueAttention(context.Background(), request, auth); !errors.Is(err, errAttention) || len(store.History()) != 0 {
				t.Fatal("nonempty or incomplete queue accepted", err)
			}
		})
	}
}

func TestAttentionContinuationJournalFailuresNeverResendUnknownSubmission(t *testing.T) {
	for _, phase := range []string{"prepared", "submitting", "result"} {
		t.Run(phase, func(t *testing.T) {
			h, store := attentionGateway(t)
			request, auth := continuationRequestFor(h)
			store.failOn = "." + phase
			go func() {
				serveContinuationChecks(t, h, request)
				if phase == "result" {
					serveOneEmptyContinuation(t, h)
				}
			}()
			if _, err := h.continueAttention(context.Background(), request, auth); err == nil {
				t.Fatal("journal failure ignored")
			}
			expected := map[string]int{"prepared": 0, "submitting": 1, "result": 2}[phase]
			if len(store.History()) != expected {
				t.Fatal("unexpected durable prefix")
			}
			if phase == "result" {
				store.failOn = ""
				result, err := h.continueAttention(context.Background(), request, auth)
				if err != nil || result.Status != "uncertain" || result.NewTurnID != "" || result.Ordinal != 0 || len(store.History()) != 2 {
					t.Fatal("lost result retried or invented native evidence", result, err)
				}
			}
		})
	}
}

func TestAttentionContinuationPreparedOnlyReplayRevalidatesBeforeSingleSubmit(t *testing.T) {
	h, store := attentionGateway(t)
	request, auth := continuationRequestFor(h)
	store.failOn = ".submitting"
	go serveContinuationChecks(t, h, request)
	if _, err := h.continueAttention(context.Background(), request, auth); err == nil {
		t.Fatal("expected injected pre-submit failure")
	}
	store.failOn = ""
	go func() { serveContinuationChecks(t, h, request); serveOneEmptyContinuation(t, h) }()
	result, err := h.continueAttention(context.Background(), request, auth)
	if err != nil || result.Status != "started" || len(store.History()) != 3 {
		t.Fatal(result, err)
	}
}

func TestAttentionContinuationMalformedOrLostNativeStartResultStaysUncertain(t *testing.T) {
	for _, raw := range []string{`{}`, `{"turn":{"id":"original-turn","status":"inProgress","itemsView":"notLoaded","items":[]}}`,
		`{"turn":{"id":"new-turn","status":"completed","itemsView":"notLoaded","items":[]}}`,
		`{"turn":{"id":"new-turn","status":"inProgress","itemsView":"summary","items":[]}}`,
		`{"turn":{"id":"new-turn","status":"inProgress","itemsView":"notLoaded"}}`,
		`{"turn":{"id":"","status":"inProgress","itemsView":"notLoaded","items":[]}}`,
		`{"turn":{"id":"new-turn","id":"another","status":"inProgress","itemsView":"notLoaded","items":[]}}`} {
		t.Run(raw, func(t *testing.T) {
			h, store := attentionGateway(t)
			request, auth := continuationRequestFor(h)
			go func() { serveContinuationChecks(t, h, request); recoveryReply(t, h, "turn/start", raw, 10) }()
			first, err := h.continueAttention(context.Background(), request, auth)
			if err != nil || first.Status != "uncertain" || first.NewTurnID != "" || first.Ordinal != 0 {
				t.Fatal(first, err)
			}
			second, err := h.continueAttention(context.Background(), request, auth)
			if err != nil || second != first || len(store.History()) != 3 {
				t.Fatal(second, err)
			}
		})
	}
}

func TestAttentionContinuationKnownResultStillRequiresUnfinishedAuthorization(t *testing.T) {
	h, _ := attentionGateway(t)
	request, auth := continuationRequestFor(h)
	go func() { serveContinuationChecks(t, h, request); serveOneEmptyContinuation(t, h) }()
	if _, err := h.continueAttention(context.Background(), request, auth); err != nil {
		t.Fatal(err)
	}
	auth.ServicePendingUnfinished = false
	if _, err := h.continueAttention(context.Background(), request, auth); !errors.Is(err, errAttention) {
		t.Fatal("known result bypassed current service authorization", err)
	}
}

func TestAttentionContinuationNoNativeReadBypassForForeignThread(t *testing.T) {
	c, _, _, _ := actualCoordinator(t)
	req, _ := newWorkRequest(context.Background(), "turn/start", json.RawMessage(`{"threadId":"foreign","input":[]}`), 32768)
	if !errors.Is(c.validateWork(req), codexrpc.ErrRequest) {
		t.Fatal("continuation escaped owned thread fence")
	}
	h, store := attentionGateway(t)
	request, auth := continuationRequestFor(h)
	store.failOn = ".submitting"
	go serveContinuationChecks(t, h, request)
	_, _ = h.continueAttention(context.Background(), request, auth)
	// A different digest/original turn cannot create another intent for a delivery.
	request.OriginalTurnID = "other"
	auth.OriginalTurnID = "other"
	if _, err := h.continueAttention(context.Background(), request, auth); !errors.Is(err, errAttention) {
		t.Fatal("conflicting continuation identity accepted", err)
	}
	if !strings.HasSuffix(store.History()[0].Operation, ".prepared") {
		t.Fatal("lost original intent")
	}
}

func TestAttentionContinuationLostStartResponseNeverRetries(t *testing.T) {
	h, store := attentionGateway(t)
	request, auth := continuationRequestFor(h)
	go func() {
		serveContinuationChecks(t, h, request)
		req := <-h.router.requests
		if req.method != "turn/start" {
			t.Error("unexpected native method")
		}
		req.reply <- workResult{err: context.DeadlineExceeded}
	}()
	first, err := h.continueAttention(context.Background(), request, auth)
	if err != nil || first.Status != "uncertain" || first.Ordinal != 0 || first.NewTurnID != "" {
		t.Fatal(first, err)
	}
	second, err := h.continueAttention(context.Background(), request, auth)
	if err != nil || second != first || len(store.History()) != 3 {
		t.Fatal(second, err)
	}
	select {
	case req := <-h.router.requests:
		t.Fatal("lost native response triggered retry", req.method)
	default:
	}
}
