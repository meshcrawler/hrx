package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAuthenticatedSourceRetainsStaleRowsWithoutExportingLocally(t *testing.T) {
	now := time.Now().UTC()
	unavailable := false
	c := &Collector{
		Local: func(context.Context) ([]Record, error) { return nil, nil },
		Fetchers: map[string]func(context.Context) (Snapshot, error){
			"hub": func(context.Context) (Snapshot, error) {
				if unavailable {
					return Snapshot{}, errors.New("SECRET transport details")
				}
				return Snapshot{Version: 1, GeneratedAt: now, Agents: []Row{
					{Record: Record{RunID: "older", HostID: "host-a", LastActivity: now.Add(-time.Hour)}, SourceStatus: "stale"},
					{Record: Record{RunID: "newer", HostID: "host-b", LastActivity: now}},
				}}, nil
			},
		},
	}
	c.Refresh(context.Background())
	read := func(path string) Snapshot {
		t.Helper()
		req := httptest.NewRequest("GET", "http://127.0.0.1"+path, nil)
		w := httptest.NewRecorder()
		c.Handler().ServeHTTP(w, req)
		var s Snapshot
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &s) != nil {
			t.Fatal(w.Body.String())
		}
		return s
	}
	s := read("/api/v1/agents")
	if len(s.Agents) != 2 || s.Agents[0].RunID != "newer" || s.Agents[0].Source != "hub" {
		t.Fatalf("bad pooled rows: %+v", s)
	}
	if s.Agents[1].SourceStatus != "stale" {
		t.Fatal("connected hub hid stale host")
	}
	if len(read("/api/v1/local").Agents) != 0 {
		t.Fatal("pooled observations leaked into local endpoint")
	}
	unavailable = true
	c.Refresh(context.Background())
	s = read("/api/v1/agents")
	if len(s.Agents) != 2 || s.Agents[0].SourceStatus != "unreachable" || !s.Agents[0].LastActivity.Equal(now) {
		t.Fatalf("outage fabricated freshness: %+v", s)
	}
	for _, source := range s.Sources {
		if source.ID == "hub" && source.Error != "Source unavailable or snapshot stale" {
			t.Fatal("transport error leaked")
		}
	}
}
