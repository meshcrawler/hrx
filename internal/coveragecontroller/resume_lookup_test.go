package coveragecontroller

import (
	"encoding/json"
	"testing"
)

func TestResumeLookupOnlyPermitsSavedThreadRead(t *testing.T) {
	for _, tc := range []struct {
		raw, expected string
		valid         bool
	}{
		{`{"threadId":"saved","includeTurns":true}`, "saved", true},
		{`{"threadId":"saved"}`, "saved", true},
		{`{"threadId":"saved"}`, "", false},
		{`{"threadId":"other"}`, "saved", false},
		{`{"threadId":"saved","includeTurns":null}`, "saved", false},
		{`{"threadId":"saved","input":[]}`, "saved", false},
		{`{"threadId":"saved","threadId":"other"}`, "saved", false},
	} {
		if validResumeLookup(json.RawMessage(tc.raw), tc.expected) != tc.valid {
			t.Fatal(tc.raw)
		}
	}
}
