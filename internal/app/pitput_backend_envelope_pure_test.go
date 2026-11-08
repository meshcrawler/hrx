package app

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// Golden values from compact_tool_envelope.py encode()'s canonical Python JSON
// shape, including non-ASCII stdout and a nonzero helper result.
const backendEnvelopeZero = "PITPUT_FIXTURE_V1_call_aaaaaaaaaaaaaaaaaaaaaaaa:eyJjYWxsX2lkIjoiY2FsbF9hYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWEiLCJyZXR1cm5jb2RlIjowLCJzdGRvdXQiOiJ7XCJib2R5X3V0ZjhcIjogXCJzeW50aGV0aWMgXHUwMGU5XHVkODNkXHVkZTQyXCIsIFwiY29tcGxldGVfbWVzc2FnZVwiOiBmYWxzZX1cbiIsInYiOjF9"
const backendEnvelopeTwo = "PITPUT_FIXTURE_V1_call_aaaaaaaaaaaaaaaaaaaaaaaa:eyJjYWxsX2lkIjoiY2FsbF9hYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWEiLCJyZXR1cm5jb2RlIjoyLCJzdGRvdXQiOiJ7XCJib2R5X3V0ZjhcIjogXCJzeW50aGV0aWMgXHUwMGU5XHVkODNkXHVkZTQyXCIsIFwiY29tcGxldGVfbWVzc2FnZVwiOiBmYWxzZX1cbiIsInYiOjF9"
const backendEnvelopeCallHash = "8cb542c670c16d9a707f398ac9964755057192795fd470deb8cdc70a585a1627"

func TestBackendCompactEnvelopeProjectsActualShapeWithoutReadSuccessOrBody(t *testing.T) {
	for code, token := range map[int]string{0: backendEnvelopeZero, 2: backendEnvelopeTwo} {
		value := backendCompactEnvelope([]string{token}, backendEnvelopeCallHash)
		if value["status"] != "observed" || value["returncode"] != code || value["stdout_bytes"] != 61 || value["stdout_sha256"] != "e50b9ca93fded85e504435ee0c5fbc477f0b48c1a1f24a5dfede51a31c77fa9d" {
			t.Fatal("canonical fixture envelope lost", value)
		}
		raw, _ := json.Marshal(value)
		if strings.Contains(string(raw), "synthetic") || strings.Contains(string(raw), "call_") || strings.Contains(string(raw), "completed") {
			t.Fatal("diagnostic exported body or claimed native read completion")
		}
	}
}

func TestBackendCompactEnvelopeKeepsAbsentUnknownMalformedAndAmbiguousDistinct(t *testing.T) {
	cases := []struct {
		content []string
		status  string
	}{
		{nil, "unavailable"}, {[]string{"private body command credential"}, "unrecognized"},
		{[]string{backendEnvelopeZero + "="}, "invalid"},
		{[]string{backendEnvelopeZero, backendEnvelopeZero}, "ambiguous"},
		{[]string{backendEnvelopeZero + "a"}, "invalid"},
	}
	for _, item := range cases {
		value := backendCompactEnvelope(item.content, backendEnvelopeCallHash)
		if value["status"] != item.status || value["returncode"] != nil {
			t.Fatal("unproven envelope became a result", value)
		}
		raw, _ := json.Marshal(value)
		if strings.Contains(string(raw), "private") {
			t.Fatal("arbitrary output leaked")
		}
	}
	if backendCompactEnvelope([]string{backendEnvelopeZero}, strings.Repeat("f", 64))["status"] != "unrecognized" {
		t.Fatal("foreign call envelope matched")
	}
	for _, raw := range []string{
		`{"v":1,"call_id":"call_aaaaaaaaaaaaaaaaaaaaaaaa","returncode":0,"stdout":"private"}`,
		`{"call_id":"call_aaaaaaaaaaaaaaaaaaaaaaaa","extra":"private","returncode":0,"stdout":"private","v":1}`,
	} {
		marker := "PITPUT_FIXTURE_V1_call_aaaaaaaaaaaaaaaaaaaaaaaa:" + base64.RawURLEncoding.EncodeToString([]byte(raw))
		if backendCompactEnvelope([]string{marker}, backendEnvelopeCallHash)["status"] != "invalid" {
			t.Fatal("noncanonical or extra fields accepted")
		}
	}
}

func TestBackendCompactEnvelopeIgnoresInputCommandsAndClassifiesOnlyKnownOutput(t *testing.T) {
	node := map[string]any{"input": map[string]any{"command": backendEnvelopeZero},
		"output": "BLOCKED: explicitly authorized disposable native fixture required private suffix"}
	value := backendCompactEnvelope(backendToolOutputStrings(node), backendEnvelopeCallHash)
	if value["status"] != "unrecognized" || value["wrapper_error_class"] != "fixture_authorization_missing" {
		t.Fatal("input command became helper output")
	}
	node = map[string]any{"type": "tool", "id": "call_aaaaaaaaaaaaaaaaaaaaaaaa", "name": "shell", "status": "completed", "output": backendEnvelopeTwo}
	states := backendNativeTimeline([]any{node}, backendEnvelopeCallHash)["tool_states"].([]map[string]any)
	if states[0]["status"] != "completed" || states[0]["fixture_output"].(map[string]any)["returncode"] != 2 {
		t.Fatal("native completed status hid nonzero helper result")
	}
}
