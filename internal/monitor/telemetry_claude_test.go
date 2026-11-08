package monitor

import "testing"

func TestClaudeTelemetryUsage(t *testing.T) {
	event, ok := parseClaudeTelemetry([]byte(`{"type":"assistant","sessionId":"native-session","version":"2.1.306","timestamp":"2026-10-21T12:00:00Z","message":{"model":"claude-opus-5-5","usage":{"input_tokens":2,"cache_read_input_tokens":100,"cache_creation_input_tokens":20,"output_tokens":999,"cache_creation":{"ephemeral_1h_input_tokens":20}}}}`))
	if !ok || event.Used == nil || *event.Used != 122 || event.Limit != nil || event.Session != "native-session" || event.Model != "claude-opus-5-5" || event.Version != "2.1.306" || event.At.IsZero() {
		t.Fatalf("unexpected usage event: %+v", event)
	}
}

func TestClaudeTelemetryInvalidUsagePreservesModel(t *testing.T) {
	for _, usage := range []string{`null`, `{}`, `{"input_tokens":-1}`, `{"input_tokens":1,"cache_read_input_tokens":-1}`, `{"input_tokens":1.5}`, `{"input_tokens":1,"cache_creation_input_tokens":"bad"}`, `{"input_tokens":9223372036854775807,"cache_read_input_tokens":1}`} {
		event, ok := parseClaudeTelemetry([]byte(`{"type":"assistant","sessionId":"s","message":{"model":"changed","usage":` + usage + `}}`))
		if !ok || event.Model != "changed" || event.Used != nil || event.Limit != nil {
			t.Fatalf("usage %s: %+v, %v", usage, event, ok)
		}
	}
}

func TestClaudeTelemetryCompaction(t *testing.T) {
	for _, metadata := range []string{`{"postTokens":123,"preTokens":100000}`, `{"preTokens":100000}`, `{"postTokens":-1}`} {
		event, ok := parseClaudeTelemetry([]byte(`{"type":"system","subtype":"compact_boundary","sessionId":"s","uuid":"boundary-id","compactMetadata":` + metadata + `}`))
		if !ok || !event.ResetContext || event.CompactionID != "boundary-id" || event.Limit != nil || event.Started {
			t.Fatalf("unexpected boundary: %+v", event)
		}
		if metadata == `{"postTokens":123,"preTokens":100000}` {
			if event.Used == nil || *event.Used != 123 {
				t.Fatalf("missing post-compaction estimate: %+v", event)
			}
		} else if event.Used != nil {
			t.Fatalf("invalid post-compaction estimate: %+v", event)
		}
	}
}

func TestClaudeTelemetryIgnoresUnrelatedRecords(t *testing.T) {
	for _, line := range []string{`not json`, `{"type":"assistant","message":{"model":"m"}}`, `{"type":"user","sessionId":"s"}`, `{"type":"system","subtype":"compact_start","sessionId":"s"}`, `{"type":"assistant","sessionId":"s","isSidechain":true,"message":{"model":"m","usage":{"input_tokens":1}}}`} {
		if event, ok := parseClaudeTelemetry([]byte(line)); ok {
			t.Fatalf("accepted %s: %+v", line, event)
		}
	}
}
