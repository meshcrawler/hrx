package coveragecontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/meshcrawler/hrx/internal/codexrpc"
	"github.com/meshcrawler/hrx/internal/coveragejournal"
)

const attentionProtocol = "hrx-owned-attention.v1"

var attentionID = regexp.MustCompile(`^delivery-[0-9a-f]{64}$`)
var errAttention = errors.New("owned attention operation unavailable")

// AttentionTestPoint is an opt-in, caller-owned acceptance seam. No process or
// environment setting enables it. Callbacks must stop blocking when ctx ends.
// Prepared is durable and precedes the durable submitting marker and native call.
// QueueResponse follows a successful, owner-consumed native response and precedes
// the admitted journal commit. Errors leave the last durable evidence untouched.
type AttentionTestPoint struct {
	Phase, ThreadID, DeliveryID, PayloadSHA256 string
	Attempt, ResponseOrdinal                   uint64
}

const (
	AttentionPrepared      = "prepared"
	AttentionQueueResponse = "queue_response"
)

type attentionRequest struct {
	Protocol string `json:"protocol"`
	Runtime  string `json:"runtime_binding"`
	Thread   string `json:"thread_id"`
	Delivery string `json:"delivery_id"`
	Text     string `json:"text"`
	Attempt  uint64 `json:"attempt"`
}
type attentionOutcome struct {
	Protocol         string `json:"protocol"`
	Runtime          string `json:"runtime_binding"`
	AdmissionBinding string `json:"admission_binding"`
	Thread           string `json:"thread_id"`
	Delivery         string `json:"delivery_id"`
	Digest           string `json:"payload_sha256"`
	Status           string `json:"status"`
	Attempt          uint64 `json:"attempt"`
	Ordinal          uint64 `json:"response_ordinal"`
	// Recovery fields identify a current owned read witness. AdmissionBinding
	// remains the original submission context; Ordinal stays zero when its lost
	// queue/add response is unknown. Normal outcomes omit all recovery fields.
	RecoverySource  string `json:"recovery_source,omitempty"`
	RecoveryBinding string `json:"recovery_binding,omitempty"`
	RecoveryOrdinal uint64 `json:"recovery_ordinal,omitempty"`
}
type attentionHistory interface {
	History() []coveragejournal.Stage
}

