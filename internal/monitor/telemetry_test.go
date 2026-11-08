package monitor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const nativeID = "01a15d6c-f4e0-7ae1-9163-d4d247c1bcac"

func codexLine(kind, payload string) string {
	return `{"timestamp":"2026-10-21T10:00:01Z","type":"` + kind + `","payload":` + payload + "}\n"
}
func usageLine(used, limit string) string {
	return codexLine("event_msg", `{"type":"token_count","info":{"total_token_usage":{"total_tokens":999999999},"last_token_usage":{"total_tokens":`+used+`},"model_context_window":`+limit+`}}`)
}
func fixtureTelemetry(t *testing.T, content string) (Record, string) {
	t.Helper()
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	path := filepath.Join(root, "sessions", "2026", "10", "21", "rollout-2026-10-21-"+nativeID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return Record{RunID: strings.Repeat("a", 32), SessionID: nativeID, Harness: "codex", Status: "running", StartedAt: time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC), Telemetry: &telemetryState{Root: root}}, path
}
func collectTest(r *Record) {
	budget := int64(16 * 1024 * 1024)
	collectTelemetry(context.Background(), r, &budget)
}
func sessionLine(id string) string {
	return codexLine("session_meta", `{"id":"`+id+`","cli_version":"0.163.0","base_instructions":"SECRET_TRANSCRIPT_CANARY"}`)
}
func appendTelemetry(t *testing.T, path, text string) {
	t.Helper()
	f, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if _, e = f.WriteString(text); e != nil {
		t.Fatal(e)
	}
}

func TestTelemetryCodexReplayResumeAndRedaction(t *testing.T) {
	compact := codexLine("compacted", `{"window_id":"window-1","message":"SECRET_TRANSCRIPT_CANARY"}`)
	r, path := fixtureTelemetry(t, sessionLine(nativeID)+usageLine("1000", "2000")+compact+compact+usageLine("300", "2000"))
	collectTest(&r)
	if r.Metrics.Context.Used != 300 || *r.Metrics.Context.Limit != 2000 || r.Metrics.Compactions.Count != 1 {
		t.Fatalf("wrong latest metrics: %+v", r.Metrics)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "SECRET") || strings.Contains(string(b), r.Telemetry.Root) || strings.Contains(string(b), "window-1") {
		t.Fatalf("private metadata escaped: %s", b)
	}
	// Persist/reload exactly as the monitor does, then replay and resume separately.
	b, _ = json.Marshal(diskRecord{Record: r, Telemetry: r.Telemetry})
	var disk diskRecord
	if e := json.Unmarshal(b, &disk); e != nil {
		t.Fatal(e)
	}
	r = disk.Record
	r.Telemetry = disk.Telemetry
	collectTest(&r)
	if r.Metrics.Compactions.Count != 1 {
		t.Fatal("restart duplicated compaction")
	}
	resume := r
	resume.RunID = strings.Repeat("b", 32)
	resume.Telemetry = &telemetryState{Root: r.Telemetry.Root}
	resume.StartedAt = r.StartedAt.Add(2 * time.Second)
	collectTest(&resume)
	if resume.Metrics.Context != nil || resume.Metrics.Compactions.Count != 1 {
		t.Fatal("resume reused predecessor context or lost count")
	}
	appendTelemetry(t, path, strings.ReplaceAll(usageLine("100", "4000"), "10:00:01Z", "10:00:03Z"))
	collectTest(&resume)
	if resume.Metrics.Context == nil || *resume.Metrics.Context.Limit != 4000 {
		t.Fatal("resume did not update denominator")
	}
}

