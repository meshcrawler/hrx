package coveragecontroller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/codexrpc"
)

func TestApprovalBookRequiresReductionAndConsumesReplyOnce(t *testing.T) {
	g, w := actualGate(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	id, _ := g.identity()
	book := newApprovalBook(id.generation, 2)
	raw := []byte(`{"id":0,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread","turnId":"turn","itemId":"item"}}`)
	w.in <- raw
	message, err := g.client.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := book.capture(message); err != nil {
		t.Fatal(err)
	}
	reply := []byte(`{"id":0,"result":{"decision":"accept"}}`)
	if _, _, _, err := book.take(reply); err == nil {
		t.Fatal("approval escaped before reduction")
	}
	f := frame{id, message.Ordinal, serverRequestFrame, raw}
	if err := book.reduce(f, "foreign"); err == nil {
		t.Fatal("foreign thread request accepted")
	}
	if err := book.reduce(f, "thread"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"id":0,"method":"spoof","result":{}}`,
		`{"id":0,"result":{},"error":{}}`,
		`{"id":0,"jsonrpc":"1.0","result":{}}`,
		`{"id":0,"id":0,"result":{}}`,
	} {
		if _, _, _, err := book.take([]byte(bad)); err == nil {
			t.Fatal("invalid reply consumed correlation")
		}
	}
	original, result, remote, err := book.take(reply)
	if err != nil || original.Ordinal != message.Ordinal || string(result) != `{"decision":"accept"}` || remote != nil {
		t.Fatal(original, string(result), err)
	}
	// The stored message retains native transport ownership, not just its JSON ID.
	if err := g.client.Reply(ctx, original, result, nil); err != nil {
		t.Fatal(err)
	}
	var nativeReply map[string]json.RawMessage
	if json.Unmarshal(<-w.out, &nativeReply) != nil || string(nativeReply["id"]) != "0" {
		t.Fatal(nativeReply)
	}
	if _, _, _, err := book.take(reply); err == nil {
		t.Fatal("duplicate approval accepted")
	}
	if err := book.capture(message); err == nil {
		t.Fatal("recycled native ID accepted")
	}
}
func TestApprovalBookBoundsOutstandingAndRejectsForeignGeneration(t *testing.T) {
	book := newApprovalBook("generation", 1)
	message := codexrpc.Message{Kind: codexrpc.ServerRequest, Generation: "foreign", Ordinal: 1, Raw: json.RawMessage(`{"id":0}`)}
	if book.capture(message) == nil {
		t.Fatal("foreign generation accepted")
	}
	message.Generation = "generation"
	if err := book.capture(message); err != nil {
		t.Fatal(err)
	}
	message.Ordinal = 2
	message.Raw = json.RawMessage(`{"id":1}`)
	if book.capture(message) == nil {
		t.Fatal("outstanding request limit exceeded")
	}
}

func TestTerminalAnswersApprovalWhileNativeWorkWaits(t *testing.T) {
	g, native := actualGate(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := fakeStore()
	store.unblock()
	h, err := newGateway(ctx, g, store, "fixture", time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close(ctx)
	bootstrapDone := make(chan error, 1)
	go func() { bootstrapDone <- bootstrapResponder(ctx, native, "") }()
	gatewayHandle(t, h, ctx, `{"id":1,"method":"initialize","params":{}}`)
	gatewayHandle(t, h, ctx, `{"method":"initialized"}`)
	gatewayHandle(t, h, ctx, `{"id":2,"method":"thread/start","params":{"cwd":"/fixture","historyMode":"paginated"}}`)
	if err := <-bootstrapDone; err != nil {
		t.Fatal(err)
	}
	peer := newPeerWire()
	peerDone := make(chan error, 1)
	go func() { peerDone <- h.servePeer(ctx, terminalSource, peer) }()
	peer.in <- []byte(`{"id":42,"method":"turn/start","params":{"threadId":"thread","input":[]}}`)
	var call map[string]json.RawMessage
	select {
	case raw := <-native.out:
		if json.Unmarshal(raw, &call) != nil || string(call["method"]) != `"turn/start"` {
			t.Fatal(string(raw))
		}
	case <-ctx.Done():
		t.Fatal("work did not reach native")
	}
	native.in <- []byte(`{"id":0,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread","turnId":"turn","itemId":"item"}}`)
	for {
		select {
		case raw := <-peer.out:
			var envelope map[string]json.RawMessage
			if json.Unmarshal(raw, &envelope) != nil {
				t.Fatal(string(raw))
			}
			if string(envelope["method"]) == `"item/commandExecution/requestApproval"` {
				goto delivered
			}
		case <-ctx.Done():
			t.Fatal("approval blocked behind pending native RPC")
		}
	}
delivered:
	answer := []byte(`{"id":0,"result":{"decision":"accept"}}`)
	for _, source := range []workSource{boardSource, directMessageSource} {
		if err := h.answer(ctx, source, answer); err == nil {
			t.Fatal("auxiliary approved native work")
		}
	}
	peer.in <- answer
	select {
	case raw := <-native.out:
		var reply map[string]json.RawMessage
		if json.Unmarshal(raw, &reply) != nil || string(reply["id"]) != "0" || string(reply["result"]) != `{"decision":"accept"}` {
			t.Fatal(string(raw))
		}
	case <-ctx.Done():
		t.Fatal("native approval response deadlocked")
	}
	result, _ := json.Marshal(map[string]json.RawMessage{"id": call["id"], "result": json.RawMessage(`{}`)})
	native.in <- result
	select {
	case raw := <-peer.out:
		var reply map[string]json.RawMessage
		if json.Unmarshal(raw, &reply) != nil || string(reply["id"]) != "42" || reply["error"] != nil {
			t.Fatal(string(raw))
		}
	case <-ctx.Done():
		t.Fatal("work reply never released")
	}
	peer.in <- answer
	select {
	case <-peerDone:
	case <-ctx.Done():
		t.Fatal("replayed approval did not close terminal")
	}
	select {
	case raw := <-native.out:
		t.Fatal("replayed approval reached native", string(raw))
	default:
	}
}

func TestApprovalResolutionInvalidatesAlreadyClaimedReply(t *testing.T) {
	book := newApprovalBook("generation", 2)
	message := codexrpc.Message{Kind: codexrpc.ServerRequest, Generation: "generation", Ordinal: 1, Raw: json.RawMessage(`{"id":0,"method":"item/fileChange/requestApproval","params":{"threadId":"thread"}}`)}
	if err := book.capture(message); err != nil {
		t.Fatal(err)
	}
	identity := connectionID{generation: "generation"}
	if err := book.reduce(frame{identity, 1, serverRequestFrame, message.Raw}, "thread"); err != nil {
		t.Fatal(err)
	}
	claimed, _, _, err := book.take([]byte(`{"id":0,"result":{"decision":"accept"}}`))
	if err != nil || !book.live(claimed) {
		t.Fatal("reply was not live", err)
	}
	resolved := frame{identity, 2, notificationFrame, []byte(`{"method":"serverRequest/resolved","params":{"threadId":"thread","requestId":0}}`)}
	if err := book.resolve(resolved, "thread"); err != nil {
		t.Fatal(err)
	}
	if book.live(claimed) {
		t.Fatal("resolved approval remained writable")
	}
}

func TestApprovalBookConnectionRequestsRemainOneShot(t *testing.T) {
	for _, method := range []string{"account/chatgptAuthTokens/refresh", "attestation/generate", "currentTime/read"} {
		t.Run(method, func(t *testing.T) {
			book := newApprovalBook("generation", 1)
			raw, _ := json.Marshal(map[string]any{"id": 0, "method": method, "params": map[string]any{}})
			message := codexrpc.Message{Kind: codexrpc.ServerRequest, Generation: "generation", Ordinal: 1, Raw: raw}
			if err := book.capture(message); err != nil {
				t.Fatal(err)
			}
			identity := connectionID{generation: "generation"}
			if err := book.reduce(frame{identity, 1, serverRequestFrame, raw}, "thread"); err != nil {
				t.Fatal(err)
			}
			reply := []byte(`{"id":0,"result":{}}`)
			if _, _, _, err := book.take(reply); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := book.take(reply); err == nil {
				t.Fatal("connection request answered twice")
			}
		})
	}
}