// attentionBinding names this exact owned connection/session. Historical stages
// never establish live authority: handle has already required the current owner.
func (h *gateway) attentionBinding() string {
	id, _ := h.g.identity()
	raw, _ := json.Marshal([]string{id.epoch, id.nonce, id.generation,
		h.b.native.Identity.ThreadID, h.b.native.Identity.SessionID})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (r *Runtime) AttentionBinding() string { return r.gateway.attentionBinding() }

// attention serializes only bounded auxiliary admission operations. The native
// receive/reducer owner remains active while the journal's own serialized writer
// commits historical work evidence; no handler reads native frames or publishes
// a coverage frontier from an admission receipt.
func (h *gateway) attention(ctx context.Context, source workSource, id json.RawMessage, raw json.RawMessage) ([]byte, error) {
	return h.attentionOperation(ctx, source, id, raw, false)
}

func (h *gateway) attentionOperation(ctx context.Context, source workSource, id json.RawMessage, raw json.RawMessage, observeOnly bool) ([]byte, error) {
	if source == terminalSource {
		return nil, errAttention
	}
	ctx, cancel := context.WithTimeout(ctx, h.stageBudget)
	defer cancel()
	release, err := h.input.acquire(ctx, false)
	if err != nil {
		return nil, err
	}
	defer release()
	if !h.attentionMu.TryLock() {
		return nil, errAttention
	}
	defer h.attentionMu.Unlock()
	if checkJSON(raw) != nil {
		return nil, errAttention
	}
	var fields map[string]json.RawMessage
	var request attentionRequest
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 6 || json.Unmarshal(raw, &request) != nil ||
		request.Protocol != attentionProtocol || request.Runtime != h.attentionBinding() ||
		request.Thread != h.b.native.Identity.ThreadID || !attentionID.MatchString(request.Delivery) ||
		request.Attempt == 0 || request.Attempt > uint64(1<<63-1) || len(request.Text) == 0 || len(request.Text) > 16384 {
		return nil, errAttention
	}
	history, ok := h.store.(attentionHistory)
	if !ok {
		return nil, errAttention
	}
	sum := sha256.Sum256([]byte(request.Text))
	digest := hex.EncodeToString(sum[:])
	attempt := request.Attempt
	var previous *attentionOutcome
	preparedNoWrite := false
	for _, stage := range history.History() {
		var outcome attentionOutcome
		if json.Unmarshal(stage.Payload, &outcome) != nil || outcome.Protocol != attentionProtocol ||
			outcome.Delivery != request.Delivery {
			continue
		}
		if outcome.Thread != request.Thread || outcome.Digest != digest {
			return nil, errAttention
		}
		saved := outcome
		previous = &saved
		preparedNoWrite = strings.HasSuffix(stage.Operation, ".prepared_no_write_v1")
	}
	reply := func(outcome attentionOutcome) ([]byte, error) {
		outcome.Runtime = request.Runtime
		result, _ := json.Marshal(outcome)
		return gatewayReply(id, codexrpc.Response{Result: result})
	}
	if previous != nil {
		if request.Attempt == previous.Attempt {
			switch previous.Status {
			case "prepared", "submitting", "uncertain":
				if previous.Status == "prepared" && preparedNoWrite && !observeOnly {
					// Every v1 submitting marker is durable BEFORE the router can write.
					// This proof is unavailable for legacy prepared records.
					if err := h.appendAttentionOutcome(ctx, previous, "not_admitted"); err != nil {
						return nil, err
					}
				} else {
					witness, found := h.reconcileAttention(ctx, request)
					if found {
						// Never invent an ordinal for the lost queue/add response.
						previous.Ordinal = 0
						previous.RecoverySource = witness.source
						previous.RecoveryBinding = h.attentionBinding()
						previous.RecoveryOrdinal = witness.ordinal
						if err := h.appendAttentionOutcome(ctx, previous, "recovered_admitted"); err != nil {
							return nil, err
						}
					} else {
						previous.Status = "uncertain"
					}
				}
			}
			return reply(*previous)
		}
		if previous.Status != "not_admitted" || request.Attempt != previous.Attempt+1 {
			return nil, errAttention
		}
	} else if request.Attempt != 1 {
		return nil, errAttention
	}

	if observeOnly {
		return nil, errAttention // Observation never creates another native operation.
	}

	if attempt > uint64(1<<63-1) {
		return nil, errAttention
	}
	outcome := attentionOutcome{Protocol: attentionProtocol, Runtime: request.Runtime,
		AdmissionBinding: request.Runtime, Thread: request.Thread, Delivery: request.Delivery,
		Digest: digest, Status: "prepared", Attempt: attempt}

	if err := h.appendAttentionOutcome(ctx, &outcome, "prepared"); err != nil {
		return nil, err
	}
	if err := h.attentionTestPoint(ctx, AttentionPrepared, outcome); err != nil {
		return nil, err
	}
	// A crash after this marker is ambiguous even if the request was not written.
	if err := h.appendAttentionOutcome(ctx, &outcome, "submitting"); err != nil {
		return nil, err
	}
	params, _ := json.Marshal(map[string]any{"threadId": request.Thread,
		"clientUserMessageId": request.Delivery,
		"input":               []map[string]string{{"type": "text", "text": request.Text}}})
	work, validationErr := newWorkRequest(ctx, "thread/queue/add", params, h.router.maxFrame)
	result := workResult{err: validationErr}
	if validationErr == nil {
		result = h.router.execute(ctx, work)
	}
	status := "uncertain"
	if result.err == nil && len(result.response.RemoteError) == 0 && result.response.Ordinal != 0 {
		if err := h.attentionTestPoint(ctx, AttentionQueueResponse, attentionOutcome{
			Thread: outcome.Thread, Delivery: outcome.Delivery, Digest: outcome.Digest,
			Attempt: outcome.Attempt, Ordinal: result.response.Ordinal,
		}); err != nil {
			return nil, err
		}
		status = "admitted"
	} else if result.response.Ordinal == 0 && (errors.Is(result.err, errWorkNotReady) ||
		errors.Is(result.err, codexrpc.ErrRequest) || errors.Is(result.err, codexrpc.ErrPublicationRefused)) {
		status = "not_admitted"
	}
	outcome.Ordinal = result.response.Ordinal
	if err := h.appendAttentionOutcome(ctx, &outcome, status); err != nil {
		return nil, err
	}
	return reply(outcome)
}

func (h *gateway) attentionTestPoint(ctx context.Context, phase string, outcome attentionOutcome) error {
	if h.attentionTestHook == nil {
		return ctx.Err()
	}
	if err := h.attentionTestHook(ctx, AttentionTestPoint{Phase: phase, ThreadID: outcome.Thread,
		DeliveryID: outcome.Delivery, PayloadSHA256: outcome.Digest, Attempt: outcome.Attempt,
		ResponseOrdinal: outcome.Ordinal}); err != nil {
		return err
	}
	return ctx.Err()
}

func (h *gateway) appendAttentionOutcome(ctx context.Context, outcome *attentionOutcome, phase string) error {
	outcome.Status = phase
	if phase == "recovered_admitted" {
		outcome.Status = "admitted"
	}
	payload, _ := json.Marshal(outcome)
	connection, revision := h.g.identity()
	marker := phase
	if phase == "prepared" {
		marker = "prepared_no_write_v1"
	}
	stage := coveragejournal.Stage{Epoch: connection.epoch, Connection: connection.nonce,
		Generation: connection.generation, Revision: revision,
		Operation: "attention." + outcome.Delivery[9:] + "." + strconv.FormatUint(outcome.Attempt, 10) + "." + marker,
		Payload:   payload}
	durability, err := h.store.Append(ctx, stage)
	if err != nil || durability != coveragejournal.Durable {
		h.cancelCause(errAttention)
		return errAttention
	}
	return nil
}