func TestTelemetryCompactionAndModelInvalidate(t *testing.T) {
	r, path := fixtureTelemetry(t, sessionLine(nativeID)+codexLine("turn_context", `{"model":"old"}`)+usageLine("1000", "2000"))
	collectTest(&r)
	appendTelemetry(t, path, codexLine("turn_context", `{"model":"new"}`))
	collectTest(&r)
	if r.Metrics.Context != nil {
		t.Fatal("model switch retained denominator")
	}
	appendTelemetry(t, path, usageLine("500", "null"))
	collectTest(&r)
	if r.Metrics.Context == nil || r.Metrics.Context.Limit != nil {
		t.Fatal("missing limit fabricated percentage")
	}
	appendTelemetry(t, path, codexLine("compacted", `{"window_id":"window-1"}`))
	collectTest(&r)
	if r.Metrics.Context != nil || r.Metrics.Compactions.Count != 1 {
		t.Fatal("compaction retained stale estimate")
	}
}

func TestTelemetryCodexRepeatedUsageIsNotFreshInference(t *testing.T) {
	r, path := fixtureTelemetry(t, sessionLine(nativeID)+usageLine("1000", "2000"))
	collectTest(&r)
	original := r.Metrics.Context.At
	appendTelemetry(t, path, strings.ReplaceAll(usageLine("1000", "2000"), "10:00:01Z", "10:00:02Z"))
	collectTest(&r)
	if !r.Metrics.Context.At.Equal(original) {
		t.Fatal("status-only token event refreshed context age")
	}
	appendTelemetry(t, path, codexLine("compacted", `{"window_id":"window-new"}`)+strings.ReplaceAll(usageLine("1000", "2000"), "10:00:01Z", "10:00:03Z"))
	collectTest(&r)
	if r.Metrics.Context != nil {
		t.Fatal("duplicate usage restored pre-compaction context")
	}
	// Equal-sized responses are still distinct when native lifetime usage advances.
	appendTelemetry(t, path, strings.ReplaceAll(strings.ReplaceAll(usageLine("1000", "4000"), "999999999", "1000000999"), "10:00:01Z", "10:00:04Z"))
	collectTest(&r)
	if r.Metrics.Context == nil || *r.Metrics.Context.Limit != 4000 {
		t.Fatal("new inference with equal size was dropped")
	}
	resume := r
	resume.Telemetry = &telemetryState{Root: r.Telemetry.Root}
	resume.StartedAt = time.Date(2026, 10, 21, 10, 0, 5, 0, time.UTC)
	appendTelemetry(t, path, strings.ReplaceAll(strings.ReplaceAll(usageLine("1000", "4000"), "999999999", "1000000999"), "10:00:01Z", "10:00:06Z"))
	collectTest(&resume)
	if resume.Metrics.Context != nil {
		t.Fatal("resume reused status-only report of prior model's inference")
	}
}

func TestTelemetryCompletedRunFreezesAtCollectedEnd(t *testing.T) {
	r, path := fixtureTelemetry(t, sessionLine(nativeID)+usageLine("1000", "2000"))
	end := time.Date(2026, 10, 21, 10, 0, 2, 0, time.UTC)
	r.EndedAt = &end
	r.Status = "exited"
	collectTest(&r)
	before, _ := json.Marshal(r.Metrics)
	appendTelemetry(t, path, "corrupt resumed-session record\n"+strings.ReplaceAll(usageLine("10", "2000"), "10:00:01Z", "10:00:03Z"))
	collectTest(&r)
	after, _ := json.Marshal(r.Metrics)
	if string(before) != string(after) {
		t.Fatal("successor session changed completed run's metrics")
	}
}

func TestTelemetryInvalidatesChangedLimitAndMalformedNativeMetadata(t *testing.T) {
	for _, next := range []string{
		usageLine("1000", "4000"),
		codexLine("compacted", `{"window_id":123}`),
		codexLine("event_msg", `{"type":"token_count","info":{"last_token_usage":{"total_tokens":"broken"}}}`),
	} {
		r, path := fixtureTelemetry(t, sessionLine(nativeID)+usageLine("1000", "2000"))
		collectTest(&r)
		appendTelemetry(t, path, next)
		collectTest(&r)
		if r.Metrics.Context != nil {
			t.Fatal("native metadata change retained an unverified context/limit", next)
		}
	}
}

