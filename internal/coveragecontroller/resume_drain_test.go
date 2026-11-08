package coveragecontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/codexlaunch"
	"github.com/meshcrawler/hrx/internal/coveragejournal"
)

type resumeHistoryStore struct {
	stageStore
	stages []coveragejournal.Stage
}

func (s resumeHistoryStore) History() []coveragejournal.Stage { return s.stages }

func resumeAdmission(status string) coveragejournal.Stage {
	sum := sha256.Sum256([]byte(recoveryText))
	raw, _ := json.Marshal(attentionOutcome{Protocol: attentionProtocol, Thread: "thread", Delivery: recoveryDelivery,
		Digest: hex.EncodeToString(sum[:]), Status: status, Attempt: 1})
	return coveragejournal.Stage{Payload: raw}
}

func testResumeDrain(t *testing.T) *resumeDrain {
	t.Helper()
	d, err := newResumeDrain(resumeHistoryStore{stages: []coveragejournal.Stage{resumeAdmission("admitted")}}, "thread")
	if err != nil {
		t.Fatal(err)
	}
	d.expected[recoveryDelivery], d.armed = true, true
	return d
}

func drainNotice(d *resumeDrain, method, params string) error {
	return d.notification(method, json.RawMessage(params))
}

func drainRead(status, text string, full bool) string {
	var history map[string]any
	_ = json.Unmarshal([]byte(recoveryHistory("thread", "full", text)), &history)
	thread := history["thread"].(map[string]any)
	turn := thread["turns"].([]any)[0].(map[string]any)
	turn["status"] = status
	if !full {
		turn["itemsView"], turn["items"] = "notLoaded", []any{}
	}
	var baseline map[string]any
	_ = json.Unmarshal([]byte(bootstrapRead), &baseline)
	base := baseline["thread"].(map[string]any)
	base["turns"], base["status"] = thread["turns"], map[string]any{"type": "idle"}
	if status == "inProgress" {
		base["status"] = map[string]any{"type": "active", "activeFlags": []any{}}
	}
	raw, _ := json.Marshal(baseline)
	return string(raw)
}

func foregroundHistory(raw, status, text string) string {
	var document map[string]any
	_ = json.Unmarshal([]byte(raw), &document)
	thread := document["thread"].(map[string]any)
	turn := map[string]any{"id": "stopped-foreground", "status": status, "itemsView": "full",
		"items": []any{map[string]any{"id": "foreground-user", "type": "userMessage", "clientId": nil,
			"content": []any{map[string]any{"type": "text", "text": text, "text_elements": []any{}}}}}}
	thread["turns"] = append([]any{turn}, thread["turns"].([]any)...)
	result, _ := json.Marshal(document)
	return string(result)
}

func stoppedForegroundHistory() string {
	return foregroundHistory(strings.Replace(bootstrapRead, `"turns":[]`, `"status":{"type":"notLoaded"},"turns":[]`, 1),
		"interrupted", "original busy input")
}

