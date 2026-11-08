package backend

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// This opt-in proof mints/replays/revokes one short-lived lease; no inference.
func TestBrokerLiveLeaseLifecycle(t *testing.T) {
	origin := os.Getenv("HRX_LEASE_LIVE_URL")
	if origin == "" {
		t.Skip("requires explicit live broker")
	}
	data, err := os.ReadFile(os.Getenv("HRX_LEASE_LIVE_AUTH_COMMAND"))
	if err != nil {
		t.Fatal("controller auth command file unavailable")
	}
	var argv []string
	if json.Unmarshal(data, &argv) != nil || len(argv) == 0 {
		t.Fatal("invalid controller auth command")
	}
	b := LeaseBroker{URL: origin, Authenticate: func(ctx context.Context) (string, error) {
		output, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
		if err != nil {
			return "", fmt.Errorf("controller auth command failed")
		}
		return strings.TrimSpace(string(output)), nil
	}}
	raw := make([]byte, 16)
	if _, err = rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	hex := fmt.Sprintf("%x", raw)
	request := leaseRequest()
	request.ClientID = "hrx-proxmox-acceptance"
	request.RequestID = hex[:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	lease, err := b.Mint(ctx, request)
	if lease.ID != "" {
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if e := b.Revoke(cleanup, lease.ID); e != nil {
				t.Errorf("lease %s needs revocation: %v", lease.ID, e)
			} else {
				t.Logf("revoked lease %s", lease.ID)
			}
		}()
	}
	if err != nil {
		t.Fatal(err)
	}
	again, err := b.Mint(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != lease.ID || again.key != lease.key {
		t.Fatal("idempotent retry minted a different grant")
	}
	t.Logf("verified scoped Claude lease %s; replay returned same grant; expires %s", lease.ID, lease.ExpiresAt.Format(time.RFC3339))
}