func TestTelemetryForkDoesNotCountInheritedCompactions(t *testing.T) {
	parent := strings.ReplaceAll(codexLine("compacted", `{"window_id":"parent-window"}`), "10:00:01Z", "09:59:59Z")
	child := codexLine("compacted", `{"window_id":"child-window"}`)
	r, _ := fixtureTelemetry(t, sessionLine(nativeID)+parent+child)
	collectTest(&r)
	if r.Metrics.Compactions.Count != 1 {
		t.Fatal("fork counted compaction predating its native session metadata")
	}
}

func TestTelemetryTruncationRotationAndIsolation(t *testing.T) {
	r, path := fixtureTelemetry(t, sessionLine(nativeID)+usageLine("10", "100"))
	collectTest(&r)
	partial := usageLine("20", "200")
	appendTelemetry(t, path, partial[:len(partial)-2])
	collectTest(&r)
	if r.Metrics.Context.Used != 10 || r.Metrics.Availability != "catching-up" {
		t.Fatal("incomplete final line used")
	}
	appendTelemetry(t, path, partial[len(partial)-2:])
	collectTest(&r)
	if r.Metrics.Context.Used != 20 {
		t.Fatal("completed append not read")
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(sessionLine("other-session")+usageLine("99", "100")), 0600); err != nil {
		t.Fatal(err)
	}
	collectTest(&r)
	if r.Metrics.Context != nil || r.Metrics.Compactions != nil {
		t.Fatal("foreign session contaminated metrics")
	}
	// Truncate in place: reset/rebind and report observed-only history.
	if err := os.WriteFile(path, []byte(sessionLine(nativeID)), 0600); err != nil {
		t.Fatal(err)
	}
	collectTest(&r)
	if r.Metrics.Context != nil || r.Metrics.Compactions.Count != 0 || r.Metrics.Compactions.Coverage != "observed" {
		t.Fatal("truncation reused old context")
	}
}

func TestTelemetryOversizedRecordsBounded(t *testing.T) {
	huge := `{"type":"response_item","payload":"` + strings.Repeat("x", 5*1024*1024) + "\"}\n"
	r, _ := fixtureTelemetry(t, sessionLine(nativeID)+huge+usageLine("42", "100"))
	collectTest(&r)
	if !r.Telemetry.Discarding {
		t.Fatal("oversized line did not yield with progress")
	}
	collectTest(&r)
	if r.Metrics.Context == nil || r.Metrics.Context.Used != 42 || r.Metrics.Availability != "partial" {
		t.Fatalf("did not recover after oversized record: %+v", r.Metrics)
	}
}

func TestTelemetryUnavailableAndMissingValues(t *testing.T) {
	for _, line := range []string{usageLine("null", "2000"), usageLine("-1", "2000"), usageLine("1000000000001", "2000")} {
		r, _ := fixtureTelemetry(t, sessionLine(nativeID)+line)
		collectTest(&r)
		if r.Metrics.Context != nil {
			t.Fatal("invalid usage accepted")
		}
	}
	r, _ := fixtureTelemetry(t, sessionLine(nativeID))
	r.Harness = "opencode"
	collectTest(&r)
	if r.Metrics.Availability != "unsupported" || r.Metrics.Compactions != nil {
		t.Fatal("unsupported isn't unknown")
	}
	r.Harness = "codex"
	r.Telemetry = nil
	collectTest(&r)
	if r.Metrics.Availability != "unavailable" {
		t.Fatal("missing source reported available")
	}
}

func TestTelemetrySourceAmbiguityAndSymlink(t *testing.T) {
	r, path := fixtureTelemetry(t, sessionLine(nativeID)+usageLine("10", "100"))
	other := filepath.Join(filepath.Dir(path), "rollout-duplicate-"+nativeID+".jsonl")
	if err := os.WriteFile(other, []byte(sessionLine(nativeID)+usageLine("99", "100")), 0600); err != nil {
		t.Fatal(err)
	}
	collectTest(&r)
	if r.Metrics.Context != nil || r.Metrics.Availability != "unavailable" {
		t.Fatal("ambiguous source guessed")
	}
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".target"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".target", path); err != nil {
		t.Fatal(err)
	}
	r.Telemetry.AttemptedAt = time.Time{}
	collectTest(&r)
	if r.Metrics.Context != nil {
		t.Fatal("symlink source followed")
	}
}

