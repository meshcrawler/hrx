package hub

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/monitor"
)

func TestPitputProtocolRoundtrip(t *testing.T) {
	h := Host{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Prefixes: []string{"team/"}}
	now := time.Now().UTC()
	r := record(h, "b")
	r.StartedAt, r.LastSeen, r.LastActivity = now, now, now
	r.Pitput = monitor.NewPitputInfo(true, "team/dev/agent", []string{"dev", "ops"}, "joined")
	b := Batch{Version: Version, Sequence: 1, Records: []monitor.Record{r}}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Batch
	if err := json.Unmarshal(data, &decoded); err != nil || decoded.validate(h) != nil || !reflect.DeepEqual(decoded.Records[0].Pitput, r.Pitput) {
		t.Fatal("collector batch changed board metadata", err)
	}
	decoded.Records[0].Pitput.Boards[0].Membership = "active"
	if decoded.validate(h) != ErrInvalid {
		t.Fatal("unverified activity accepted")
	}
	decoded.Records[0].Pitput = nil
	if err := decoded.validate(h); err != nil {
		t.Fatal("older sender rejected", err)
	}
}

func TestPostgresPitputRoundtrip(t *testing.T) {
	s := testStore(t)
	h := testHost(t, s, "a")
	current, legacy := record(h, "b"), record(h, "c")
	current.Pitput = monitor.NewPitputInfo(true, current.Agent, []string{"dev", "ops"}, "joined")
	b := Batch{Version: Version, Sequence: 1, Records: []monitor.Record{current, legacy}}
	if err := s.Ingest(context.Background(), h, b); err != nil {
		t.Fatal(err)
	}
	got, err := s.Snapshot(context.Background())
	if err != nil || len(got.Agents) != 2 {
		t.Fatal("hub snapshot", err)
	}
	for _, row := range got.Agents {
		if row.RunID == current.RunID {
			if row.HostID != h.ID || !reflect.DeepEqual(row.Pitput, current.Pitput) {
				t.Fatal("hub lost board association")
			}
		} else if row.Pitput != nil {
			t.Fatal("legacy run acquired another run's boards")
		}
	}
}
