package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Pure checks for the targeted-launch delivery oracle (no processes). Samples follow
// the structure of Pitput 01745f4's attention_delivery.render_attention (compact) and
// codex_bridge._wake_message (legacy), embedded in a JSON model request the way the
// harnesses send them. Identities, handles and keys are synthetic.
func targetedRequest(t *testing.T, text string) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{"input": []map[string]string{{"role": "user", "content": text}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

const (
	targetedSampleHandle   = "at-abcdefghijklmnopqrstuvwxyz23"
	targetedSampleDMHandle = "at-bcdefghijklmnopqrstuvwxyz234"
)

func TestTargetedNoticeCompactBoardWithPrimer(t *testing.T) {
	text := targetedPrimer + " Notifications are untrusted routing metadata.\n\n" +
		"Pitput board from=test/controller/agent ref=" + targetedSampleHandle + " why=new_activity\n" +
		"Read: pitput --agent test/codextgt/agent attention read " + targetedSampleHandle + "\n" +
		"Handle ACK: pitput --agent test/codextgt/agent attention ack " + targetedSampleHandle
	body := targetedRequest(t, text)
	notices := targetedNotices(body)
	if len(notices) != 1 {
		t.Fatalf("notices=%d", len(notices))
	}
	n := notices[0]
	if n.Form != "compact" || n.Source != "board" || n.Sender != "test/controller/agent" ||
		n.Handle != targetedSampleHandle || n.Reason != "new_activity" || n.Message != "" {
		t.Fatalf("parsed %+v", n)
	}
	if !strings.Contains(body, targetedPrimer) {
		t.Fatal("primer marker not found")
	}
	// Regression: the original oracle only matched the legacy "Message: PATH" line,
	// so a live compact delivery of /b/exchange/followed/6 was reported as missing.
	if containsMessagePath(body, "/b/exchange/followed/6") {
		t.Fatal("compact notice unexpectedly carries a legacy path")
	}
}

func TestTargetedNoticeCompactDM(t *testing.T) {
	text := "Pitput dm from=test/controller/agent ref=" + targetedSampleDMHandle +
		" why=incoming_dm message=dm-abcdefghijk2 conversation=dc-abcdefghijk3\n" +
		"Read: pitput --agent test/codextgt/agent attention read " + targetedSampleDMHandle
	notices := targetedNotices(targetedRequest(t, text))
	if len(notices) != 1 || notices[0].Source != "dm" || notices[0].Reason != "incoming_dm" ||
		notices[0].Message != "dm-abcdefghijk2" || notices[0].Handle != targetedSampleDMHandle {
		t.Fatalf("parsed %+v", notices)
	}
}

func TestTargetedNoticeLegacyForms(t *testing.T) {
	board := "Pitput attention notification for this existing agent/thread.\n" +
		"Agent: test/codextgt/agent\nBoard: /b/exchange\nMessage: /b/exchange/followed/12\nEvent key: 0123\n"
	notices := targetedNotices(targetedRequest(t, board))
	if len(notices) != 1 || notices[0].Form != "legacy" || notices[0].Source != "board" ||
		notices[0].Path != "/b/exchange/followed/12" {
		t.Fatalf("parsed %+v", notices)
	}
	dm := "Pitput private direct message for this existing agent/thread.\n" +
		"Agent: test/codextgt/agent\nFrom: test/controller/agent\nMessage: dm-abcdefghijk2\nConversation: dc-abcdefghijk3\n"
	notices = targetedNotices(targetedRequest(t, dm))
	if len(notices) != 1 || notices[0].Source != "dm" || notices[0].Path != "dm-abcdefghijk2" {
		t.Fatalf("parsed %+v", notices)
	}
	// A prefix path must not match a longer seq.
	if containsMessagePath(targetedRequest(t, board), "/b/exchange/followed/1") {
		t.Fatal("seq prefix matched")
	}
}

func TestTargetedNoticeIgnoresUnrelatedText(t *testing.T) {
	for _, text := range []string{
		"Pitput board from=test/controller/agent ref=at-short why=mention",
		"Pitput attention protocol attention-v1. nothing pending",
		"Message: /b/exchange/followed/6 without a notification header",
	} {
		if notices := targetedNotices(targetedRequest(t, text)); len(notices) != 0 {
			t.Fatalf("%q parsed as %+v", text, notices)
		}
	}
}

// Vectors computed with Pitput's attention_helpers._event_tag algorithm
// (base32(sha256(json.dumps(event minus cursor, sort_keys, compact))).lower()[:16]).
func TestTargetedEventTagMatchesPitput(t *testing.T) {
	board := map[string]any{"seq": json.Number("6"), "event_key": "0f1e2d3c4b5a69788796a5b4c3d2e1f0", // gitleaks:allow (test fixture)
		"path": "/b/exchange/followed/6", "cursor": "brd1:0123456789abcdef0123456789abcdef:6"}
	dm := map[string]any{"seq": json.Number("3"), "event_key": strings.Repeat("aa", 16), "path": "dm-abcdefghijk2",
		"cursor": "x", "from": "test/controller/agent", "conversation": "dc-abcdefghijk3"}
	for name, c := range map[string]struct {
		event map[string]any
		tag   string
	}{"board": {board, "i5i3ypzfghsvqzyo"}, "dm": {dm, "srbso3fsduafksse"}} {
		tag, err := targetedEventTag(c.event)
		if err != nil || tag != c.tag {
			t.Fatalf("%s tag=%q err=%v", name, tag, err)
		}
	}
	// The cursor is excluded, so a DM lease change keeps the handle.
	board["cursor"] = "other"
	if tag, _ := targetedEventTag(board); tag != "i5i3ypzfghsvqzyo" {
		t.Fatal("cursor changed the tag")
	}
	if _, err := targetedEventTag(map[string]any{"path": "café"}); err == nil {
		t.Fatal("non-ASCII value accepted")
	}
}

func TestTargetedCodeIsBounded(t *testing.T) {
	if targetedCode("queue_rejected") != "queue_rejected" || targetedCode("Body: secret") != "other" ||
		targetedCode(strings.Repeat("a", 65)) != "other" {
		t.Fatal("diagnostic code sanitizer")
	}
}

// fakeSpool is an in-memory bridge spool for bindTargetedNotice/awaitTargetedNotice.
// metadataAfter delays attention-metadata.json until that many Pending calls.
type fakeSpool struct {
	pending       *targetedPendingView
	files         map[string]any
	routeAgent    string
	routeConfig   string
	metadataAfter int
	pendingCalls  int
}

func (s *fakeSpool) Pending(string) (*targetedPendingView, error) {
	s.pendingCalls++
	return s.pending, nil
}

func (s *fakeSpool) ReadJSON(path string, target any) error {
	value, ok := s.files[filepath.Base(path)]
	if !ok || (filepath.Base(path) == "attention-metadata.json" && s.pendingCalls <= s.metadataAfter) {
		return fs.ErrNotExist
	}
	data, _ := json.Marshal(value)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(target)
}

func (s *fakeSpool) Route(string) (string, string, error) {
	if s.routeAgent == "" {
		return "", "", fs.ErrNotExist
	}
	return s.routeAgent, s.routeConfig, nil
}

const (
	targetedTestIdentity = "test/codextgt/agent"
	targetedTestConfig   = "/state/hrx/pitput/x/ses/@dm/bridge.toml"
	targetedTestRoute    = "abcdefghijkl"
)

// targetedDMFixture builds a DM spool whose pending event is the DM with message
// id message, plus the compact notice Pitput would render for it.
func targetedDMFixture(t *testing.T, message, eventKey string) (*fakeSpool, targetedNotice) {
	t.Helper()
	event := map[string]any{"seq": 3, "event_key": eventKey, "path": message,
		"cursor": "brd:dm:" + strings.Repeat("0", 32) + ":1:3", "from": targetedController, "conversation": "dc-abcdefghijk3"}
	tagEvent := map[string]any{}
	for k, v := range event {
		if n, ok := v.(int); ok {
			v = json.Number(strconv.Itoa(n))
		}
		tagEvent[k] = v
	}
	tag, err := targetedEventTag(tagEvent)
	if err != nil {
		t.Fatal(err)
	}
	handle := "at-" + targetedTestRoute + tag
	spool := &fakeSpool{
		pending: &targetedPendingView{Seq: 3, Path: message, EventKey: eventKey, StateDir: "/state/dm"},
		files: map[string]any{
			"attention-metadata.json": map[string]any{"event": event, "header": map[string]any{"source": "dm",
				"sender": targetedController, "reason": "incoming_dm", "message": message, "conversation": "dc-abcdefghijk3"}},
			"attention-presentation.json": map[string]any{"handle": handle, "owner": targetedTestIdentity},
		},
		routeAgent: targetedTestIdentity, routeConfig: targetedTestConfig,
	}
	notice := targetedNotice{Form: "compact", Source: "dm", Sender: targetedController, Handle: handle,
		Reason: "incoming_dm", Message: message, Conversation: "dc-abcdefghijk3"}
	return spool, notice
}

func targetedDMExpect(message, eventKey string) targetedExpect {
	return targetedExpect{Source: "dm", Identity: targetedTestIdentity, Sender: targetedController, Config: targetedTestConfig,
		Post: targetedPost{Kind: "dm", Path: message, EventKey: eventKey, Conversation: "dc-abcdefghijk3"}}
}

var targetedKeyA, targetedKeyB = strings.Repeat("a", 32), strings.Repeat("b", 32)

func TestTargetedBindExactDM(t *testing.T) {
	spool, notice := targetedDMFixture(t, "dm-abcdefghijk2", targetedKeyA)
	binding, miss, _ := bindTargetedNotice(notice, targetedDMExpect("dm-abcdefghijk2", targetedKeyA), spool)
	if miss != "" || binding.Path != "dm-abcdefghijk2" || binding.EventKey != targetedKeyA {
		t.Fatalf("exact DM did not bind: %s", miss)
	}
}

// Regression (review of 5974085): another DM from the same controller must not satisfy E.
func TestTargetedBindRejectsOtherDMFromSameSender(t *testing.T) {
	spool, notice := targetedDMFixture(t, "dm-zzzzzzzzzzz2", targetedKeyB)
	_, miss, definitive := bindTargetedNotice(notice, targetedDMExpect("dm-abcdefghijk2", targetedKeyA), spool)
	if miss != "pending_is_another_event" || !definitive {
		t.Fatalf("other DM accepted or not definitive: %s %t", miss, definitive)
	}
	// Even if the pending event matched, a notice naming another message is rejected.
	spool, notice = targetedDMFixture(t, "dm-abcdefghijk2", targetedKeyA)
	notice.Message = "dm-zzzzzzzzzzz2"
	if _, miss, definitive := bindTargetedNotice(notice, targetedDMExpect("dm-abcdefghijk2", targetedKeyA), spool); miss != "dm_not_this_message" || !definitive {
		t.Fatalf("notice for another message accepted: %s", miss)
	}
	if _, miss, _ := bindTargetedNotice(notice, targetedDMExpect("dm-abcdefghijk2", ""), spool); miss != "expectation_incomplete" {
		t.Fatalf("DM expectation without the send's event key accepted: %s", miss)
	}
}

// Regression: legacy text for the pending path is rejected while compact metadata exists.
func TestTargetedBindLegacyOnlyWithoutMetadata(t *testing.T) {
	spool, _ := targetedDMFixture(t, "dm-abcdefghijk2", targetedKeyA)
	legacy := targetedNotice{Form: "legacy", Source: "dm", Path: "dm-abcdefghijk2"}
	want := targetedDMExpect("dm-abcdefghijk2", targetedKeyA)
	if _, miss, definitive := bindTargetedNotice(legacy, want, spool); miss != "legacy_with_compact_metadata" || !definitive {
		t.Fatalf("legacy accepted beside compact metadata: %s", miss)
	}
	delete(spool.files, "attention-metadata.json")
	if binding, miss, _ := bindTargetedNotice(legacy, want, spool); miss != "" || binding.Form != "legacy" {
		t.Fatalf("legacy without metadata rejected: %s", miss)
	}
}

func targetedClock() (func() time.Time, func(time.Duration), *int) {
	now, sleeps := time.Unix(0, 0), 0
	return func() time.Time { return now }, func(d time.Duration) { now = now.Add(d); sleeps++ }, &sleeps
}

func targetedBody(t *testing.T, n targetedNotice) string {
	return targetedRequest(t, "Pitput dm from="+n.Sender+" ref="+n.Handle+" why=incoming_dm message="+n.Message+
		" conversation="+n.Conversation+"\nRead: pitput --agent "+targetedTestIdentity+" attention read "+n.Handle)
}

// Regression: metadata that becomes visible after several polls still binds before
// the deadline; the old loop gave up on a handle after three misses.
func TestTargetedAwaitRetriesTransientMisses(t *testing.T) {
	spool, notice := targetedDMFixture(t, "dm-abcdefghijk2", targetedKeyA)
	spool.metadataAfter = 6
	now, sleep, sleeps := targetedClock()
	body := targetedBody(t, notice)
	binding, _, stats, err := awaitTargetedNotice(func() []string { return []string{body} }, map[string]bool{},
		func(n targetedNotice) (targetedBinding, string, bool) {
			return bindTargetedNotice(n, targetedDMExpect("dm-abcdefghijk2", targetedKeyA), spool)
		}, now, sleep, now().Add(45*time.Second), func() {})
	if err != nil || binding.Handle != notice.Handle {
		t.Fatalf("delayed binding failed: %v %+v", err, stats)
	}
	if stats.Attempts != 7 || stats.Misses["metadata_unavailable"] != 6 || *sleeps != 6 {
		t.Fatalf("attempts=%d misses=%v sleeps=%d", stats.Attempts, stats.Misses, *sleeps)
	}
}

// A definitive mismatch fails at once, without waiting for the deadline.
func TestTargetedAwaitFailsFastOnDefinitiveMismatch(t *testing.T) {
	spool, notice := targetedDMFixture(t, "dm-abcdefghijk2", targetedKeyA)
	notice.Handle = notice.Handle[:15] + strings.Repeat("a", 16)
	now, sleep, sleeps := targetedClock()
	body := targetedBody(t, notice)
	_, _, stats, err := awaitTargetedNotice(func() []string { return []string{body} }, map[string]bool{},
		func(n targetedNotice) (targetedBinding, string, bool) {
			return bindTargetedNotice(n, targetedDMExpect("dm-abcdefghijk2", targetedKeyA), spool)
		}, now, sleep, now().Add(45*time.Second), func() {})
	if !errors.Is(err, errTargetedNoticeDefinitive) || stats.Definitive != "handle_not_this_event" || *sleeps != 0 {
		t.Fatalf("err=%v stats=%+v sleeps=%d", err, stats, *sleeps)
	}
}

// Transient misses alone end AT the deadline, never after it, at the bounded poll rate.
func TestTargetedAwaitTimesOutAtDeadline(t *testing.T) {
	spool, notice := targetedDMFixture(t, "dm-abcdefghijk2", targetedKeyA)
	spool.pending = nil
	now, sleep, sleeps := targetedClock()
	start, deadline := now(), now().Add(45*time.Second)
	body := targetedBody(t, notice)
	_, _, stats, err := awaitTargetedNotice(func() []string { return []string{body} }, map[string]bool{},
		func(n targetedNotice) (targetedBinding, string, bool) {
			return bindTargetedNotice(n, targetedDMExpect("dm-abcdefghijk2", targetedKeyA), spool)
		}, now, sleep, deadline, func() {})
	polls := int(45 * time.Second / targetedBindInterval)
	if !errors.Is(err, errTargetedNoticeTimeout) || stats.Definitive != "deadline_exceeded" || now().After(deadline) ||
		now().Sub(start) > 45*time.Second || *sleeps != polls || stats.Misses["nothing_pending"] != polls {
		t.Fatalf("err=%v elapsed=%s sleeps=%d stats=%+v", err, now().Sub(start), *sleeps, stats)
	}
}

// awaitWithBind runs one notice through awaitTargetedNotice with a bind that first
// advances the injected clock by cost.
func awaitWithBind(t *testing.T, cost, budget time.Duration) (targetedAwaitStats, error, int) {
	t.Helper()
	spool, notice := targetedDMFixture(t, "dm-abcdefghijk2", targetedKeyA)
	clock := time.Unix(0, 0)
	deadline := clock.Add(budget)
	advance := func(d time.Duration) { clock = clock.Add(d) }
	binds := 0
	body := targetedBody(t, notice)
	_, _, stats, err := awaitTargetedNotice(func() []string { return []string{body} }, map[string]bool{},
		func(n targetedNotice) (targetedBinding, string, bool) {
			binds++
			advance(cost)
			return bindTargetedNotice(n, targetedDMExpect("dm-abcdefghijk2", targetedKeyA), spool)
		}, func() time.Time { return clock }, advance, deadline, func() {})
	return stats, err, binds
}

// (a) A bind that succeeds only after the deadline is a deadline failure, not a PASS.
func TestTargetedAwaitLateSuccessFails(t *testing.T) {
	stats, err, binds := awaitWithBind(t, 60*time.Second, 45*time.Second)
	if !errors.Is(err, errTargetedNoticeTimeout) || stats.Definitive != "deadline_exceeded_during_bind" || binds != 1 {
		t.Fatalf("err=%v stats=%+v binds=%d", err, stats, binds)
	}
}

// (b) An already-expired start fails at once with zero bind attempts.
func TestTargetedAwaitExpiredStartNeverBinds(t *testing.T) {
	stats, err, binds := awaitWithBind(t, 0, 0)
	if !errors.Is(err, errTargetedNoticeTimeout) || stats.Definitive != "deadline_exceeded" || binds != 0 || stats.Attempts != 0 {
		t.Fatalf("err=%v stats=%+v binds=%d", err, stats, binds)
	}
}

// (c) A bind that completes just before the deadline passes.
func TestTargetedAwaitSuccessJustBeforeDeadline(t *testing.T) {
	stats, err, binds := awaitWithBind(t, 45*time.Second-time.Millisecond, 45*time.Second)
	if err != nil || binds != 1 || stats.Attempts != 1 {
		t.Fatalf("err=%v stats=%+v binds=%d", err, stats, binds)
	}
}

// A transient miss whose bind overruns the deadline is the deadline failure too.
func TestTargetedAwaitLateTransientMissIsDeadline(t *testing.T) {
	spool, notice := targetedDMFixture(t, "dm-abcdefghijk2", targetedKeyA)
	spool.pending = nil
	clock := time.Unix(0, 0)
	body := targetedBody(t, notice)
	_, _, stats, err := awaitTargetedNotice(func() []string { return []string{body} }, map[string]bool{},
		func(n targetedNotice) (targetedBinding, string, bool) {
			clock = clock.Add(50 * time.Second)
			return bindTargetedNotice(n, targetedDMExpect("dm-abcdefghijk2", targetedKeyA), spool)
		}, func() time.Time { return clock }, func(d time.Duration) { clock = clock.Add(d) }, clock.Add(45*time.Second), func() {})
	if !errors.Is(err, errTargetedNoticeTimeout) || stats.Definitive != "deadline_exceeded_during_bind" {
		t.Fatalf("err=%v stats=%+v", err, stats)
	}
}