func TestResumeDrainPreexistingStoppedForegroundIsNotQueuedWork(t *testing.T) {
	d := testResumeDrain(t)
	d.armed = false
	if err := d.captureStoppedHistory(json.RawMessage(stoppedForegroundHistory())); err != nil {
		t.Fatal(err)
	}
	d.armed = true
	if err := drainNotice(d, "turn/started", `{"threadId":"thread","turn":{"id":"turn","status":"inProgress"}}`); err != nil {
		t.Fatal(err)
	}
	// Pinned thread_lifecycle.rs:1074-1090 retains old persisted InProgress
	// statuses while ANY turn makes the loaded thread active.
	active := foregroundHistory(drainRead("inProgress", recoveryText, true), "inProgress", "original busy input")
	if complete, err := testResumeDrain(t).history(json.RawMessage(active)); complete || err == nil || !strings.Contains(err.Error(), "reason=missing_client_identity observed=false active=true preexisting=false") {
		t.Fatal("source-faithful old status failed to reproduce original rejection", complete, err)
	}
	if complete, err := d.history(json.RawMessage(active)); complete || err != nil {
		t.Fatal("old foreground was mistaken for newly queued input", complete, err)
	}
	if err := drainNotice(d, "turn/completed", `{"threadId":"thread","turn":{"id":"turn","status":"completed"}}`); err != nil {
		t.Fatal(err)
	}
	stillStale := foregroundHistory(drainRead("completed", recoveryText, true), "inProgress", "original busy input")
	if complete, err := d.history(json.RawMessage(stillStale)); !complete || err != nil {
		t.Fatal(complete, err)
	}
	// History distinction grants no baseline authority: the unchanged strict
	// idle/resolved guard must still reject the stale InProgress status.
	native, err := codexlaunch.ParseThreadStart([]byte(bootstrapStart))
	if err != nil || native.ValidateResumeBaseline([]byte(stillStale), "fixture") == nil {
		t.Fatal("stale old turn bypassed strict baseline", err)
	}
	resolved := foregroundHistory(drainRead("completed", recoveryText, true), "interrupted", "original busy input")
	if complete, err := d.history(json.RawMessage(resolved)); !complete || err != nil {
		t.Fatal(complete, err)
	}
	if err := native.ValidateResumeBaseline([]byte(resolved), "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, claimed := d.turns["stopped-foreground"]; claimed {
		t.Fatal("historical foreground was claimed as current/completed work")
	}
}

func TestResumeDrainStoppedSnapshotRequiresFullBoundedUnambiguousHistory(t *testing.T) {
	for _, mode := range []string{"active", "unresolved", "summary", "missing-user-id", "duplicate-turn", "duplicate-item"} {
		t.Run(mode, func(t *testing.T) {
			var document map[string]any
			_ = json.Unmarshal([]byte(stoppedForegroundHistory()), &document)
			thread := document["thread"].(map[string]any)
			turns := thread["turns"].([]any)
			turn := turns[0].(map[string]any)
			switch mode {
			case "active":
				thread["status"] = map[string]any{"type": "active"}
			case "unresolved":
				turn["status"] = "inProgress"
			case "summary":
				turn["itemsView"] = "summary"
			case "missing-user-id":
				turn["items"].([]any)[0].(map[string]any)["id"] = ""
			case "duplicate-turn":
				thread["turns"] = append(turns, turns[0])
			case "duplicate-item":
				items := turn["items"].([]any)
				turn["items"] = append(items, items[0])
			}
			raw, _ := json.Marshal(document)
			d := testResumeDrain(t)
			d.armed = false
			if err := d.captureStoppedHistory(raw); err == nil || d.preexisting != nil {
				t.Fatal("invalid stopped history became usable witness", err)
			}
		})
	}
}

func TestResumeDrainInputFailureClassificationContainsNoPayload(t *testing.T) {
	for _, tc := range []struct{ content, reason string }{
		{`[{"type":"text","text":"private changed text","text_elements":[]}]`, "payload_digest"},
		{`[{"type":"image","url":"private image"}]`, "input_type"},
		{`[]`, "input_count"},
		{`[{"type":"text","text":"private changed text","text_elements":null}]`, "input_shape"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			d := testResumeDrain(t)
			d.turns["turn"] = false
			var document map[string]any
			_ = json.Unmarshal([]byte(drainRead("inProgress", recoveryText, true)), &document)
			item := document["thread"].(map[string]any)["turns"].([]any)[0].(map[string]any)["items"].([]any)[0].(map[string]any)
			item["content"] = json.RawMessage(tc.content)
			raw, _ := json.Marshal(document)
			if _, err := d.history(raw); err == nil || !strings.Contains(err.Error(), "reason="+tc.reason) || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), recoveryDelivery) {
				t.Fatal("missing or unsafe diagnostic", err)
			}
		})
	}
}

