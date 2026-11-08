package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFinishRemainsExitedWhileSupervisorLives(t *testing.T) {
	s := Store{filepath.Join(t.TempDir(), "monitor")}
	r, err := s.Register(Record{Agent: "host/dev/agent", Harness: "codex", SessionID: "retained-session"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(r.RunID); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Snapshot(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Status != "exited" || rows[0].EndedAt == nil {
		t.Fatal("live supervisor resurrected ended run", err)
	}
	ended := *rows[0].EndedAt
	if err := s.Activity(r.RunID, "tool-use", "late-hook"); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(r.RunID); err != nil {
		t.Fatal(err)
	}
	rows, err = s.Snapshot(context.Background())
	if err != nil || rows[0].SessionID != "retained-session" || !rows[0].LastActivity.Equal(r.LastActivity) || !rows[0].EndedAt.Equal(ended) || !rows[0].LastSeen.Equal(ended) {
		t.Fatal("late hook or repeat cleanup modified ended session", err)
	}
}

func TestStoreActivityAndPIDReuse(t *testing.T) {
	s := Store{filepath.Join(t.TempDir(), "monitor")}
	r, err := s.Register(Record{Agent: "host/dev/agent", Harness: "claude", Model: "opus", Credential: Credential{Provider: "gitlab", Profile: "developer", Scopes: []string{"api", "write_repository"}}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.HostID()
	if err != nil || id != r.HostID {
		t.Fatal("host identity changed")
	}
	rows, err := s.Snapshot(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Status != "running" {
		t.Fatal("live observation failed", err)
	}
	if !rows[0].LastActivity.Equal(r.LastActivity) {
		t.Fatal("observation falsely counted as activity")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Activity(r.RunID, "user-message", "native-session"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	rows, err = s.Snapshot(context.Background())
	if err != nil || rows[0].SessionID != "native-session" || rows[0].Activity != "user-message" || !rows[0].LastActivity.After(r.LastActivity) {
		t.Fatal("activity not captured", err)
	}
	if err := s.update(r.RunID, func(r *Record) { r.ProcessStart = "a recycled PID" }); err != nil {
		t.Fatal(err)
	}
	last := rows[0].LastSeen
	rows, err = s.Snapshot(context.Background())
	if err != nil || rows[0].Status != "exited" || !rows[0].LastSeen.Equal(last) {
		t.Fatal("recycled PID reported alive", err)
	}
	encoded, _ := json.Marshal(rows)
	if strings.Contains(string(encoded), "process_start") || strings.Contains(string(encoded), "token") {
		t.Fatal("private fields exported")
	}
	if err := s.Activity("../../escape", "tool-use", ""); err == nil {
		t.Fatal("unsafe run ID accepted")
	}
	if err := s.Activity(r.RunID, "unrecognized", ""); err == nil {
		t.Fatal("unrecognized event accepted")
	}
}

func TestCollectorSourcesAndStaleCache(t *testing.T) {
	now := time.Now().UTC()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/local" {
			t.Error("collector used aggregating endpoint")
		}
		// Deliberately include secret fields not in the public schema.
		_, _ = w.Write([]byte(`{"version":1,"generated_at":"` + now.Format(time.RFC3339Nano) + `","agents":[{"run_id":"same-name","host_id":"remote-host","host":"remote","agent":"same-agent","last_seen":"` + now.Format(time.RFC3339Nano) + `","last_activity":"` + now.Format(time.RFC3339Nano) + `","token":"MUST_NOT_LEAK","credential":{"provider":"gitlab","token":"MUST_NOT_LEAK"}}]}`))
	}))
	defer remote.Close()
	c := &Collector{Local: func(context.Context) ([]Record, error) {
		return []Record{{RunID: "same-name", HostID: "local-host", Agent: "same-agent", LastActivity: now.Add(-time.Minute)}}, nil
	}, Peers: map[string]string{"remote": remote.URL}}
	c.Refresh(context.Background())
	if len(c.all.Agents) != 2 || c.all.Agents[0].HostID != "remote-host" {
		t.Fatal("host-scoped rows or activity sort lost")
	}
	encoded, _ := json.Marshal(c.all)
	if strings.Contains(string(encoded), "MUST_NOT_LEAK") {
		t.Fatal("source leaked non-schema fields")
	}
	remote.Close()
	c.Refresh(context.Background())
	if len(c.all.Agents) != 2 || c.all.Agents[0].SourceStatus != "unreachable" || !c.all.Agents[0].LastSeen.Equal(now) {
		t.Fatal("offline source lost cache or advanced timestamp")
	}
}

func TestDashboardHTTPBoundaries(t *testing.T) {
	c := &Collector{Local: func(context.Context) ([]Record, error) { return []Record{}, nil }}
	c.Refresh(context.Background())
	handler := c.Handler()
	for _, tc := range []struct {
		path, host, origin, method string
		status                     int
	}{
		{"/", "127.0.0.1:8787", "", "GET", 200},
		{"/api/v1/agents", "localhost:8787", "", "GET", 200},
		{"/api/v1/agents", "attacker.example", "", "GET", 403},
		{"/api/v1/agents", "127.0.0.1:8787", "https://attacker.example", "GET", 403},
		{"/api/v1/agents", "127.0.0.1:8787", "", "POST", 405},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Host = tc.host
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%+v: %d", tc, w.Code)
		}
		if w.Code == 200 && w.Header().Get("Content-Security-Policy") == "" {
			t.Fatal("missing content policy")
		}
	}
	for _, source := range []string{"http://other.example", "https://user:pass@example.com", "https://example.com/path", "https://example.com?token=secret"} {
		if ValidateSource(source) == nil {
			t.Fatal("unsafe source accepted", source)
		}
	}
	if ValidateSource("http://127.0.0.1:8788") != nil {
		t.Fatal("loopback tunnel rejected")
	}
}
