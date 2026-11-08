package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func exchangeRequest() ownedAdmissionRequest {
	return ownedAdmissionRequest{Protocol: "hrx-owned-attention.v1", Runtime: strings.Repeat("a", 64), Thread: "thread", Delivery: "delivery-" + strings.Repeat("b", 64), Text: "notice", Attempt: 1}
}
func exchangeAdmission(request ownedAdmissionRequest, old bool) json.RawMessage {
	digest := sha256.Sum256([]byte(request.Text))
	binding := request.Runtime
	if old {
		binding = strings.Repeat("c", 64)
	}
	raw, _ := json.Marshal(map[string]any{"protocol": request.Protocol, "runtime_binding": request.Runtime, "admission_binding": binding, "thread_id": request.Thread, "delivery_id": request.Delivery, "payload_sha256": hex.EncodeToString(digest[:]), "status": "admitted", "attempt": request.Attempt, "response_ordinal": 7})
	return raw
}
func exchangeContinuation(request ownedAdmissionRequest, status string) json.RawMessage {
	digest := sha256.Sum256([]byte(request.Text))
	value := map[string]any{"protocol": "hrx-owned-attention-continuation.v1", "runtime_binding": request.Runtime, "thread_id": request.Thread, "delivery_id": request.Delivery, "payload_sha256": hex.EncodeToString(digest[:]), "status": status, "original_turn_id": "original", "response_ordinal": 0, "submission_binding": ""}
	if status == "started" {
		value["new_turn_id"] = "new"
		value["response_ordinal"] = 9
		value["submission_binding"] = request.Runtime
	}
	raw, _ := json.Marshal(value)
	return raw
}
func TestOwnedAdmissionExchangeFreshAdmissionNeverContinues(t *testing.T) {
	request := exchangeRequest()
	raw := exchangeAdmission(request, false)
	calls := 0
	result, err := ownedAdmissionExchange(request, func(method string, params any) (json.RawMessage, error) {
		calls++
		if method != "pitput/attention/admit" {
			t.Fatal(method)
		}
		return raw, nil
	})
	if err != nil || string(result) != string(raw) || calls != 1 {
		t.Fatal(err, calls)
	}
}
func TestOwnedAdmissionExchangeReplaySeparatesContinuationAndRetainsOriginalBytes(t *testing.T) {
	for _, status := range []string{"started", "not_required"} {
		t.Run(status, func(t *testing.T) {
			request := exchangeRequest()
			raw := exchangeAdmission(request, true)
			methods := []string{}
			result, err := ownedAdmissionExchange(request, func(method string, params any) (json.RawMessage, error) {
				methods = append(methods, method)
				if method == "pitput/attention/admit" {
					return raw, nil
				}
				if method != "pitput/attention/continue" {
					t.Fatal(method)
				}
				actual, ok := params.(ownedAdmissionRequest)
				if !ok || actual != request {
					t.Fatal("continuation changed original exact operation")
				}
				return exchangeContinuation(request, status), nil
			})
			if err != nil || string(result) != string(raw) || len(methods) != 2 {
				t.Fatal(err, methods)
			}
		})
	}
}
func TestOwnedAdmissionExchangeKeepsHistoricalRecoveryBindingOnLaterRuntimeReplay(t *testing.T) {
	request := exchangeRequest()
	var value map[string]any
	_ = json.Unmarshal(exchangeAdmission(request, true), &value)
	value["response_ordinal"] = 0
	value["recovery_source"] = "history"
	value["recovery_binding"] = strings.Repeat("d", 64)
	value["recovery_ordinal"] = 8
	raw, _ := json.Marshal(value)
	result, err := ownedAdmissionExchange(request, func(method string, _ any) (json.RawMessage, error) {
		if method == "pitput/attention/admit" {
			return raw, nil
		}
		return exchangeContinuation(request, "not_required"), nil
	})
	if err != nil || string(result) != string(raw) {
		t.Fatal("historical witness was renamed or rejected", err)
	}
}
func TestOwnedAdmissionExchangeUnknownContinuationPreservesEvidenceButReturnsUncertain(t *testing.T) {
	for _, which := range []string{"uncertain", "lost", "wrong-binding", "wrong-delivery", "wrong-digest", "no-new-id", "zero-ordinal"} {
		t.Run(which, func(t *testing.T) {
			request := exchangeRequest()
			raw := exchangeAdmission(request, true)
			result, err := ownedAdmissionExchange(request, func(method string, _ any) (json.RawMessage, error) {
				if method == "pitput/attention/admit" {
					return raw, nil
				}
				if which == "lost" {
					return nil, errors.New("lost reply")
				}
				var value map[string]any
				_ = json.Unmarshal(exchangeContinuation(request, "started"), &value)
				switch which {
				case "uncertain":
					return exchangeContinuation(request, "uncertain"), nil
				case "wrong-binding":
					value["runtime_binding"] = "wrong"
				case "wrong-delivery":
					value["delivery_id"] = "wrong"
				case "wrong-digest":
					value["payload_sha256"] = "wrong"
				case "no-new-id":
					delete(value, "new_turn_id")
				case "zero-ordinal":
					value["response_ordinal"] = 0
				}
				bad, _ := json.Marshal(value)
				return bad, nil
			})
			if err == nil || string(result) != string(raw) {
				t.Fatal("continuation status or original admission evidence lost", which)
			}
		})
	}
}

func TestOwnedAdmissionObserveNeverRequestsContinuation(t *testing.T) {
	request := exchangeRequest()
	raw := exchangeAdmission(request, true)
	calls := 0
	result, err := ownedAdmissionExchangeMode(request, func(method string, _ any) (json.RawMessage, error) {
		calls++
		if method != "pitput/attention/observe" {
			t.Fatal(method)
		}
		return raw, nil
	}, true)
	if err != nil || calls != 1 || string(result) != string(raw) {
		t.Fatal(err, calls)
	}
}
