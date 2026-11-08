package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func leaseRequest() LeaseRequest {
	return LeaseRequest{RequestID: "12345678-1234-1234-1234-123456789abc", ClientID: "hrx-worker", Purpose: "development", Models: []string{"claude-sonnet-5-5"}, CredentialProfile: "claude-code", TTLSeconds: 300, MaxBudgetUSD: 1, RPMLimit: 10, TPMLimit: 200000, MaxParallelRequests: 1}
}
func leaseResponse() map[string]any {
	return map[string]any{
		"lease_id": "87654321-1234-1234-1234-123456789abc", "key": "private-gateway-key", "expires_at": time.Now().Add(4 * time.Minute).UTC().Format(time.RFC3339), "models": []string{"claude-sonnet-5-5"},
		"limits":      map[string]any{"max_budget_usd": 1, "rpm_limit": 10, "tpm_limit": 200000, "max_parallel_requests": 1},
		"gateway":     map[string]any{"openai_base_url": "https://gateway.example/v1", "anthropic_base_url": "https://gateway.example"},
		"credentials": map[string]any{"profile": "claude-code", "claude_code_oauth_token": "sk-ant-oat-private-fixture"},
	}
}
func brokerFor(t *testing.T, handler http.HandlerFunc) LeaseBroker {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	return LeaseBroker{URL: server.URL, Client: server.Client(), Authenticate: func(context.Context) (string, error) { return "private-bootstrap-token", nil }}
}
func TestLeaseMintPrivateTransferAndRevoke(t *testing.T) {
	revoked := false
	b := brokerFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer private-bootstrap-token" {
			t.Error("missing broker authentication")
		}
		if r.Method == http.MethodDelete {
			revoked = true
			w.WriteHeader(204)
			return
		}
		if r.Header.Get("Idempotency-Key") != leaseRequest().RequestID {
			t.Error("missing durable retry key")
		}
		json.NewEncoder(w).Encode(leaseResponse())
	})
	lease, err := b.Mint(context.Background(), leaseRequest())
	if err != nil {
		t.Fatal(err)
	}
	normal, _ := json.Marshal(lease)
	for _, s := range []string{string(normal), fmt.Sprintf("%+v", lease), fmt.Sprintf("%#v", lease)} {
		if strings.Contains(s, "private-") {
			t.Fatal("credential in ordinary formatting")
		}
	}
	private, err := lease.PrivateJSON()
	if err != nil || !strings.Contains(string(private), "private-gateway-key") || !strings.Contains(string(private), "sk-ant-oat-private-fixture") {
		t.Fatal("private transfer missing credentials")
	}
	if err = b.Revoke(context.Background(), lease.ID); err != nil || !revoked {
		t.Fatalf("revoke failed %v", err)
	}
}
func TestLeaseRejectsWrongScopeExpiryAndMissingOAuth(t *testing.T) {
	changes := []func(map[string]any){
		func(m map[string]any) { m["models"] = []string{"other-model"} },
		func(m map[string]any) { m["expires_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339) },
		func(m map[string]any) { m["limits"].(map[string]any)["max_budget_usd"] = 2 },
		func(m map[string]any) { delete(m, "credentials") },
	}
	for i, change := range changes {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			b := brokerFor(t, func(w http.ResponseWriter, r *http.Request) {
				data := leaseResponse()
				change(data)
				json.NewEncoder(w).Encode(data)
			})
			lease, err := b.Mint(context.Background(), leaseRequest())
			if err == nil {
				t.Fatal("invalid grant accepted")
			}
			if lease.ID == "" {
				t.Fatal("lost revocation handle")
			}
			if _, err = lease.PrivateJSON(); err == nil {
				t.Fatal("invalid grant exposed credentials")
			}
		})
	}
}
func TestBrokerSuppressesBodiesAndRedirects(t *testing.T) {
	for _, status := range []int{401, 500, 302} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			b := brokerFor(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "https://untrusted.example")
				w.WriteHeader(status)
				fmt.Fprint(w, "private-bootstrap-token private-gateway-key")
			})
			_, err := b.Mint(context.Background(), leaseRequest())
			if err == nil || strings.Contains(err.Error(), "private-") {
				t.Fatalf("unsafe broker failure: %v", err)
			}
		})
	}
}

func TestLeaseBrokerRejectsInvalidIdentifiersBeforeAuthentication(t *testing.T) {
	for _, value := range []string{"", "readiness challenge", strings.Repeat("a", 65), "worker/name"} {
		for _, field := range []string{"client", "purpose"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				request := leaseRequest()
				if field == "client" {
					request.ClientID = value
				} else {
					request.Purpose = value
				}
				broker := LeaseBroker{URL: "https://broker.example", Authenticate: func(context.Context) (string, error) {
					t.Fatal("invalid request reached authentication")
					return "", nil
				}}
				if _, err := broker.Mint(context.Background(), request); err == nil {
					t.Fatal("invalid broker identifier accepted")
				}
			})
		}
	}
}