func TestResumeDrainPreexistingWitnessDoesNotAuthorizeChangedOrObservedWork(t *testing.T) {
	for _, mode := range []string{"unknown-new", "changed-text", "changed-user-id", "observed-old"} {
		t.Run(mode, func(t *testing.T) {
			d := testResumeDrain(t)
			d.armed = false
			if err := d.captureStoppedHistory(json.RawMessage(stoppedForegroundHistory())); err != nil {
				t.Fatal(err)
			}
			d.armed = true
			raw := foregroundHistory(drainRead("inProgress", recoveryText, true), "inProgress", "original busy input")
			switch mode {
			case "unknown-new":
				raw = strings.Replace(raw, "stopped-foreground", "unwitnessed-new", 1)
			case "changed-text":
				raw = strings.Replace(raw, "original busy input", "different private input", 1)
			case "changed-user-id":
				raw = strings.Replace(raw, "foreground-user", "different-user", 1)
			case "observed-old":
				if err := drainNotice(d, "turn/started", `{"threadId":"thread","turn":{"id":"stopped-foreground","status":"inProgress"}}`); err != nil {
					t.Fatal(err)
				}
			}
			if complete, err := d.history(json.RawMessage(raw)); complete || err == nil {
				t.Fatal("unproven work used historical exception", complete, err)
			} else {
				for _, secret := range []string{"original busy input", "different private input", "stopped-foreground", "foreground-user", recoveryDelivery} {
					if strings.Contains(err.Error(), secret) {
						t.Fatal("diagnostic exposed native identity/content")
					}
				}
				if mode == "observed-old" && !strings.Contains(err.Error(), "reason=missing_client_identity observed=true active=true preexisting=true unchanged=true") {
					t.Fatal("diagnostic omitted binding classification", err)
				}
			}
		})
	}
}

func TestResumeDrainLatestJournalEligibility(t *testing.T) {
	for _, status := range []string{"admitted", "submitting", "uncertain", "not_admitted", "prepared"} {
		t.Run(status, func(t *testing.T) {
			d, err := newResumeDrain(resumeHistoryStore{stages: []coveragejournal.Stage{resumeAdmission("admitted"), resumeAdmission(status)}}, "thread")
			if err != nil {
				t.Fatal(err)
			}
			want := status == "admitted" || status == "submitting" || status == "uncertain"
			if d.exactInput(recoveryDelivery, json.RawMessage(`[{"type":"text","text":"`+recoveryText+`"}]`)) != want {
				t.Fatal("stale outcome authorized input", status)
			}
		})
	}
}

func TestResumeDrainWitnessAndTransientIncompleteHistory(t *testing.T) {
	d := testResumeDrain(t)
	if err := drainNotice(d, "turn/started", `{"threadId":"thread","turn":{"id":"turn","status":"inProgress","items":[]}}`); err != nil {
		t.Fatal(err)
	}
	if done, err := d.history(json.RawMessage(drainRead("inProgress", recoveryText, false))); done || err != nil {
		t.Fatal("transient absent witness must quarantine, not admit or reject", done, err)
	}
	if done, err := d.history(json.RawMessage(drainRead("inProgress", recoveryText, true))); done || err != nil {
		t.Fatal("active exact witness must not admit", done, err)
	}
	if done, err := d.history(json.RawMessage(drainRead("completed", recoveryText, true))); done || err != nil {
		t.Fatal("completed history sealed before observed terminal notification", done, err)
	}
	if err := drainNotice(d, "turn/completed", `{"threadId":"thread","turn":{"id":"turn","status":"completed"}}`); err != nil {
		t.Fatal(err)
	}
	if done, err := d.history(json.RawMessage(drainRead("completed", recoveryText, true))); !done || err != nil {
		t.Fatal(done, err)
	}
	// Both notifications may predate listener attachment. The pre-resume queue
	// snapshot plus exact same-turn completed history still proves safe drain.
	if done, err := testResumeDrain(t).history(json.RawMessage(drainRead("completed", recoveryText, true))); !done || err != nil {
		t.Fatal("missed listener attachment lost positive durable witness", done, err)
	}
}

