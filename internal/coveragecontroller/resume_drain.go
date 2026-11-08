package coveragecontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The pinned queue extension starts durable queued input on resume's completed
// idle notification (ext/queue/src/service.rs:405-449,538-565). Started removes
// the queue entry; interrupting or resending it would not recover that work.
// This evidence assembler runs only inside the exclusive bootstrap owner. It
// never admits input, acknowledges a source or manufactures handling evidence.
type resumeDrain struct {
	thread                  string
	eligible                map[string]string // latest journal outcome: delivery -> text SHA256
	expected                map[string]bool   // exact durable queue snapshot before resume
	turns                   map[string]bool   // observed turn -> terminal notification observed
	items                   map[string]string // item identity -> turn identity
	finished                map[string]bool
	preexisting             map[string]resumeStoppedTurn
	allowInterruptedHistory bool // host continuation verifier installed; live queued work still drains
	armed                   bool
	version                 uint64
}

type resumeStoppedTurn struct {
	status string
	input  [32]byte
}

type resumeHistoryItem struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	ClientID *string         `json:"clientId"`
	Content  json.RawMessage `json:"content"`
}

type resumeHistoryTurn struct {
	ID, Status string
	ItemsView  string               `json:"itemsView"`
	Items      *[]resumeHistoryItem `json:"items"`
}

func resumeHistoryTurns(raw json.RawMessage, thread string) ([]resumeHistoryTurn, string, error) {
	var result struct {
		Thread struct {
			ID     string                `json:"id"`
			Status struct{ Type string } `json:"status"`
			Turns  *[]resumeHistoryTurn  `json:"turns"`
		} `json:"thread"`
	}
	if checkJSON(raw) != nil || json.Unmarshal(raw, &result) != nil || result.Thread.ID != thread || result.Thread.Turns == nil || len(*result.Thread.Turns) > attentionRecoveryItems {
		return nil, "", fmt.Errorf("%w: invalid resume history", errBootstrap)
	}
	seen, items := map[string]bool{}, 0
	for _, turn := range *result.Thread.Turns {
		if turn.ID == "" || len(turn.ID) > 512 || seen[turn.ID] {
			return nil, "", fmt.Errorf("%w: duplicate resume history turn", errBootstrap)
		}
		seen[turn.ID] = true
		if turn.Items != nil {
			items += len(*turn.Items)
		}
		if items > attentionRecoveryItems {
			return nil, "", fmt.Errorf("%w: resume history item limit", errBootstrap)
		}
	}
	return *result.Thread.Turns, result.Thread.Status.Type, nil
}

// Fingerprint only historical user inputs, never their later model output.
// Exact item ID, optional client identity, content and input order are retained;
// JSON field ordering/whitespace is not part of native user input identity.
func (turn resumeHistoryTurn) inputFingerprint() ([32]byte, error) {
	if turn.Items == nil || turn.ItemsView != "full" {
		return [32]byte{}, fmt.Errorf("%w: incomplete preexisting resume history", errBootstrap)
	}
	users := []any{}
	seen := map[string]bool{}
	for _, item := range *turn.Items {
		if item.ID == "" || len(item.ID) > 512 || seen[item.ID] || item.Type == "" {
			return [32]byte{}, fmt.Errorf("%w: invalid preexisting resume item identity", errBootstrap)
		}
		seen[item.ID] = true
		if item.Type != "userMessage" {
			continue
		}
		var content []json.RawMessage
		if json.Unmarshal(item.Content, &content) != nil || content == nil {
			return [32]byte{}, fmt.Errorf("%w: invalid preexisting resume user input", errBootstrap)
		}
		// Canonicalize nested object order while retaining numbers verbatim.
		var normalized any
		decoder := json.NewDecoder(strings.NewReader(string(item.Content)))
		decoder.UseNumber()
		if decoder.Decode(&normalized) != nil {
			return [32]byte{}, errBootstrap
		}
		users = append(users, []any{item.ID, item.ClientID, normalized})
	}
	raw, _ := json.Marshal(users)
	return sha256.Sum256(raw), nil
}

