package coveragecontroller

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestPreservedStartRetainsNativeFieldsWithoutAliasing(t *testing.T) {
	raw := json.RawMessage(`{ "cwd":"/fixture", "historyMode":"paginated", "ephemeral":false,"permissions":"workspace","developerInstructions":"native <terminal> guidance","config":{"features":{"terminal_visualization":true}},"futureNativeField":{"a":1} }`)
	got, err := preservedStart(raw, 4096)
	if err != nil {
		t.Fatal(err)
	}
	var before, after any
	json.Unmarshal(raw, &before)
	json.Unmarshal(got, &after)
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if !bytes.Equal(a, b) {
		t.Fatal("native fields changed")
	}
	original := bytes.Clone(got)
	raw[0] = '!'
	if !bytes.Equal(got, original) {
		t.Fatal("aliased caller request")
	}
}
func TestPreservedStartRejectsAmbiguousAndUnboundedRequests(t *testing.T) {
	for _, raw := range []string{
		`{"cwd":"/fixture","historyMode":"paginated","ephemeral":true}`,
		`{"cwd":"/fixture","historyMode":"paginated","ephemeral":null}`,
		`{"cwd":"relative","historyMode":"paginated"}`,
		`{"cwd":"/fixture","historyMode":"legacy"}`,
		`{"cwd":"/fixture","historyMode":"paginated","config":{"a":1,"a":2}}`,
		`{"cwd":"/fixture","historyMode":"paginated"} {}`,
	} {
		if _, err := preservedStart(json.RawMessage(raw), 4096); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	if _, err := preservedStart(json.RawMessage(`{"cwd":"/fixture","historyMode":"paginated"}`), 10); err == nil {
		t.Fatal("unbounded request")
	}
}

func TestPreservedStartKeepsNativeWorkingDirectoryInheritance(t *testing.T) {
	for _, raw := range []string{`{"historyMode":"paginated"}`, `{"cwd":null,"historyMode":"paginated"}`} {
		got, err := preservedStart(json.RawMessage(raw), 4096)
		if err != nil || string(got) != raw {
			t.Fatal("native inheritance changed", err)
		}
	}
	for _, value := range []string{`""`, `"relative"`, `42`, `false`} {
		if _, err := preservedStart(json.RawMessage(`{"cwd":`+value+`,"historyMode":"paginated"}`), 4096); err == nil {
			t.Fatal("invalid explicit cwd accepted")
		}
	}
}
