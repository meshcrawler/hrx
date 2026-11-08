package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestEnrollmentBindsGrantAndRedactsCredential(t *testing.T) {
	raw := []byte(`{"identity":"host/worker/agent","boards":["/b/test"],"pitput":{"url":"https://forum.example","token":"fixture-private-token","ca_pem":"certificate"}}`)
	profile, err := decodeEnrollment(raw, "host/worker/agent", []string{"test"})
	if err != nil {
		t.Fatal(err)
	}
	ordinary, _ := json.Marshal(profile)
	for _, output := range []string{string(ordinary), fmt.Sprint(profile), fmt.Sprintf("%#v", profile)} {
		if strings.Contains(output, "fixture-private-token") {
			t.Fatal("credential exposed in ordinary formatting")
		}
	}
	private, err := profile.PrivateJSON()
	if err != nil || !strings.Contains(string(private), "fixture-private-token") {
		t.Fatal("private transfer unavailable")
	}
	for _, changed := range []string{
		strings.Replace(string(raw), "host/worker/agent", "host/other/agent", 1),
		strings.Replace(string(raw), "/b/test", "/b/other", 1),
		strings.Replace(string(raw), "https://forum.example", "http://forum.example", 1),
		strings.Replace(string(raw), "fixture-private-token", "", 1),
		strings.Replace(string(raw), `"ca_pem":"certificate"`, `"enrollment_token":"must-not-transfer"`, 1),
		string(raw) + ` {}`,
	} {
		if _, err := decodeEnrollment([]byte(changed), "host/worker/agent", []string{"test"}); err == nil {
			t.Fatal("mismatched or unsafe grant accepted")
		}
	}
}

func TestEnrollmentRejectsInvalidRequestBeforeExecution(t *testing.T) {
	for _, item := range []struct {
		identity string
		boards   []string
	}{
		{"host/worker/agent", nil}, {"invalid", []string{"test"}}, {"host/worker/agent", []string{"test/thread"}},
	} {
		if _, err := EnrollAgent(context.Background(), "/does-not-exist", item.identity, item.boards); err == nil || strings.Contains(err.Error(), "controller diagnostics") {
			t.Fatal("invalid request reached executable", err)
		}
	}
	boards, err := enrollmentBoards([]string{"b", "/b/a", "a"})
	if err != nil || strings.Join(boards, ",") != "a,b" {
		t.Fatal("board normalization failed")
	}
}
