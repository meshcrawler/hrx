package monitor

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPitputMetadataLifecycleAndAggregation(t *testing.T) {
	ctx := context.Background()
	store := Store{Dir: filepath.Join(t.TempDir(), "monitor")}
	want := NewPitputInfo(true, "host/dev/agent", []string{"ops", "/b/dev", "ops", "../private"}, "joined")
	if len(want.Boards) != 2 || want.Boards[0].Name != "/b/dev" || !ValidatePitput(want) {
		t.Fatal("normalization failed", want)
	}
	r, err := store.Register(Record{Harness: "codex", Model: "fixture", SessionID: "first", Agent: "host/dev/agent", Pitput: want})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Activity(r.RunID, "session-start", "resumed"); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(r.RunID); err != nil {
		t.Fatal(err)
	}
	local := &Collector{Local: store.Snapshot}
	local.Refresh(ctx)
	server := httptest.NewServer(local.Handler())
	defer server.Close()
	pooled := &Collector{
		Local: func(context.Context) ([]Record, error) {
			return []Record{{RunID: "old", HostID: "other", Agent: r.Agent}}, nil
		},
		Peers: map[string]string{"remote": server.URL},
	}
	pooled.Refresh(ctx)
	response := httptest.NewRecorder()
	pooled.Handler().ServeHTTP(response, httptest.NewRequest("GET", "http://127.0.0.1/api/v1/agents", nil))
	var got Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil || len(got.Agents) != 2 {
		t.Fatal("pool roundtrip", err, response.Body.String())
	}
	for _, row := range got.Agents {
		if row.RunID == r.RunID {
			if row.HostID != r.HostID || row.Source != "remote" || row.SessionID != "resumed" || row.Status != "exited" || !reflect.DeepEqual(row.Pitput, want) {
				t.Fatalf("cross-session/host metadata corruption: %+v", row)
			}
		} else if row.Pitput != nil {
			t.Fatal("old observation acquired another run's boards")
		}
	}
}

func TestPitputValidationAndUnknownFields(t *testing.T) {
	for _, p := range []*PitputInfo{
		nil, NewPitputInfo(false, "", nil, "configured"),
		NewPitputInfo(true, "host/dev/agent", []string{"dev"}, "joined"),
	} {
		if !ValidatePitput(p) {
			t.Fatal("valid metadata rejected")
		}
	}
	for _, p := range []*PitputInfo{
		{Enabled: true, Boards: []PitputBoard{{Name: "/b/dev", Membership: "active"}}},
		{Enabled: true, Boards: []PitputBoard{{Name: "https://example.com", Membership: "joined"}}},
		{Enabled: true, Identity: strings.Repeat("x", 201)},
		{Enabled: false, Boards: []PitputBoard{{Name: "/b/dev", Membership: "joined"}}},
		{Enabled: true, Boards: make([]PitputBoard, 65)},
		{Enabled: true, Boards: []PitputBoard{{Name: "/b/dev", Membership: "joined"}, {Name: "/b/dev", Membership: "joined"}}},
	} {
		if ValidatePitput(p) {
			t.Fatal("invalid metadata accepted", p)
		}
	}
	var r Record
	decoder := json.NewDecoder(strings.NewReader(`{"pitput":{"enabled":true,"boards":[],"future_status":{"secret":"not exported"}}}`))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&r); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(r)
	if strings.Contains(string(data), "not exported") {
		t.Fatal("unrecognized private payload retained")
	}
}
