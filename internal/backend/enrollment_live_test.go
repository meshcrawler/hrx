package backend

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPitputLiveEnrollmentReuse(t *testing.T) {
	python := os.Getenv("HRX_ENROLLMENT_LIVE_PYTHON")
	if python == "" {
		t.Skip("requires explicit controller enrollment installation")
	}
	identity, board := os.Getenv("HRX_AGENT_LIVE_IDENTITY"), os.Getenv("HRX_AGENT_LIVE_BOARD")
	if identity == "" || board == "" {
		t.Fatal("explicit identity and board required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	first, err := EnrollAgent(ctx, python, identity, []string{board})
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnrollAgent(ctx, python, identity, []string{board})
	if err != nil {
		t.Fatal(err)
	}
	if first.token != second.token || first.URL != second.URL || strings.Join(first.Boards, ",") != strings.Join(second.Boards, ",") {
		t.Fatal("repeat enrollment changed scoped grant")
	}
	t.Logf("verified repeat enrollment for %s; scoped credential unchanged", identity)
}
