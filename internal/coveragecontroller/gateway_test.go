package coveragecontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/meshcrawler/hrx/internal/codexlaunch"
	"testing"
	"time"
)

func gatewayHandle(t *testing.T, h *gateway, ctx context.Context, raw string) []byte {
	t.Helper()
	reply, err := h.handle(ctx, terminalSource, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return reply
}
func TestGatewayPreservesStartupAndHoldsReplyUntilDurable(t *testing.T) {
	g, w := actualGate(t)
	store := fakeStore()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := newGateway(ctx, g, store, "fixture", time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	h.initialDeveloperInstructions = "host init"
	h.profileDeveloperInstructions = codexlaunch.Optional[string]{Present: true, Value: "profile guidance"}
	defer func() { store.unblock(); h.close(context.Background()) }()
	const initializedRequest = `{"cwd":"/fixture","developerInstructions":"host init\n\nprofile guidance\n\nnative instructions","historyMode":"paginated"}`
	served := make(chan error, 1)
	go func() {
		err := bootstrapResponderParams(ctx, w, "profile", json.RawMessage(initializedRequest))
		if err != nil {
			w.Close()
		}
		served <- err
	}()
	gatewayHandle(t, h, ctx, `{"id":1,"method":"initialize","params":{"clientInfo":{"name":"native-tui","version":"1"},"capabilities":{"experimentalApi":true}}}`)
	gatewayHandle(t, h, ctx, `{"method":"initialized"}`)
	start := make(chan []byte, 1)
	fail := make(chan error, 1)
	go func() {
		reply, err := h.handle(ctx, terminalSource, []byte(`{"id":2,"method":"thread/start","params":{"cwd":"/fixture","historyMode":"paginated","developerInstructions":"native instructions"}}`))
		if err != nil {
			fail <- err
		} else {
			start <- reply
		}
	}()
	select {
	case stage := <-store.entered:
		var evidence baselineEvidence
		digest := sha256.Sum256([]byte(initializedRequest))
		if json.Unmarshal(stage.Payload, &evidence) != nil || evidence.RequestHash != hex.EncodeToString(digest[:]) {
			t.Fatal("durable baseline did not bind initialized request")
		}
	case err := <-fail:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("stage never started")
	}
	// Both bulletin sources are blocked by the same owner while the journal is stalled.
	for _, source := range []workSource{boardSource, directMessageSource} {
		reply, err := h.handle(ctx, source, []byte(`{"id":1,"method":"thread/queue/add","params":{"threadId":"thread","input":[],"clientUserMessageId":"pending"}}`))
		var value map[string]json.RawMessage
		if err != nil || json.Unmarshal(reply, &value) != nil || len(value["error"]) == 0 {
			t.Fatal("unstaged work released", string(reply), err)
		}
	}
	select {
	case reply := <-start:
		t.Fatal("startup escaped before durability", string(reply))
	default:
	}
	select {
	case raw := <-w.out:
		t.Fatal("unstaged native input", string(raw))
	default:
	}
	store.unblock()
	select {
	case reply := <-start:
		var value struct {
			ID     int
			Result json.RawMessage
		}
		if json.Unmarshal(reply, &value) != nil || value.ID != 2 || len(value.Result) == 0 {
			t.Fatal(string(reply))
		}
	case err := <-fail:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("startup not released")
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	event, err := h.events.next(ctx)
	if err != nil || event.kind != notificationFrame {
		t.Fatal("startup notification lost", err)
	}
	// Queue-client initialization stays local, without creating another native owner.
	if _, err := h.handle(ctx, boardSource, []byte(`{"id":1,"method":"initialize","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-w.out:
		t.Fatal("queue created another native handshake", string(raw))
	default:
	}
	report, err := h.close(ctx)
	if err != nil || !report.ReceiverJoined || !report.WriterJoined {
		t.Fatal(report, err)
	}
}
func TestGatewayRejectsPrematureQueueAndClosesBeforeStartup(t *testing.T) {
	g, w := actualGate(t)
	store := fakeStore()
	defer store.unblock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	h, err := newGateway(ctx, g, store, "fixture", time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.handle(ctx, boardSource, []byte(`{"id":1,"method":"initialize","params":{}}`)); !errors.Is(err, errWorkNotReady) {
		t.Fatal(err)
	}
	select {
	case raw := <-w.out:
		t.Fatal("queue took startup ownership", string(raw))
	default:
	}
	report, err := h.close(ctx)
	if err != nil || report.CleanupError != nil {
		t.Fatal(report, err)
	}
	if _, err := h.handle(ctx, terminalSource, []byte(`{"id":1,"method":"initialize","params":{}}`)); !errors.Is(err, errCoordinatorLost) {
		t.Fatal(err)
	}
}

func TestGatewayRejectsInvalidVersionBeforeNativeHandshake(t *testing.T) {
	g, w := actualGate(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	store := fakeStore()
	defer store.unblock()
	h, err := newGateway(ctx, g, store, "fixture", time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close(ctx)
	if _, err := h.handle(ctx, terminalSource, []byte(`{"jsonrpc":"1.0","id":1,"method":"initialize","params":{}}`)); err == nil {
		t.Fatal("invalid version accepted")
	}
	select {
	case raw := <-w.out:
		t.Fatal("invalid handshake reached native", string(raw))
	default:
	}
}

func TestGatewayResumesIdleHistoryOnlyAfterNewDurableBaseline(t *testing.T) {
	g, w := actualGate(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := fakeStore()
	h, err := newGateway(ctx, g, store, "fixture", time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	h.expectedThreadID = "thread"
	h.initialDeveloperInstructions = "fresh-only init must not replace saved instructions"
	h.profileDeveloperInstructions = codexlaunch.Optional[string]{Present: true, Value: "fresh profile must not reinitialize resume"}
	defer func() { store.unblock(); h.close(ctx) }()
	const resume = `{"threadId":"thread","excludeTurns":true,"developerInstructions":"retain native override"}`
	expected, _, err := preservedResume(json.RawMessage(resume), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- bootstrapResponderParams(ctx, w, "resume", expected) }()
	gatewayHandle(t, h, ctx, `{"id":1,"method":"initialize","params":{}}`)
	gatewayHandle(t, h, ctx, `{"method":"initialized"}`)
	done := make(chan error, 1)
	go func() {
		_, err := h.handle(ctx, terminalSource, []byte(`{"id":2,"method":"thread/resume","params":`+resume+`}`))
		done <- err
	}()
	select {
	case stage := <-store.entered:
		var evidence baselineEvidence
		if json.Unmarshal(stage.Payload, &evidence) != nil || evidence.Kind != "resume-baseline" || evidence.Thread != "thread" || evidence.RequestHash == "" {
			t.Fatal("resume evidence missing")
		}
	case err := <-done:
		t.Fatal("resume escaped baseline staging", err)
	case <-ctx.Done():
		t.Fatal("resume did not stage")
	}
	select {
	case err := <-done:
		t.Fatal("resume released before durability", err)
	default:
	}
	if _, ok := g.client.Current(); ok {
		t.Fatal("resume admitted before journal completion")
	}
	store.unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("resume never released")
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if _, ok := g.client.Current(); !ok {
		t.Fatal("durable resume never admitted")
	}
}

func TestGatewayServicesConnectionRequestDuringInitialize(t *testing.T) {
	g, native := actualGate(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	store := fakeStore()
	defer store.unblock()
	h, err := newGateway(ctx, g, store, "fixture", time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close(ctx)
	peer := newPeerWire()
	joined := make(chan error, 1)
	go func() { joined <- h.servePeer(ctx, terminalSource, peer) }()
	peer.in <- []byte(`{"id":1,"method":"initialize","params":{}}`)
	var call map[string]json.RawMessage
	select {
	case raw := <-native.out:
		if json.Unmarshal(raw, &call) != nil {
			t.Fatal(string(raw))
		}
	case <-ctx.Done():
		t.Fatal("initialize missing")
	}
	native.in <- []byte(`{"id":0,"method":"account/chatgptAuthTokens/refresh","params":{}}`)
	select {
	case raw := <-peer.out:
		var request map[string]json.RawMessage
		if json.Unmarshal(raw, &request) != nil || string(request["id"]) != "0" || string(request["method"]) != `"account/chatgptAuthTokens/refresh"` {
			t.Fatal(string(raw))
		}
	case <-ctx.Done():
		t.Fatal("startup connection request deadlocked")
	}
	response := []byte(`{"id":0,"result":{"accessToken":"fixture-only"}}`)
	if err := h.answer(ctx, boardSource, response); err == nil {
		t.Fatal("board answered startup account request")
	}
	peer.in <- response
	select {
	case raw := <-native.out:
		var reply map[string]json.RawMessage
		if json.Unmarshal(raw, &reply) != nil || string(reply["id"]) != "0" || reply["result"] == nil {
			t.Fatal("native startup answer lost")
		}
	case <-ctx.Done():
		t.Fatal("startup answer did not reach native")
	}
	raw, _ := json.Marshal(map[string]json.RawMessage{"id": call["id"], "result": json.RawMessage(`{}`)})
	native.in <- raw
	select {
	case raw := <-peer.out:
		var reply map[string]json.RawMessage
		if json.Unmarshal(raw, &reply) != nil || string(reply["id"]) != "1" {
			t.Fatal(string(raw))
		}
	case <-ctx.Done():
		t.Fatal("initialize never completed")
	}
	if _, ok := g.client.Current(); ok {
		t.Fatal("authentication response granted work admission")
	}
	peer.Close()
	select {
	case <-joined:
	case <-ctx.Done():
		t.Fatal("startup peer did not join")
	}
}

func TestSavedLaunchRejectsWrongThreadBeforeNativeWrite(t *testing.T) {
	for _, raw := range []string{
		`{"id":2,"method":"thread/start","params":{"historyMode":"paginated"}}`,
		`{"id":2,"method":"thread/resume","params":{"threadId":"different"}}`,
	} {
		g, w := actualGate(t)
		store := fakeStore()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		h, err := newGateway(ctx, g, store, "fixture", time.Second, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		// Model the already-completed connection handshake. The attempted thread
		// operation must be rejected locally without creating native history.
		h.initialized = true
		h.b.step = 1
		h.expectedThreadID = "saved-thread"
		_, err = h.handle(ctx, terminalSource, []byte(raw))
		if err == nil {
			t.Fatal("saved launch accepted different thread", raw)
		}
		select {
		case out := <-w.out:
			t.Fatal("wrong-thread operation reached native", string(out))
		default:
		}
		store.unblock()
		h.close(context.Background())
		cancel()
	}
}

func TestLaunchRecordModelBinding(t *testing.T) {
	h := &gateway{expectedModel: "expected", b: &bootstrap{native: codexlaunch.Native{Model: "different"}}}
	if h.validateLaunchBinding() == nil {
		t.Fatal("different launch model accepted")
	}
	h.b.native.Model = "expected"
	if err := h.validateLaunchBinding(); err != nil {
		t.Fatal(err)
	}
}