// Pinned thread_processor.rs:2960-3000,3050-3084 supports full thread/read
// before loading/resuming a stored thread. thread_lifecycle.rs:1074-1090 marks
// old InProgress turns interrupted only while the WHOLE thread is not active.
// Their persisted status may reappear as InProgress during queued work. Bind
// this distinction to a pre-resume stopped snapshot, never a status heuristic.
func (d *resumeDrain) captureStoppedHistory(raw json.RawMessage) error {
	turns, status, err := resumeHistoryTurns(raw, d.thread)
	if err != nil {
		return err
	}
	if d.armed || d.preexisting != nil || status != "notLoaded" && status != "idle" {
		return fmt.Errorf("%w: pre-resume history is not stopped", errBootstrap)
	}
	stopped := map[string]resumeStoppedTurn{}
	for _, turn := range turns {
		if turn.Status != "completed" && turn.Status != "interrupted" && turn.Status != "failed" {
			return fmt.Errorf("%w: unresolved pre-resume history", errBootstrap)
		}
		fingerprint, err := turn.inputFingerprint()
		if err != nil {
			return err
		}
		stopped[turn.ID] = resumeStoppedTurn{status: turn.Status, input: fingerprint}
	}
	d.preexisting = stopped
	return nil
}

func newResumeDrain(store stageStore, thread string) (*resumeDrain, error) {
	history, ok := store.(attentionHistory)
	if !ok || thread == "" {
		return nil, fmt.Errorf("%w: resume admission history unavailable", errBootstrap)
	}
	latest := map[string]attentionOutcome{}
	for _, stage := range history.History() {
		var outcome attentionOutcome
		if json.Unmarshal(stage.Payload, &outcome) != nil || outcome.Protocol != attentionProtocol {
			continue
		}
		// Latest means latest overall for a delivery, not latest eligible record.
		// A later negative/prepared record must supersede an older admission.
		latest[outcome.Delivery] = outcome
	}
	d := &resumeDrain{thread: thread, eligible: map[string]string{}, expected: map[string]bool{}, turns: map[string]bool{}, items: map[string]string{}, finished: map[string]bool{}}
	for delivery, outcome := range latest {
		if outcome.Thread != thread || !attentionID.MatchString(delivery) || outcome.Attempt == 0 || outcome.Attempt > 1<<63-1 {
			continue
		}
		if outcome.Status != "submitting" && outcome.Status != "uncertain" && outcome.Status != "admitted" {
			continue
		}
		digest, err := hex.DecodeString(outcome.Digest)
		if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != outcome.Digest {
			return nil, fmt.Errorf("%w: invalid resume admission digest", errBootstrap)
		}
		d.eligible[delivery] = outcome.Digest
	}
	return d, nil
}

func (d *resumeDrain) exactInput(delivery string, raw json.RawMessage) bool {
	digest, ok := d.eligible[delivery]
	if !ok {
		return false
	}
	var input []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &input) != nil || len(input) != 1 || !exactAttentionInput(raw, input[0].Text) {
		return false
	}
	sum := sha256.Sum256([]byte(input[0].Text))
	return hex.EncodeToString(sum[:]) == digest
}

func (d *resumeDrain) turnInputMismatch(clientID string, raw json.RawMessage, duplicate bool) string {
	if clientID == "" {
		return "missing_client_identity"
	}
	if _, eligible := d.eligible[clientID]; !eligible {
		return "unknown_client_identity"
	}
	if !d.expected[clientID] {
		return "not_in_prevalidated_queue"
	}
	if duplicate {
		return "duplicate_client_identity"
	}
	var input []struct{ Type, Text string }
	if json.Unmarshal(raw, &input) != nil {
		return "input_shape"
	}
	if len(input) != 1 {
		return "input_count"
	}
	if input[0].Type != "text" {
		return "input_type"
	}
	if !exactAttentionInput(raw, input[0].Text) {
		return "input_shape"
	}
	return "payload_digest"
}