func TestTelemetrySnapshotToAPI(t *testing.T) {
	r, _ := fixtureTelemetry(t, sessionLine(nativeID)+usageLine("800", "2000"))
	store := Store{Dir: filepath.Join(t.TempDir(), "monitor")}
	root := r.Telemetry.Root
	r, err := store.Register(r)
	if err != nil {
		t.Fatal(err)
	}
	// Fix fixture clock independently of the actual process-observation clock.
	if err := store.update(r.RunID, func(row *Record) {
		row.StartedAt = time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
		row.Telemetry = &telemetryState{Root: root}
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Metrics == nil || rows[0].Metrics.Context == nil || rows[0].Metrics.Context.Used != 800 {
		t.Fatalf("snapshot did not collect: %+v", rows)
	}
	collector := &Collector{Local: store.Snapshot}
	collector.Refresh(context.Background())
	server := httptest.NewServer(collector.Handler())
	defer server.Close()
	response, err := http.Get(server.URL + "/api/v1/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatal("snapshot API failed", err, response.StatusCode)
	}
	var snap Snapshot
	if err := json.Unmarshal(payload, &snap); err != nil || len(snap.Agents) != 1 || snap.Agents[0].Metrics.Context.Used != 800 {
		t.Fatal("API lost native telemetry", err)
	}
	if strings.Contains(string(payload), root) || strings.Contains(string(payload), "SECRET") {
		t.Fatal("snapshot leaked source data")
	}
	if !ValidateMetrics(rows[0].Metrics, nativeID, "codex") {
		t.Fatal("valid metrics rejected")
	}
	rows[0].Metrics.Context.Used = -1
	if ValidateMetrics(rows[0].Metrics, nativeID, "codex") {
		t.Fatal("negative metric accepted")
	}
}

func TestTelemetryClaudeSessionIsolationAndCompletion(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	path := filepath.Join(root, "projects", "project", nativeID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","sessionId":"` + nativeID + `","timestamp":"2026-10-21T10:00:01Z","version":"2.1.306","message":{"model":"claude-test","usage":{"input_tokens":20,"cache_read_input_tokens":100,"cache_creation_input_tokens":30},"content":[{"text":"SECRET_TRANSCRIPT_CANARY"}]}}` + "\n"
	compact := `{"type":"system","subtype":"compact_boundary","sessionId":"` + nativeID + `","uuid":"completed-1","timestamp":"2026-10-21T10:00:02Z","compactMetadata":{"postTokens":40}}` + "\n"
	if err := os.WriteFile(path, []byte(line+strings.ReplaceAll(compact, nativeID, "other-session")+compact+compact), 0600); err != nil {
		t.Fatal(err)
	}
	r := Record{RunID: strings.Repeat("a", 32), SessionID: nativeID, Harness: "claude", Status: "running", StartedAt: time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC), Telemetry: &telemetryState{Root: root}}
	collectTest(&r)
	if r.Metrics.Context == nil || r.Metrics.Context.Used != 40 || r.Metrics.Context.Limit != nil || r.Metrics.Compactions.Count != 1 {
		t.Fatalf("Claude metrics wrong: %+v", r.Metrics)
	}
	appendTelemetry(t, path, strings.ReplaceAll(line, `"sessionId":`, `"isSidechain":true,"sessionId":`))
	collectTest(&r)
	if r.Metrics.Context.Used != 40 {
		t.Fatal("subagent contaminated parent")
	}
	appendTelemetry(t, path, strings.ReplaceAll(strings.ReplaceAll(compact, `"postTokens":40`, `"preTokens":100`), "completed-1", "completed-2"))
	collectTest(&r)
	if r.Metrics.Context != nil || r.Metrics.Compactions.Count != 2 {
		t.Fatal("unknown post-compaction retained context or duplicate counted")
	}
}
