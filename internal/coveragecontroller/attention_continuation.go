package coveragecontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/meshcrawler/hrx/internal/coveragejournal"
)

const attentionContinuationProtocol = "hrx-owned-attention-continuation.v1"

// This is an internal primitive, NOT a public RPC or an ACK/handling operation.
// The caller must freshly authenticate and generation-fence the exact service
// delivery, prove its handling remains unfinished, and hold a real exclusive
// input barrier across this entire call. Local pending/admission/interrupted
// history does NOT establish service authorization or absence of a remote ACK.
// These facts must never be accepted as an untrusted client's boolean assertion.
// Runtime binds their scope to this exact live owned connection.
type attentionContinuationAuthorization struct {
	Runtime, Delivery, Digest, OriginalTurnID string
	ServicePendingUnfinished                  bool
	ExclusiveInputHeld                        bool
}

type attentionContinuationRequest struct {
	Runtime, Thread, Delivery, Digest, OriginalTurnID, Text string
}

// Started means one new empty turn was accepted, not that the original input
// was handled. SubmissionBinding/Ordinal describe its actual native response;
// Runtime is rewritten to the current owner on replay. No source outcome is
// relabeled, no original notification is resent, and no ACK is invented.
type attentionContinuationOutcome struct {
	Protocol          string `json:"protocol"`
	Runtime           string `json:"runtime_binding"`
	SubmissionBinding string `json:"submission_binding"`
	Thread            string `json:"thread_id"`
	Delivery          string `json:"delivery_id"`
	Digest            string `json:"payload_sha256"`
	OriginalTurnID    string `json:"original_turn_id"`
	NewTurnID         string `json:"new_turn_id,omitempty"`
	Status            string `json:"status"`
	Ordinal           uint64 `json:"response_ordinal"`
}

func continuationKey(request attentionContinuationRequest) string {
	raw, _ := json.Marshal([]string{attentionContinuationProtocol, request.Thread, request.Delivery, request.Digest, request.OriginalTurnID})
	sum := sha256.Sum256(raw)
	return "attention.continue." + hex.EncodeToString(sum[:])
}

// Pin: openai/codex rust-v0.163.0 / 83086188ae0614bd882818ff1aa23f3194962012.
// app-server/src/request_processors/turn_processor.rs:599-677 routes empty input
// through start_or_steer; core/src/session/turn_input.rs:429-451 starts a NEW
// regular turn without a user-input item; :846-847 rejects empty active steering.
// core/src/session/turn.rs:553-569 builds provider input from native history.
// core/src/session/mod.rs:1097-1127 generates a fresh submission ID and dropping
// the waiter does not retract the request. Therefore submitting-without-result
// is uncertain forever here, including a second crash during continuation.
// This supports the fixed original-crash case, not general crash-safe recovery.
func (h *gateway) continueAttention(ctx context.Context, request attentionContinuationRequest, authorization attentionContinuationAuthorization) (attentionContinuationOutcome, error) {
	var none attentionContinuationOutcome
	if ctx == nil || ctx.Err() != nil || h.state.Load() != 1 || !h.attentionMu.TryLock() {
		return none, errAttention
	}
	defer h.attentionMu.Unlock()
	select {
	case <-h.g.done():
		return none, errAttention
	default:
	}
	ctx, cancel := context.WithTimeout(ctx, h.stageBudget)
	defer cancel()
	digest := sha256.Sum256([]byte(request.Text))
	if request.Runtime != h.attentionBinding() || request.Thread != h.b.native.Identity.ThreadID ||
		!attentionID.MatchString(request.Delivery) || len(request.Text) == 0 || len(request.Text) > 16384 ||
		request.Digest != hex.EncodeToString(digest[:]) || strings.TrimSpace(request.OriginalTurnID) == "" || len(request.OriginalTurnID) > 512 ||
		!authorization.ServicePendingUnfinished || !authorization.ExclusiveInputHeld ||
		authorization.Runtime != request.Runtime || authorization.Delivery != request.Delivery ||
		authorization.Digest != request.Digest || authorization.OriginalTurnID != request.OriginalTurnID {
		return none, errAttention
	}
	history, ok := h.store.(attentionHistory)
	if !ok {
		return none, errAttention
	}
	key := continuationKey(request)
	var previous *attentionContinuationOutcome
	for _, stage := range history.History() {
		var saved attentionContinuationOutcome
		if json.Unmarshal(stage.Payload, &saved) != nil || saved.Protocol != attentionContinuationProtocol || saved.Delivery != request.Delivery {
			continue
		}
		if saved.Thread != request.Thread || saved.Digest != request.Digest || saved.OriginalTurnID != request.OriginalTurnID {
			return none, errAttention
		}
		if stage.Operation != key+"."+saved.Status && !(saved.Status == "started" && stage.Operation == key+".result") {
			return none, errAttention
		}
		copy := saved
		previous = &copy
	}
	if previous != nil {
		switch previous.Status {
		case "started":
			if !continuationTurnID(previous.NewTurnID) || previous.NewTurnID == request.OriginalTurnID || previous.Ordinal == 0 || previous.SubmissionBinding == "" {
				return none, errAttention
			}
			previous.Runtime = request.Runtime
			return *previous, nil
		case "submitting", "uncertain":
			previous.Status = "uncertain"
			previous.Runtime = request.Runtime
			return *previous, nil
		case "prepared": // This version always journals submitting before execute.
		default:
			return none, errAttention
		}
	}
	// Full hydration is bounded by the owned frame limit and item bound. Never
	// substitute metadata-only/partial history or page truncation for this proof.
	// Check the queue before history so an already queued turn cannot drain
	// between the history snapshot and the empty-queue check unnoticed. The
	// required ingress barrier prevents new external queue/input writes.
	if !h.continuationQueueEmpty(ctx, request.Thread) || !h.continuationHistoryReady(ctx, request) {
		return none, errAttention
	}
	// Caller still holds the exclusive input barrier; owner fences every RPC.
	outcome := attentionContinuationOutcome{Protocol: attentionContinuationProtocol, Runtime: request.Runtime,
		SubmissionBinding: request.Runtime, Thread: request.Thread, Delivery: request.Delivery,
		Digest: request.Digest, OriginalTurnID: request.OriginalTurnID}
	if previous == nil {
		if err := h.appendContinuation(ctx, key, &outcome, "prepared"); err != nil {
			return none, err
		}
	}
	if err := h.appendContinuation(ctx, key, &outcome, "submitting"); err != nil {
		return none, err
	}
	params, _ := json.Marshal(map[string]any{"threadId": request.Thread, "input": []any{}})
	work, err := newWorkRequest(ctx, "turn/start", params, h.router.maxFrame)
	if err != nil {
		return none, errAttention
	}
	result := h.router.execute(ctx, work)
	phase := "uncertain"
	if result.err == nil && len(result.response.RemoteError) == 0 && result.response.Ordinal != 0 &&
		len(result.response.Result) <= h.router.maxFrame && checkJSON(result.response.Result) == nil {
		var response struct {
			Turn struct {
				ID        string             `json:"id"`
				Status    string             `json:"status"`
				ItemsView string             `json:"itemsView"`
				Items     *[]json.RawMessage `json:"items"`
			} `json:"turn"`
		}
		if json.Unmarshal(result.response.Result, &response) == nil && continuationTurnID(response.Turn.ID) &&
			response.Turn.ID != request.OriginalTurnID && response.Turn.Status == "inProgress" &&
			response.Turn.ItemsView == "notLoaded" && response.Turn.Items != nil && len(*response.Turn.Items) == 0 {
			phase = "result"
			outcome.NewTurnID = response.Turn.ID
			outcome.Ordinal = result.response.Ordinal
		}
	}
	if err := h.appendContinuation(ctx, key, &outcome, phase); err != nil {
		return none, err
	}
	return outcome, nil
}

