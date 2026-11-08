package coveragecontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/meshcrawler/hrx/internal/codexrpc"
)

// continuationRoute is separate from admit: it takes the exclusive ingress
// lease before attentionMu, never upgrades a shared admission lease. Only the
// installed host verifier can authorize unfinished work; no request boolean can.
func (h *gateway) continuationRoute(ctx context.Context, source workSource, id, raw json.RawMessage) ([]byte, error) {
	if source == terminalSource || !h.combinedResumeDrain || h.verifyPending == nil || checkJSON(raw) != nil {
		return nil, errAttention
	}
	var fields map[string]json.RawMessage
	var request attentionRequest
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 6 || json.Unmarshal(raw, &request) != nil ||
		request.Protocol != attentionProtocol || request.Runtime != h.attentionBinding() || request.Thread != h.b.native.ThreadID ||
		!attentionID.MatchString(request.Delivery) || request.Attempt == 0 || request.Attempt > 1<<63-1 || len(request.Text) == 0 || len(request.Text) > 16384 {
		return nil, errAttention
	}
	ctx, cancel := context.WithTimeout(ctx, h.stageBudget)
	defer cancel()
	release, err := h.exclusiveInput(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err = h.verifyOwnedPending(ctx, OwnedPendingRequest{RuntimeBinding: request.Runtime, ThreadID: request.Thread, DeliveryID: request.Delivery}); err != nil {
		return nil, err
	}
	history, ok := h.store.(attentionHistory)
	if !ok {
		return nil, errAttention
	}
	digest := sha256.Sum256([]byte(request.Text))
	hash := hex.EncodeToString(digest[:])
	var latest *attentionOutcome
	var savedContinuation *attentionContinuationOutcome
	for _, stage := range history.History() {
		var admission attentionOutcome
		if json.Unmarshal(stage.Payload, &admission) == nil && admission.Protocol == attentionProtocol && admission.Delivery == request.Delivery {
			copy := admission
			latest = &copy
		}
		var continuation attentionContinuationOutcome
		if json.Unmarshal(stage.Payload, &continuation) == nil && continuation.Protocol == attentionContinuationProtocol && continuation.Delivery == request.Delivery {
			copy := continuation
			savedContinuation = &copy
		}
	}
	if latest == nil || latest.Thread != request.Thread || latest.Digest != hash || latest.Attempt != request.Attempt ||
		latest.Status != "admitted" || latest.AdmissionBinding == "" || latest.AdmissionBinding == request.Runtime {
		return nil, errAttention
	}
	original := ""
	if savedContinuation != nil {
		if savedContinuation.Thread != request.Thread || savedContinuation.Digest != hash {
			return nil, errAttention
		}
		original = savedContinuation.OriginalTurnID
	} else {
		var status string
		original, status, err = h.continuationOriginal(ctx, request)
		if err != nil {
			return nil, err
		}
		if status == "completed" || status == "inProgress" {
			result := attentionContinuationOutcome{Protocol: attentionContinuationProtocol, Runtime: request.Runtime,
				Thread: request.Thread, Delivery: request.Delivery, Digest: hash, OriginalTurnID: original, Status: "not_required"}
			payload, _ := json.Marshal(result)
			return gatewayReply(id, codexrpc.Response{Result: payload})
		}
		if status != "interrupted" {
			return nil, errAttention
		}
	}
	result, err := h.continueAttention(ctx, attentionContinuationRequest{Runtime: request.Runtime, Thread: request.Thread,
		Delivery: request.Delivery, Digest: hash, OriginalTurnID: original, Text: request.Text}, attentionContinuationAuthorization{
		Runtime: request.Runtime, Delivery: request.Delivery, Digest: hash, OriginalTurnID: original,
		ServicePendingUnfinished: true, ExclusiveInputHeld: true})
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(result)
	return gatewayReply(id, codexrpc.Response{Result: payload})
}

// Discover only the exact journal-bound ORIGINAL user turn. Full hydration and
// last-turn checks fail closed; no arbitrary client-selected continuation turn.
func (h *gateway) continuationOriginal(ctx context.Context, request attentionRequest) (string, string, error) {
	raw, _, ok := h.attentionRecoveryRead(ctx, "thread/read", map[string]any{"threadId": request.Thread, "includeTurns": true})
	if !ok {
		return "", "", errAttention
	}
	var response struct {
		Thread struct {
			ID    string `json:"id"`
			Turns *[]struct {
				ID, Status string
				ItemsView  string `json:"itemsView"`
				Items      *[]struct {
					Type     string          `json:"type"`
					ClientID string          `json:"clientId"`
					Content  json.RawMessage `json:"content"`
				} `json:"items"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Thread.ID != request.Thread || response.Thread.Turns == nil || len(*response.Thread.Turns) > attentionRecoveryItems {
		return "", "", errAttention
	}
	matches, count := 0, 0
	original, status := "", ""
	seen := map[string]bool{}
	turns := *response.Thread.Turns
	for index, turn := range turns {
		if !continuationTurnID(turn.ID) || seen[turn.ID] || turn.ItemsView != "full" || turn.Items == nil {
			return "", "", errAttention
		}
		seen[turn.ID] = true
		count += len(*turn.Items)
		if count > attentionRecoveryItems {
			return "", "", errAttention
		}
		users := 0
		target := false
		for _, item := range *turn.Items {
			if item.Type != "userMessage" {
				continue
			}
			users++
			if item.ClientID == request.Delivery {
				if !exactAttentionInput(item.Content, request.Text) || index != len(turns)-1 {
					return "", "", errAttention
				}
				matches++
				target = true
				original = turn.ID
				status = turn.Status
			}
		}
		if target && users != 1 {
			return "", "", errAttention
		}
	}
	if matches != 1 {
		return "", "", errAttention
	}
	return original, status, nil
}
