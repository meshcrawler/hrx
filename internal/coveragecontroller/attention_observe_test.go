package coveragecontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/meshcrawler/hrx/internal/coveragejournal"
)

func TestAttentionObserveNeverSubmitsAndRetainsHistoricalWitness(t *testing.T) {
	for _, status := range []string{"absent", "prepared", "uncertain", "admitted", "recovered"} {
		t.Run(status, func(t *testing.T) {
			h, store := attentionGateway(t)
			sum := sha256.Sum256([]byte(recoveryText))
			previous := attentionOutcome{Protocol: attentionProtocol, Runtime: strings.Repeat("b", 64), AdmissionBinding: strings.Repeat("b", 64), Thread: "thread", Delivery: recoveryDelivery, Digest: hex.EncodeToString(sum[:]), Status: status, Attempt: 1}
			if status == "admitted" {
				previous.Ordinal = 49
			}
			if status == "recovered" {
				previous.Status = "admitted"
				previous.RecoverySource, previous.RecoveryBinding, previous.RecoveryOrdinal = "history", strings.Repeat("c", 64), 51
			}
			if status != "absent" {
				payload, _ := json.Marshal(previous)
				store.stages = []coveragejournal.Stage{{Operation: "attention.prepared_no_write_v1", Payload: payload}}
			}
			if status == "prepared" || status == "uncertain" {
				go func() {
					recoveryReply(t, h, "thread/queue/list", `{"data":[],"nextCursor":null}`, 61)
					recoveryReply(t, h, "thread/read", `{"thread":{"id":"thread","turns":[]}}`, 62)
				}()
			}
			params, _ := json.Marshal(attentionRequest{Protocol: attentionProtocol, Runtime: h.attentionBinding(), Thread: "thread", Delivery: recoveryDelivery, Text: recoveryText, Attempt: 1})
			raw, err := h.attentionOperation(context.Background(), boardSource, json.RawMessage(`1`), params, true)
			if status == "absent" {
				if err == nil || len(store.History()) != 0 {
					t.Fatal("observation created an operation")
				}
			} else {
				var reply struct{ Result attentionOutcome }
				if err != nil || json.Unmarshal(raw, &reply) != nil {
					t.Fatal(err)
				}
				if status == "admitted" || status == "recovered" {
					previous.Runtime = h.attentionBinding()
					if reply.Result != previous {
						t.Fatal("historical admission witness changed", reply.Result)
					}
				} else if reply.Result.Status != "uncertain" {
					t.Fatal("absence became admission", reply.Result)
				}
				if len(store.History()) != 1 {
					t.Fatal("absence mutated admission history")
				}
			}
			select {
			case request := <-h.router.requests:
				t.Fatal("unexpected native work", request.method)
			default:
			}
		})
	}
}

func TestAttentionObserveRecoversOnlyPositiveHistoryWithoutInput(t *testing.T) {
	h, store := attentionGateway(t)
	seedAmbiguous(t, h, store, "uncertain")
	go func() {
		recoveryReply(t, h, "thread/queue/list", `{"data":[],"nextCursor":null}`, 61)
		recoveryReply(t, h, "thread/read", recoveryHistory("thread", "full", recoveryText), 62)
	}()
	params, _ := json.Marshal(attentionRequest{Protocol: attentionProtocol, Runtime: h.attentionBinding(), Thread: "thread", Delivery: recoveryDelivery, Text: recoveryText, Attempt: 1})
	raw, err := h.attentionOperation(context.Background(), boardSource, json.RawMessage(`1`), params, true)
	var reply struct{ Result attentionOutcome }
	if err != nil || json.Unmarshal(raw, &reply) != nil || reply.Result.Status != "admitted" || reply.Result.Ordinal != 0 || reply.Result.RecoveryOrdinal != 62 || reply.Result.RecoverySource != "history" {
		t.Fatal(err, reply.Result)
	}
	select {
	case request := <-h.router.requests:
		t.Fatal("unexpected native work", request.method)
	default:
	}
}

func TestAttentionObserveRejectsExternalOrStaleRequestsBeforeNativeRead(t *testing.T) {
	for _, mismatch := range []string{"terminal", "runtime", "thread", "attempt", "digest"} {
		t.Run(mismatch, func(t *testing.T) {
			h, store := attentionGateway(t)
			seedAmbiguous(t, h, store, "uncertain")
			source := boardSource
			request := attentionRequest{Protocol: attentionProtocol, Runtime: h.attentionBinding(), Thread: "thread", Delivery: recoveryDelivery, Text: recoveryText, Attempt: 1}
			switch mismatch {
			case "terminal":
				source = terminalSource
			case "runtime":
				request.Runtime = strings.Repeat("f", 64)
			case "thread":
				request.Thread = "other"
			case "attempt":
				request.Attempt = 2
			case "digest":
				request.Text = "changed"
			}
			params, _ := json.Marshal(request)
			if _, err := h.attentionOperation(context.Background(), source, json.RawMessage(`1`), params, true); err == nil {
				t.Fatal("mismatch accepted")
			}
			select {
			case work := <-h.router.requests:
				t.Fatal("mismatch reached native", work.method)
			default:
			}
		})
	}
}
