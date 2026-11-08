package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/meshcrawler/hrx/internal/monitor"
)

func TestObservationBatchRespectsEncodedByteLimit(t *testing.T) {
	rows := make([]monitor.Record, MaxBatch+1)
	for i := range rows {
		rows[i] = monitor.Record{RunID: fmt.Sprintf("%032x", i+1), Model: strings.Repeat("&", 256)}
		rows[i].Credential.Scopes = make([]string, 32)
		for j := range rows[i].Credential.Scopes {
			rows[i].Credential.Scopes[j] = strings.Repeat("&", 64)
		}
	}
	remaining := rows
	sequence := int64(1)
	for len(remaining) > 0 {
		batch, err := observationBatch(sequence, remaining)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(batch)
		if len(data) > MaxRequestBytes || len(batch.Records) == 0 || len(batch.Records) > MaxBatch || batch.Sequence != sequence {
			t.Fatal("invalid batch boundary")
		}
		if !reflect.DeepEqual(batch.Records, remaining[:len(batch.Records)]) {
			t.Fatal("batch lost ordering or records")
		}
		remaining = remaining[len(batch.Records):]
		sequence++
	}
	if sequence <= 3 {
		t.Fatal("fixture did not exercise byte-based splitting")
	}
	if _, err := observationBatch(1, []monitor.Record{{Model: strings.Repeat("x", MaxRequestBytes)}}); err == nil {
		t.Fatal("oversized single record accepted")
	}
	if (Host{Prefixes: []string{"team/"}}).Allows("team/" + strings.Repeat("a", 256) + "/agent") {
		t.Fatal("unbounded identity accepted")
	}
}

func TestForwarderDoesNotPublishUntilPendingBatchIsDurable(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, strings.Repeat("a", 32))
	dir := t.TempDir()
	f := &Forwarder{Client: client, Dir: dir, state: outbox{Version: Version, NextSequence: 1, Sent: map[string]string{}, Pending: &Batch{Version: Version, Sequence: 1}}}
	// Make atomic replacement fail even when tests run as root. This models
	// a failed spool write without relying on filesystem permission semantics.
	path := filepath.Join(dir, "outbox.json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.deliver(context.Background()); err == nil {
		t.Fatal("spool failure ignored")
	}
	if requests != 0 || f.state.Pending == nil || f.state.NextSequence != 1 {
		t.Fatal("unpersisted batch was published or discarded")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := f.deliver(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || f.state.Pending != nil || f.state.NextSequence != 2 {
		t.Fatal("recovery did not deliver exactly once")
	}
	var saved outbox
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.NextSequence != 2 || saved.Pending != nil {
		t.Fatal("checkpoint not durable after recovery")
	}
}

func TestForwarderLostResponseRestart(t *testing.T) {
	ctx := context.Background()
	store := monitor.Store{Dir: filepath.Join(t.TempDir(), "monitor")}
	hostID, err := store.HostID()
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := store.Register(monitor.Record{Agent: "team/dev/agent", Harness: "codex", Model: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Register(monitor.Record{Agent: "other/dev/agent", Harness: "claude", Model: "fixture"}); err != nil {
		t.Fatal(err)
	}
	var batches []Batch
	var sequence int64
	lost := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/host":
			json.NewEncoder(w).Encode(Host{ID: hostID, Prefixes: []string{"team/"}, LastSequence: sequence})
		case "/api/v1/heartbeat":
			w.WriteHeader(200)
		case "/api/v1/observations":
			var b Batch
			if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
				t.Error(err)
			}
			batches = append(batches, b)
			sequence = b.Sequence
			if lost {
				lost = false
				http.Error(w, "lost response after commit", 503)
				return
			}
			w.WriteHeader(200)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f := &Forwarder{Client: client, Monitor: store, Dir: dir}
	if err = f.Open(ctx); err != nil {
		t.Fatal(err)
	}
	other := &Forwarder{Client: client, Monitor: store, Dir: dir}
	if err = other.Open(ctx); !errors.Is(err, ErrCollectorRunning) {
		t.Fatalf("lock: %v", err)
	}
	if err = f.Sync(ctx); err == nil {
		t.Fatal("expected lost response")
	}
	f.Close()
	if err = store.Finish(allowed.RunID); err != nil {
		t.Fatal(err)
	}
	f = &Forwarder{Client: client, Monitor: store, Dir: dir}
	if err = f.Open(ctx); err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = f.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 3 {
		t.Fatalf("got %d batches", len(batches))
	}
	if !reflect.DeepEqual(batches[0], batches[1]) {
		t.Fatal("retry changed the committed payload")
	}
	for _, b := range batches {
		if len(b.Records) != 1 || b.Records[0].RunID != allowed.RunID {
			t.Fatalf("unauthorized record forwarded: %+v", b)
		}
	}
	if batches[2].Sequence != 2 || batches[2].Records[0].Status != "exited" {
		t.Fatal("exit not forwarded after retry")
	}
	if f.state.Pending != nil || f.state.NextSequence != 3 {
		t.Fatal("checkpoint not advanced")
	}
	f.Close()
	// An old local checkpoint must not silently overwrite hub state.
	f.state.NextSequence = 1
	if err = f.save(); err != nil {
		t.Fatal(err)
	}
	if err = f.Open(ctx); err == nil {
		t.Fatal("accepted stale checkpoint")
	}
}
