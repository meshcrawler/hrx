package coveragecontroller

import (
	"encoding/json"
	"testing"
)

func TestInitialInstructionsPreserveTerminalGuidanceAndNativeFields(t *testing.T) {
	for _, existing := range []string{"null", `"terminal guidance"`, `"init guidance"`} {
		raw := json.RawMessage(`{"cwd":"/fixture","historyMode":"paginated","developerInstructions":` + existing + `,"config":{"features":{"example":true}},"futureNativeField":[1,2]}`)
		got, err := initializedStart(raw, "init guidance", 4096)
		if err != nil {
			t.Fatal(err)
		}
		var before, after map[string]json.RawMessage
		_ = json.Unmarshal(raw, &before)
		_ = json.Unmarshal(got, &after)
		var instructions string
		if json.Unmarshal(after["developerInstructions"], &instructions) != nil {
			t.Fatal("instructions absent")
		}
		expected := "init guidance"
		if existing == `"terminal guidance"` {
			expected += "\n\nterminal guidance"
		}
		if instructions != expected {
			t.Fatal("guidance changed")
		}
		for _, key := range []string{"cwd", "historyMode", "config", "futureNativeField"} {
			if string(before[key]) != string(after[key]) {
				t.Fatal("native field changed", key)
			}
		}
	}
}

func TestInitialInstructionsRejectMalformedOrOversizeBootstrap(t *testing.T) {
	for _, raw := range []string{
		`{"cwd":"/fixture","historyMode":"paginated","developerInstructions":42}`,
		`{"cwd":"/fixture","historyMode":"paginated","developerInstructions":"a","developerInstructions":"b"}`,
	} {
		if _, err := initializedStart(json.RawMessage(raw), "init", 4096); err == nil {
			t.Fatal("ambiguous instructions accepted")
		}
	}
	raw := json.RawMessage(`{"cwd":"/fixture","historyMode":"paginated"}`)
	if _, err := initializedStart(raw, "init guidance", len(raw)+1); err == nil {
		t.Fatal("amended request exceeded bound")
	}
	if _, err := initializedStart(raw, string([]byte{0xff}), 4096); err == nil {
		t.Fatal("invalid UTF-8 initialization accepted")
	}
	got, err := initializedStart(raw, "", 4096)
	if err != nil || string(got) != string(raw) {
		t.Fatal("empty initialization changed native request", err)
	}
}

func TestInitialInstructionsExplicitEmptyClearsBase(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"historyMode":"paginated"}`, ""},
		{`{"historyMode":"paginated","developerInstructions":"terminal guidance"}`, "terminal guidance"},
	} {
		raw, err := initializedStartValue(json.RawMessage(tc.raw), "", true, 4096)
		var fields map[string]json.RawMessage
		if err != nil || json.Unmarshal(raw, &fields) != nil {
			t.Fatal(err)
		}
		var got string
		if json.Unmarshal(fields["developerInstructions"], &got) != nil || got != tc.want {
			t.Fatal("empty profile lost explicit override or terminal text", string(raw))
		}
	}
}
