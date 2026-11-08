package app

import (
	"encoding/json"
	"testing"

	"github.com/meshcrawler/hrx/internal/codexrpc"
)

func TestPitputObservationDefersOnlyCorrelatedPublicationRefusal(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		err       error
		want      bool
	}{
		{"publication", `{"code":-32001,"message":"coverage admission unavailable"}`, codexrpc.ErrRemote, true},
		{"transport", `{"code":-32001,"message":"coverage admission unavailable"}`, codexrpc.ErrRead, false},
		{"scope", `{"code":-32602,"message":"request outside admitted scope"}`, codexrpc.ErrRemote, false},
		{"native", `{"code":-32001,"message":"native thread missing"}`, codexrpc.ErrRemote, false},
		{"malformed", `{`, codexrpc.ErrRemote, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := fixtureObservationDeferred(codexrpc.Response{RemoteError: json.RawMessage(tc.raw)}, tc.err); got != tc.want {
				t.Fatalf("deferred=%t, want %t", got, tc.want)
			}
		})
	}
}