func continuationTurnID(id string) bool { return strings.TrimSpace(id) != "" && len(id) <= 512 }

func (h *gateway) appendContinuation(ctx context.Context, key string, outcome *attentionContinuationOutcome, phase string) error {
	outcome.Status = phase
	if phase == "result" {
		outcome.Status = "started"
	}
	payload, _ := json.Marshal(outcome)
	connection, revision := h.g.identity()
	durability, err := h.store.Append(ctx, coveragejournal.Stage{Epoch: connection.epoch, Connection: connection.nonce,
		Generation: connection.generation, Revision: revision, Operation: key + "." + phase, Payload: payload})
	if err != nil || durability != coveragejournal.Durable {
		h.cancelCause(errAttention)
		return errAttention
	}
	return nil
}

func (h *gateway) continuationQueueEmpty(ctx context.Context, thread string) bool {
	raw, _, ok := h.attentionRecoveryRead(ctx, "thread/queue/list", map[string]any{"threadId": thread, "limit": 1})
	if !ok {
		return false
	}
	var response struct {
		Data *[]json.RawMessage `json:"data"`
		Next json.RawMessage    `json:"nextCursor"`
	}
	// Empty first page with a terminal cursor proves this queue empty; anything
	// queued or paginated is refused, rather than drained/deleted by this primitive.
	return json.Unmarshal(raw, &response) == nil && response.Data != nil && len(*response.Data) == 0 && string(response.Next) == "null"
}

func (h *gateway) continuationHistoryReady(ctx context.Context, request attentionContinuationRequest) bool {
	raw, _, ok := h.attentionRecoveryRead(ctx, "thread/read", map[string]any{"threadId": request.Thread, "includeTurns": true})
	if !ok {
		return false
	}
	var response struct {
		Thread struct {
			ID     string `json:"id"`
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
			Turns *[]struct {
				ID        string `json:"id"`
				Status    string `json:"status"`
				ItemsView string `json:"itemsView"`
				Items     *[]struct {
					Type     string          `json:"type"`
					ClientID string          `json:"clientId"`
					Content  json.RawMessage `json:"content"`
				} `json:"items"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Thread.ID != request.Thread || response.Thread.Status.Type != "idle" ||
		response.Thread.Turns == nil || len(*response.Thread.Turns) == 0 || len(*response.Thread.Turns) > attentionRecoveryItems {
		return false
	}
	count, matches, targetUsers := 0, 0, 0
	seen := map[string]bool{}
	turns := *response.Thread.Turns
	for i, turn := range turns {
		if !continuationTurnID(turn.ID) || seen[turn.ID] || turn.ItemsView != "full" || turn.Items == nil {
			return false
		}
		seen[turn.ID] = true
		count += len(*turn.Items)
		if count > attentionRecoveryItems {
			return false
		}
		target := turn.ID == request.OriginalTurnID
		if target && (i != len(turns)-1 || turn.Status != "interrupted") {
			return false
		}
		for _, item := range *turn.Items {
			if item.Type != "userMessage" {
				continue
			}
			if target {
				targetUsers++
			}
			if item.ClientID == request.Delivery {
				if !target || !exactAttentionInput(item.Content, request.Text) {
					return false
				}
				matches++
			}
		}
	}
	return matches == 1 && targetUsers == 1
}
