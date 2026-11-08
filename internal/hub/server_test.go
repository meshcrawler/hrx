package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/meshcrawler/hrx/internal/monitor"
)

func TestPostgresHTTPMultipleWorkersOwnership(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, s.Pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	other := &Store{Pool: pool, LeaseTTL: s.LeaseTTL, StaleAfter: s.StaleAfter}
	stores := []*Store{s, other}
	clients := make([]*Client, 2)
	requests := make([]LeaseRequest, 2)
	for i, id := range []string{"a", "b"} {
		testHost(t, s, id)
		api, err := NewServer(stores[i], Digest(strings.Repeat("admin", 10)), Digest(strings.Repeat("viewer", 10)))
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(api)
		defer server.Close()
		clients[i], err = NewClient(server.URL, strings.Repeat(id, 64))
		if err != nil {
			t.Fatal(err)
		}
		requests[i] = LeaseRequest{Version: Version, Identity: "team/mayor/agent", RunID: strings.Repeat(id, 32)}
	}
	type result struct {
		index int
		lease Lease
		err   error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for i := range clients {
		go func() {
			<-start
			var lease Lease
			err := clients[i].Call(ctx, "/api/v1/leases/acquire", requests[i], &lease)
			results <- result{i, lease, err}
		}()
	}
	close(start)
	winner, conflicts := -1, 0
	var lease Lease
	for range clients {
		r := <-results
		if r.err == nil {
			winner, lease = r.index, r.lease
		} else if errors.Is(r.err, ErrConflict) {
			var owner *OwnershipConflict
			if !errors.As(r.err, &owner) || owner.HostID != requests[1-r.index].RunID || owner.RunID != requests[1-r.index].RunID || owner.Generation != 1 || owner.ExpiresAt.IsZero() {
				t.Fatalf("missing competing owner diagnostic: %v", r.err)
			}
			conflicts++
		} else {
			t.Fatal(r.err)
		}
	}
	if winner < 0 || conflicts != 1 {
		t.Fatalf("expected one winner across API workers: winner=%d conflicts=%d", winner, conflicts)
	}
	requests[winner].Generation = lease.Generation
	if err := clients[winner].Call(ctx, "/api/v1/leases/release", requests[winner], nil); err != nil {
		t.Fatal(err)
	}
	var successor Lease
	if err := clients[1-winner].Call(ctx, "/api/v1/leases/acquire", requests[1-winner], &successor); err != nil || successor.Generation <= lease.Generation {
		t.Fatalf("successor: %+v %v", successor, err)
	}
	for _, action := range []string{"renew", "release", "acquire"} {
		request := requests[winner]
		if action == "acquire" {
			request.Generation = 0
		}
		if err := clients[winner].Call(ctx, "/api/v1/leases/"+action, request, nil); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale %s accepted: %v", action, err)
		}
	}
}

func TestPostgresHTTPAuthorityAndValidation(t *testing.T) {
	s := testStore(t)
	h := testHost(t, s, "a")
	admin, viewer := strings.Repeat("admin", 10), strings.Repeat("viewer", 10)
	api, err := NewServer(s, Digest(admin), Digest(viewer))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api)
	defer server.Close()
	call := func(method, path, token string, payload any) (int, string) {
		t.Helper()
		body, _ := json.Marshal(payload)
		req, _ := http.NewRequest(method, server.URL+path, bytes.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data)
	}
	for _, token := range []string{"", strings.Repeat("bad", 20)} {
		status, _ := call("GET", "/api/v1/agents", token, nil)
		if status != 401 && status != 403 {
			t.Fatal("unauthorized viewer accepted", status)
		}
	}
	status, _ := call("GET", "/api/v1/agents", strings.Repeat("a", 64), nil)
	if status != 403 {
		t.Fatal("host token grants dashboard read", status)
	}
	status, _ = call("POST", "/api/v1/hosts/revoke", viewer, map[string]any{"version": 1, "host_id": h.ID})
	if status != 403 {
		t.Fatal("viewer token grants host administration", status)
	}
	r := record(h, "b")
	batch := Batch{Version: 1, Sequence: 1, Records: []monitor.Record{r}}
	status, _ = call("POST", "/api/v1/observations", strings.Repeat("a", 64), batch)
	if status != 200 {
		t.Fatal("host ingestion failed", status)
	}
	other := testHost(t, s, "b")
	impersonated := record(other, "c")
	status, body := call("POST", "/api/v1/observations", strings.Repeat("a", 64), Batch{Version: Version, Sequence: 2, Records: []monitor.Record{impersonated}})
	if status != http.StatusForbidden {
		t.Fatal("host impersonation accepted", status)
	}
	status, body = call("POST", "/api/v1/leases/acquire", strings.Repeat("a", 64), LeaseRequest{Version: Version, Identity: "outside/dev/agent", RunID: r.RunID})
	if status != http.StatusForbidden || strings.Contains(body, "owner") {
		t.Fatal("out-of-scope acquisition accepted or disclosed ownership", status)
	}
	var object map[string]any
	data, _ := json.Marshal(batch)
	json.Unmarshal(data, &object)
	object["secret_token"] = "SECRET_CANARY"
	status, body = call("POST", "/api/v1/observations", strings.Repeat("a", 64), object)
	if status != 400 || strings.Contains(body, "SECRET_CANARY") {
		t.Fatal("unknown field leaked or accepted", status)
	}
	delete(object, "secret_token")
	object["version"] = 99
	status, _ = call("POST", "/api/v1/observations", strings.Repeat("a", 64), object)
	if status != 400 {
		t.Fatal("unsupported version accepted", status)
	}
	for _, token := range []string{admin, viewer} {
		status, body = call("GET", "/api/v1/agents", token, nil)
		if status != 200 || strings.Contains(body, "token_digest") || strings.Contains(body, strings.Repeat("a", 64)) {
			t.Fatal("snapshot authorization or redaction failed", status)
		}
	}
	if err := s.Revoke(context.Background(), h.ID); err != nil {
		t.Fatal(err)
	}
	status, _ = call("POST", "/api/v1/heartbeat", strings.Repeat("a", 64), map[string]any{"version": 1, "live_runs": []string{r.RunID}})
	if status != 401 {
		t.Fatal("revoked host still accepted", status)
	}
}
