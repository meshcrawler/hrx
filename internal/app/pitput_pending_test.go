package app

import (
	"strings"
	"testing"

	"github.com/meshcrawler/hrx/internal/coveragecontroller"
)

func TestPitputPendingVerificationExactBodyFreeResponse(t *testing.T) {
	request := coveragecontroller.OwnedPendingRequest{RuntimeBinding: strings.Repeat("a", 64),
		ThreadID: "019ef2d0-1234-7000-8000-123456789abc", DeliveryID: "delivery-" + strings.Repeat("b", 64)}
	valid := `{"protocol":"combined-attention.v1","delivery_id":"` + request.DeliveryID + `","pending":true}`
	if err := verifyPitputPendingResponse([]byte(valid), request); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{}`, strings.Replace(valid, "true", "false", 1),
		strings.Replace(valid, request.DeliveryID, "delivery-"+strings.Repeat("c", 64), 1),
		strings.TrimSuffix(valid, "}") + `,"body":"private"}`, valid + `{}`} {
		if err := verifyPitputPendingResponse([]byte(raw), request); err == nil {
			t.Fatal("invalid/unmatched verification accepted")
		}
	}
	args, err := pitputPendingArgs("/owned/runtime", "host/role/agent", request)
	if err != nil || len(args) != 12 || args[3] != "/owned/runtime/"+request.ThreadID+"/@combined/bridge.toml" {
		t.Fatal("host config selection changed", args, err)
	}
	request.ThreadID = "../foreign"
	if _, err := pitputPendingArgs("/owned/runtime", "host/role/agent", request); err == nil {
		t.Fatal("caller selected foreign config")
	}
	if callback := (&pitputRuntime{}).pendingVerifier("/owned/runtime", nil, "/project"); callback != nil {
		t.Fatal("ordinary launch enabled verification")
	}
}