func TestResumeDrainRejectsUnboundOrUnsupportedActivity(t *testing.T) {
	for _, tc := range []struct{ method, params string }{
		{"turn/started", `{"threadId":"foreign","turn":{"id":"turn"}}`},
		{"turn/unknown", `{"threadId":"thread","turnId":"turn"}`},
		{"item/unknown", `{"threadId":"thread","turnId":"turn"}`},
		{"item/started", `{"threadId":"thread","turnId":"turn","item":{"id":"item","type":"contextCompaction"}}`},
		{"item/started", `{"threadId":"thread","turnId":"turn","item":{"type":"commandExecution"}}`},
		{"item/agentMessage/delta", `{"threadId":"thread","turnId":"turn"}`},
	} {
		t.Run(tc.method+tc.params, func(t *testing.T) {
			if err := drainNotice(testResumeDrain(t), tc.method, tc.params); err == nil {
				t.Fatal("unclassified activity accepted")
			}
		})
	}
	d := testResumeDrain(t)
	completed := `{"threadId":"thread","turn":{"id":"turn","status":"completed"}}`
	if err := drainNotice(d, "turn/completed", completed); err != nil {
		t.Fatal(err)
	}
	if err := drainNotice(d, "turn/completed", completed); err == nil {
		t.Fatal("duplicate terminal accepted")
	}
}

func TestResumeDrainRejectsMismatchedAndInterruptedWitness(t *testing.T) {
	for _, raw := range []string{
		drainRead("completed", "wrong text", true),
		strings.Replace(drainRead("completed", recoveryText, true), recoveryDelivery, "foreign", 1),
		drainRead("interrupted", recoveryText, true),
	} {
		d := testResumeDrain(t)
		d.turns["turn"] = false
		if done, err := d.history(json.RawMessage(raw)); done || err == nil {
			t.Fatal("unproven or unsupported continuation accepted", done, err)
		}
	}
	// queue_response crash after Started removed the queue entry: no snapshot
	// entry survives. Durable interrupted input must still fail closed.
	d := testResumeDrain(t)
	d.expected = map[string]bool{}
	if _, err := d.history(json.RawMessage(drainRead("interrupted", recoveryText, true))); err == nil || !strings.Contains(err.Error(), "unsupported native continuation") {
		t.Fatal("started-before-crash gap concealed", err)
	}
}