func (d *resumeDrain) notification(method string, raw json.RawMessage) error {
	var activity struct {
		Thread string `json:"threadId"`
		TurnID string `json:"turnId"`
		ItemID string `json:"itemId"`
		Item   struct {
			ID, Type string
		} `json:"item"`
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"turn"`
	}
	if !d.armed || json.Unmarshal(raw, &activity) != nil || activity.Thread != d.thread {
		return fmt.Errorf("%w: unbound resume activity", errBootstrap)
	}
	// Pinned protocol/common.rs:1980-2028. The scripted native recovery
	// contract needs ordinary text/reasoning/command lifecycles only. Unknown
	// activity, compaction and interactive/extension workflows fail closed.
	switch method {
	case "turn/started", "turn/completed", "turn/diff/updated", "turn/plan/updated":
	case "item/started", "item/completed":
		if activity.Item.ID == "" || len(activity.Item.ID) > 512 {
			return fmt.Errorf("%w: invalid resume item identity", errBootstrap)
		}
		switch activity.Item.Type {
		case "userMessage", "agentMessage", "reasoning", "commandExecution", "plan":
		default:
			return fmt.Errorf("%w: unsupported resume item lifecycle", errBootstrap)
		}
		activity.ItemID = activity.Item.ID
	case "item/agentMessage/delta", "item/plan/delta", "item/commandExecution/outputDelta",
		"item/reasoning/summaryTextDelta", "item/reasoning/summaryPartAdded", "item/reasoning/textDelta":
		if activity.ItemID == "" || len(activity.ItemID) > 512 {
			return fmt.Errorf("%w: invalid resume delta identity", errBootstrap)
		}
	default:
		return fmt.Errorf("%w: unsupported resume activity", errBootstrap)
	}
	id := activity.TurnID
	if method == "turn/started" || method == "turn/completed" {
		id = activity.Turn.ID
	}
	if id == "" || len(id) > 512 || len(d.turns) >= attentionRecoveryItems && !hasTurn(d.turns, id) {
		return fmt.Errorf("%w: unbounded resume activity", errBootstrap)
	}
	if method == "turn/started" && hasTurn(d.turns, id) {
		return fmt.Errorf("%w: duplicate resume turn start", errBootstrap)
	}
	if d.turns[id] {
		return fmt.Errorf("%w: activity after resume terminal", errBootstrap)
	}
	if activity.ItemID != "" {
		owner, exists := d.items[activity.ItemID]
		if exists && owner != id || !exists && len(d.items) >= attentionRecoveryItems || d.finished[activity.ItemID] || exists && method == "item/started" {
			return fmt.Errorf("%w: duplicate or unbound resume item", errBootstrap)
		}
		d.items[activity.ItemID] = id
		if method == "item/completed" {
			d.finished[activity.ItemID] = true
		}
	}
	if method == "turn/completed" {
		if activity.Turn.Status != "completed" && activity.Turn.Status != "interrupted" && activity.Turn.Status != "failed" {
			return fmt.Errorf("%w: invalid resume terminal status", errBootstrap)
		}
		d.turns[id] = true
	} else if !hasTurn(d.turns, id) {
		d.turns[id] = false
	}
	d.version++
	return nil
}

func hasTurn(turns map[string]bool, id string) bool { _, ok := turns[id]; return ok }

// queueSnapshot validates every entry, including entries unrelated to the
// pending Python receipt. A partial/moving/duplicate page never proves empty.
func (h *gateway) resumeQueue(ctx context.Context, snapshot bool) (bool, error) {
	d := h.b.drain
	seen, deliveries := map[string]bool{}, map[string]bool{}
	cursor := ""
	for page := 0; page < attentionRecoveryPages; page++ {
		params := map[string]any{"threadId": d.thread, "limit": 64}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, _ := json.Marshal(params)
		response, err := h.bootstrapCall(ctx, "thread/queue/list", raw)
		if err != nil {
			return false, err
		}
		var result struct {
			Data *[]struct {
				ID       string          `json:"id"`
				ClientID string          `json:"clientUserMessageId"`
				Input    json.RawMessage `json:"input"`
			} `json:"data"`
			Next json.RawMessage `json:"nextCursor"`
		}
		if json.Unmarshal(response.Result, &result) != nil || result.Data == nil || len(result.Next) == 0 {
			return false, fmt.Errorf("%w: invalid resume queue page", errBootstrap)
		}
		for _, item := range *result.Data {
			if item.ID == "" || len(item.ID) > 512 || seen["item:"+item.ID] || deliveries[item.ClientID] || !d.exactInput(item.ClientID, item.Input) {
				return false, fmt.Errorf("%w: unmatched resume queue input", errBootstrap)
			}
			seen["item:"+item.ID], deliveries[item.ClientID] = true, true
			if snapshot {
				d.expected[item.ClientID] = true
			} else if !d.expected[item.ClientID] {
				return false, fmt.Errorf("%w: resume queue changed", errBootstrap)
			}
			if len(deliveries) > attentionRecoveryItems {
				return false, fmt.Errorf("%w: resume queue limit", errBootstrap)
			}
		}
		if string(result.Next) == "null" {
			return len(deliveries) == 0, nil
		}
		var next string
		if json.Unmarshal(result.Next, &next) != nil || next == "" || len(next) > 512 || seen["cursor:"+next] {
			return false, fmt.Errorf("%w: invalid resume queue cursor", errBootstrap)
		}
		seen["cursor:"+next], cursor = true, next
	}
	return false, fmt.Errorf("%w: resume queue page limit", errBootstrap)
}

// history proves the exact user input belongs to the observed turn, not merely
// that the delivery occurred somewhere in this thread's older history.
func (d *resumeDrain) history(raw json.RawMessage) (bool, error) {
	turns, _, err := resumeHistoryTurns(raw, d.thread)
	if err != nil {
		return false, err
	}
	seen, witnessed, completed := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, turn := range turns {
		seen[turn.ID] = true
		old, preexisting := d.preexisting[turn.ID]
		unchanged := false
		if preexisting {
			fingerprint, err := turn.inputFingerprint()
			if err != nil {
				return false, err
			}
			unchanged = old.input == fingerprint
			if !unchanged {
				return false, fmt.Errorf("%w: preexisting resume user input changed", errBootstrap)
			}
		}
		observed := hasTurn(d.turns, turn.ID)
		oldStopped := preexisting && unchanged && old.status == "interrupted" && !observed
		tracked := observed || turn.Status == "inProgress" && !oldStopped
		matches := 0
		if turn.Items != nil && turn.ItemsView == "full" {
			userItems := 0
			for _, item := range *turn.Items {
				if item.Type == "userMessage" {
					userItems++
				}
			}
			for _, item := range *turn.Items {
				if item.Type != "userMessage" {
					continue
				}
				clientID := ""
				if item.ClientID != nil {
					clientID = *item.ClientID
				}
				if _, eligible := d.eligible[clientID]; eligible && (turn.Status == "failed" || turn.Status == "interrupted" && !d.allowInterruptedHistory) {
					return false, fmt.Errorf("%w: interrupted admitted turn requires unsupported native continuation", errBootstrap)
				}
				if tracked || d.expected[clientID] {
					if !d.exactInput(clientID, item.Content) || !d.expected[clientID] || witnessed[clientID] {
						reason := d.turnInputMismatch(clientID, item.Content, witnessed[clientID])
						return false, fmt.Errorf("%w: unmatched resume turn input (reason=%s observed=%t active=%t preexisting=%t unchanged=%t user_items=%d)",
							errBootstrap, reason, observed, turn.Status == "inProgress", preexisting, unchanged, userItems)
					}
					witnessed[clientID] = true
					matches++
					if turn.Status == "completed" {
						completed[clientID] = true
					}
				}
			}
		}
		if tracked {
			if matches == 0 {
				// turn/started deliberately clears items (bespoke event handler
				// :155-184); a bounded later read must materialize the witness.
				return false, nil
			}
			if matches != 1 {
				return false, fmt.Errorf("%w: ambiguous resume turn input witness", errBootstrap)
			}
			if turn.Status != "completed" {
				return false, nil // Never treat interrupted work as eventual handling.
			}
		}
	}
	for turn, terminal := range d.turns {
		if !seen[turn] || !terminal {
			return false, nil
		}
	}
	for turn := range d.preexisting {
		if !seen[turn] {
			return false, fmt.Errorf("%w: preexisting resume turn missing", errBootstrap)
		}
	}
	// Observed lifecycles require both their terminal notification and durable
	// completed history, so a late terminal cannot cross baseline publication.
	// History alone suffices only when the entire lifecycle preceded attachment.
	for delivery := range d.expected {
		if !completed[delivery] {
			return false, nil
		}
	}
	return true, nil
}

func (h *gateway) finishResumeDrain(ctx context.Context) error {
	d := h.b.drain
	read, _ := json.Marshal(map[string]any{"threadId": d.thread, "includeTurns": true})
	for reads := 0; reads < attentionRecoveryItems; reads++ {
		empty, err := h.resumeQueue(ctx, false)
		if err != nil {
			return err
		}
		version := d.version
		response, err := h.bootstrapCall(ctx, "thread/read", read)
		if err != nil {
			return err
		}
		complete, err := d.history(response.Result)
		if err != nil {
			return err
		}
		if empty && complete && version == d.version && h.b.native.ValidateResumeBaseline(response.Result, h.b.name) == nil {
			// bootstrapCall consumed the same sole receiver's complete response
			// prefix. Subsequent frames pass the unchanged publication fence.
			h.b.readHash = sha256.Sum256(response.Result)
			h.b.step++
			h.b.drain = nil
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("%w: resume drain read limit", errBootstrap)
}
