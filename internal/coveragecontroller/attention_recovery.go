package coveragecontroller

import (
	"context"
	"encoding/json"
)

// Pinned source: openai/codex rust-v0.163.0, commit
// 83086188ae0614bd882818ff1aa23f3194962012:
// app-server-protocol/src/protocol/v2/thread.rs:926-967,1705-1712;
// app-server-protocol/src/protocol/v2/item.rs:239-242;
// ext/queue/src/service.rs:265-279,439-448; state/src/runtime/queued_items.rs:77-104.
// queue/add is NOT idempotent by client ID (each insertion gets a new UUID).
// Exact queue/history matches are positive admission witnesses only. No match,
// incomplete history, moving/over-bound pagination or RPC failure is NOT a proof
// of nonadmission. In particular cold shutdown does not expose an atomic queue /
// history checkpoint or idempotency receipt; never resend an ambiguous request.
const attentionRecoveryPages = 32
const attentionRecoveryItems = 4096

func (h *gateway) attentionRecoveryRead(ctx context.Context, method string, params any) (json.RawMessage, uint64, bool) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, 0, false
	}
	work, err := newWorkRequest(ctx, method, raw, h.router.maxFrame)
	if err != nil {
		return nil, 0, false
	}
	// Same router, sole receiver, consumed-prefix fence and bound as live work.
	result := h.router.execute(ctx, work)
	if result.err != nil || len(result.response.RemoteError) != 0 || result.response.Ordinal == 0 ||
		len(result.response.Result) > h.router.maxFrame || checkJSON(result.response.Result) != nil {
		return nil, 0, false
	}
	return result.response.Result, result.response.Ordinal, true
}

func exactAttentionInput(raw json.RawMessage, text string) bool {
	var input []map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil || len(input) != 1 {
		return false
	}
	var kind, value string
	if json.Unmarshal(input[0]["type"], &kind) != nil || kind != "text" ||
		json.Unmarshal(input[0]["text"], &value) != nil || value != text {
		return false
	}
	// Pinned v2/turn.rs:439-448 (and generated v2/UserInput.ts) serialize
	// Text.text_elements with snake_case; enum rename_all renames variants only.
	// Retain the previously accepted camelCase spelling without ambiguous aliases.
	if input[0]["text_elements"] != nil && input[0]["textElements"] != nil {
		return false
	}
	for field, value := range input[0] {
		switch field {
		case "type", "text":
		case "text_elements", "textElements":
			var elements []json.RawMessage
			if json.Unmarshal(value, &elements) != nil || elements == nil || len(elements) != 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

type attentionRecoveryWitness struct {
	source  string
	ordinal uint64
}

func (h *gateway) reconcileAttention(ctx context.Context, request attentionRequest) (attentionRecoveryWitness, bool) {
	cursor := ""
	seen := map[string]bool{}
	matches, count := 0, 0
	var witness uint64
	for page := 0; page < attentionRecoveryPages; page++ {
		params := map[string]any{"threadId": request.Thread, "limit": 64}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, ordinal, ok := h.attentionRecoveryRead(ctx, "thread/queue/list", params)
		if !ok {
			return attentionRecoveryWitness{}, false
		}
		var response struct {
			Data *[]struct {
				ID       string          `json:"id"`
				ClientID string          `json:"clientUserMessageId"`
				Input    json.RawMessage `json:"input"`
			} `json:"data"`
			Next json.RawMessage `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &response) != nil || response.Data == nil || len(response.Next) == 0 {
			return attentionRecoveryWitness{}, false
		}
		count += len(*response.Data)
		if count > attentionRecoveryItems {
			return attentionRecoveryWitness{}, false
		}
		for _, item := range *response.Data {
			if item.ID == "" || len(item.ID) > 512 || seen["item:"+item.ID] {
				return attentionRecoveryWitness{}, false
			}
			seen["item:"+item.ID] = true
			if item.ClientID == request.Delivery {
				if !exactAttentionInput(item.Input, request.Text) {
					return attentionRecoveryWitness{}, false
				}
				matches++
				witness = ordinal
			}
		}
		if string(response.Next) == "null" {
			if matches == 1 {
				return attentionRecoveryWitness{source: "queue", ordinal: witness}, true
			}
			if matches > 1 {
				return attentionRecoveryWitness{}, false
			}
			return h.reconcileAttentionHistory(ctx, request)
		}
		var next string
		if json.Unmarshal(response.Next, &next) != nil || next == "" || len(next) > 512 || seen["cursor:"+next] {
			return attentionRecoveryWitness{}, false
		}
		seen["cursor:"+next] = true
		cursor = next
	}
	return attentionRecoveryWitness{}, false
}

func (h *gateway) reconcileAttentionHistory(ctx context.Context, request attentionRequest) (attentionRecoveryWitness, bool) {
	// includeTurns=true still hydrates full legacy/paginated history in the pin.
	// An over-frame response or non-full itemsView fails closed; never treat a
	// truncated/default metadata read as absence. No resume or alternate owner.
	raw, ordinal, ok := h.attentionRecoveryRead(ctx, "thread/read", map[string]any{"threadId": request.Thread, "includeTurns": true})
	if !ok {
		return attentionRecoveryWitness{}, false
	}
	var response struct {
		Thread struct {
			ID    string `json:"id"`
			Turns *[]struct {
				ItemsView string `json:"itemsView"`
				Items     *[]struct {
					Type     string          `json:"type"`
					ClientID string          `json:"clientId"`
					Content  json.RawMessage `json:"content"`
				} `json:"items"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Thread.ID != request.Thread || response.Thread.Turns == nil || len(*response.Thread.Turns) > attentionRecoveryItems {
		return attentionRecoveryWitness{}, false
	}
	matches, count := 0, 0
	for _, turn := range *response.Thread.Turns {
		if turn.ItemsView != "full" || turn.Items == nil {
			return attentionRecoveryWitness{}, false
		}
		count += len(*turn.Items)
		if count > attentionRecoveryItems {
			return attentionRecoveryWitness{}, false
		}
		for _, item := range *turn.Items {
			if item.Type == "userMessage" && item.ClientID == request.Delivery {
				if !exactAttentionInput(item.Content, request.Text) {
					return attentionRecoveryWitness{}, false
				}
				matches++
			}
		}
	}
	return attentionRecoveryWitness{source: "history", ordinal: ordinal}, matches == 1
}