func TestResumeDrainQueueRejectsUnknownDuplicateAndChangedPayload(t *testing.T) {
	for name, data := range map[string]string{
		"unknown":            strings.Replace(recoveryQueued("queued", recoveryText), recoveryDelivery, "foreign", 1),
		"digest":             recoveryQueued("queued", "wrong text"),
		"duplicate delivery": recoveryQueued("one", recoveryText) + "," + recoveryQueued("two", recoveryText),
		"duplicate id":       recoveryQueued("one", recoveryText) + "," + recoveryQueued("one", recoveryText),
	} {
		t.Run(name, func(t *testing.T) {
			g, wire := actualGate(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			store := &attentionMemory{}
			h, err := newGateway(ctx, g, store, "fixture", time.Second, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer h.close(ctx)
			h.b.step, h.b.resumeThread, h.b.drain = 1, "thread", testResumeDrain(t)
			h.b.drain.expected, h.b.drain.armed = map[string]bool{}, false
			served := make(chan struct{})
			go func() {
				defer close(served)
				var req struct{ ID string }
				select {
				case raw := <-wire.out:
					_ = json.Unmarshal(raw, &req)
				case <-ctx.Done():
					return
				}
				raw, _ := json.Marshal(map[string]any{"id": req.ID, "result": json.RawMessage(`{"data":[` + data + `],"nextCursor":null}`)})
				wire.in <- raw
			}()
			if _, err := h.resumeQueue(ctx, true); err == nil {
				t.Fatal("invalid durable queue admitted")
			}
			<-served
		})
	}
}

func TestResumeDrainActivityBoundAndFreshGuard(t *testing.T) {
	d := testResumeDrain(t)
	for i := 0; i < attentionRecoveryItems; i++ {
		d.turns[fmt.Sprint(i)] = false
	}
	if err := drainNotice(d, "turn/started", `{"threadId":"thread","turn":{"id":"over-limit"}}`); err == nil {
		t.Fatal("turn quarantine exceeded bound")
	}
	b, err := newBootstrap(testConnection, 1, "fixture", 8, 8192, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.reduce(frame{kind: notificationFrame, raw: []byte(`{"method":"turn/started","params":{"threadId":"thread","turn":{"id":"turn"}}}`)}, ""); err == nil {
		t.Fatal("fresh activity guard weakened")
	}
}

// Actual codexrpc transport over channel-backed controllerWire only. No native
// process, socket, auxiliary bridge or inference is used by this regression.
func TestGatewayCombinedResumeDrainsBeforeDurableReady(t *testing.T) {
	for _, beforeResponse := range []bool{false, true} {
		for _, phase := range []string{"admitted", "submitting"} {
			t.Run(fmt.Sprintf("beforeResponse=%t/%s", beforeResponse, phase), func(t *testing.T) {
				g, wire := actualGate(t)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				store := fakeStore()
				h, err := newGateway(ctx, g, resumeHistoryStore{store, []coveragejournal.Stage{resumeAdmission("prepared"), resumeAdmission(phase)}}, "fixture", 3*time.Second, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				h.combinedResumeDrain, h.expectedThreadID = true, "thread"
				defer func() { store.unblock(); h.close(ctx) }()
				served := make(chan error, 1)
				go func() {
					reads, queueReads := 0, 0
					resumed := false
					for {
						var request struct{ ID, Method string }
						select {
						case raw := <-wire.out:
							if json.Unmarshal(raw, &request) != nil {
								served <- fmt.Errorf("invalid request")
								return
							}
						case <-ctx.Done():
							served <- ctx.Err()
							return
						}
						body := `{}`
						switch request.Method {
						case "initialize", "thread/name/set":
						case "initialized":
							continue
						case "thread/queue/list":
							body = `{"data":[],"nextCursor":null}`
							if queueReads == 0 {
								body = `{"data":[` + recoveryQueued("queued", recoveryText) + `],"nextCursor":null}`
							}
							queueReads++
						case "thread/resume":
							resumed = true
							body = bootstrapStart
							if beforeResponse {
								wire.in <- []byte(`{"method":"turn/started","params":{"threadId":"thread","turn":{"id":"turn","status":"inProgress"}}}`)
							}
						case "thread/read":
							if !resumed {
								body = stoppedForegroundHistory()
								break
							}
							reads++
							body = foregroundHistory(drainRead("completed", recoveryText, true), "interrupted", "original busy input")
							if reads == 1 {
								body = foregroundHistory(drainRead("inProgress", recoveryText, false), "inProgress", "original busy input")
							}
						default:
							served <- fmt.Errorf("unexpected native write: %s", request.Method)
							return
						}
						raw, _ := json.Marshal(map[string]any{"id": request.ID, "result": json.RawMessage(body)})
						wire.in <- raw
						if request.Method == "thread/resume" && !beforeResponse {
							wire.in <- []byte(`{"method":"turn/started","params":{"threadId":"thread","turn":{"id":"turn","status":"inProgress"}}}`)
						}
						if request.Method == "thread/read" && reads == 1 {
							wire.in <- []byte(`{"method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"completed"}}}`)
						}
						if reads == 2 {
							served <- nil
							return
						}
					}
				}()
				if _, err := h.handle(ctx, terminalSource, []byte(`{"id":1,"method":"initialize","params":{}}`)); err != nil {
					t.Fatal(err)
				}
				if _, err := h.handle(ctx, terminalSource, []byte(`{"method":"initialized"}`)); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() {
					_, err := h.handle(ctx, terminalSource, []byte(`{"id":2,"method":"thread/resume","params":{"threadId":"thread"}}`))
					done <- err
				}()
				select {
				case <-store.entered:
				case err := <-done:
					t.Fatal("drain failed before baseline", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				select {
				case <-h.ready:
					t.Fatal("Ready before baseline durability")
				default:
				}
				reply, refused := h.handle(ctx, boardSource, []byte(`{"id":3,"method":"thread/queue/add","params":{"threadId":"thread","input":[]}}`))
				var refusal struct{ Error json.RawMessage }
				if refused == nil && (json.Unmarshal(reply, &refusal) != nil || len(refusal.Error) == 0) {
					t.Fatal("auxiliary input admitted before baseline durability")
				}
				store.unblock()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if err := <-served; err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
