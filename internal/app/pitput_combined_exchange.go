package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

type ownedAdmissionRequest struct {
	Protocol string `json:"protocol"`
	Runtime  string `json:"runtime_binding"`
	Thread   string `json:"thread_id"`
	Delivery string `json:"delivery_id"`
	Text     string `json:"text"`
	Attempt  uint64 `json:"attempt"`
}

// No socket/process ownership here: the caller supplies exchange on its one
// private gateway endpoint. Continuation is a separate RPC, never a shared-lease
// upgrade inside admit. Original admission bytes remain the stdout/report fact.
func ownedAdmissionExchange(request ownedAdmissionRequest, exchange func(string, any) (json.RawMessage, error)) (json.RawMessage, error) {
	return ownedAdmissionExchangeMode(request, exchange, false)
}

func ownedAdmissionExchangeMode(request ownedAdmissionRequest, exchange func(string, any) (json.RawMessage, error), observeOnly bool) (json.RawMessage, error) {
	fail := errors.New("owned attention outcome uncertain")
	method := "pitput/attention/admit"
	if observeOnly {
		method = "pitput/attention/observe"
	}
	raw, err := exchange(method, request)
	if err != nil {
		return nil, fail
	}
	var result struct {
		Protocol        string `json:"protocol"`
		Runtime         string `json:"runtime_binding"`
		Admission       string `json:"admission_binding"`
		Thread          string `json:"thread_id"`
		Delivery        string `json:"delivery_id"`
		Digest          string `json:"payload_sha256"`
		Status          string `json:"status"`
		Attempt         uint64 `json:"attempt"`
		Ordinal         uint64 `json:"response_ordinal"`
		RecoverySource  string `json:"recovery_source"`
		RecoveryBinding string `json:"recovery_binding"`
		RecoveryOrdinal uint64 `json:"recovery_ordinal"`
	}
	digest := sha256.Sum256([]byte(request.Text))
	hash := hex.EncodeToString(digest[:])
	binding := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if json.Unmarshal(raw, &result) != nil || result.Protocol != request.Protocol || result.Runtime != request.Runtime ||
		result.Thread != request.Thread || result.Delivery != request.Delivery || result.Digest != hash || result.Attempt != request.Attempt ||
		!binding.MatchString(result.Admission) || (result.Status != "admitted" && result.Status != "not_admitted" && result.Status != "uncertain") {
		return nil, fail
	}
	recovery := result.RecoverySource != "" || result.RecoveryBinding != "" || result.RecoveryOrdinal != 0
	if recovery && (result.Status != "admitted" || result.Ordinal != 0 || result.RecoveryOrdinal == 0 || !binding.MatchString(result.RecoveryBinding) || (result.RecoverySource != "queue" && result.RecoverySource != "history")) {
		return nil, fail
	}
	if result.Status == "not_admitted" && result.Ordinal != 0 {
		return nil, fail
	}
	if result.Status == "admitted" && result.Ordinal == 0 && !recovery {
		return nil, fail
	}
	if observeOnly || result.Status != "admitted" || result.Admission == request.Runtime {
		return raw, nil
	}
	continued, err := exchange("pitput/attention/continue", request)
	if err != nil {
		return raw, fail
	}
	var continuation struct {
		Protocol   string `json:"protocol"`
		Runtime    string `json:"runtime_binding"`
		Thread     string `json:"thread_id"`
		Delivery   string `json:"delivery_id"`
		Digest     string `json:"payload_sha256"`
		Original   string `json:"original_turn_id"`
		New        string `json:"new_turn_id"`
		Status     string `json:"status"`
		Submission string `json:"submission_binding"`
		Ordinal    uint64 `json:"response_ordinal"`
	}
	if json.Unmarshal(continued, &continuation) != nil || continuation.Protocol != "hrx-owned-attention-continuation.v1" ||
		continuation.Runtime != request.Runtime || continuation.Thread != request.Thread || continuation.Delivery != request.Delivery || continuation.Digest != hash || strings.TrimSpace(continuation.Original) == "" || len(continuation.Original) > 512 {
		return raw, fail
	}
	switch continuation.Status {
	case "started":
		if strings.TrimSpace(continuation.New) == "" || len(continuation.New) > 512 || continuation.New == continuation.Original || !binding.MatchString(continuation.Submission) || continuation.Ordinal == 0 {
			return raw, fail
		}
	case "not_required":
		if continuation.New != "" || continuation.Ordinal != 0 || continuation.Submission != "" {
			return raw, fail
		}
	default:
		return raw, fail // uncertain never becomes successful admission/handling.
	}
	return raw, nil
}
