package coveragecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/meshcrawler/hrx/internal/codexlaunch"
	"github.com/meshcrawler/hrx/internal/codexrpc"
	"strings"
	"testing"
)

func TestWorkRenewalWaitsForDurabilityAndSemanticQuiescence(t *testing.T) {
	c, g, w, store := actualCoordinator(t)
	if _, err := c.renewWork(g); !errors.Is(err, errWorkNotReady) {
		t.Fatal("work admitted without baseline publication", err)
	}
	actualPair(t, c, g, w)
	if _, err := c.renewWork(g); !errors.Is(err, errWorkNotReady) {
		t.Fatal("unstaged candidate admitted work", err)
	}
	if err := c.pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-store.entered
	if _, err := c.renewWork(g); !errors.Is(err, errWorkNotReady) {
		t.Fatal("in-flight stage admitted work", err)
	}
	finishStage(t, c, store)
	if err := c.pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := c.renewWork(g)
	if err != nil || first.Through != 2 || first.Publication.Through != 2 {
		t.Fatal(first, err)
	}
	w.in <- []byte(`{"method":"thread/tokenUsage/updated","params":{}}`)
	f, err := g.receive(context.Background())
	if err != nil || c.accept(f) != nil {
		t.Fatal("ordinary frame not consumed", err)
	}
	second, err := c.renewWork(g)
	if err != nil || second.Through != 3 || second.Publication != first.Publication {
		t.Fatal("ordinary consumption changed durable publication", second, err)
	}
	w.in <- nativeItem(4, "item/started", "second-compaction").raw
	f, err = g.receive(context.Background())
	if err != nil || c.accept(f) != nil {
		t.Fatal(err)
	}
	if _, err := c.renewWork(g); !errors.Is(err, errWorkNotReady) {
		t.Fatal("open compaction admitted work", err)
	}
	w.in <- nativeItem(5, "item/completed", "second-compaction").raw
	f, err = g.receive(context.Background())
	if err != nil || c.accept(f) != nil {
		t.Fatal(err)
	}
	if _, err := c.renewWork(g); !errors.Is(err, errWorkNotReady) {
		t.Fatal("completed but unstaged compaction admitted work", err)
	}
}

func TestWorkRenewalRejectsUnconsumedGapAndWrongOwner(t *testing.T) {
	c, g, w, store := actualCoordinator(t)
	actualPair(t, c, g, w)
	if err := c.pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-store.entered
	finishStage(t, c, store)
	if err := c.pump(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.renewWork(&codexGate{client: g.client, binding: g.binding}); !errors.Is(err, errStageBinding) {
		t.Fatal("non-owner renewed work frontier", err)
	}
	id, _ := g.identity()
	if err := c.accept(frame{id, 4, responseFrame, []byte(`{"result":{}}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.renewWork(g); !errors.Is(err, errWorkNotReady) {
		t.Fatal("missing ordinal admitted work", err)
	}
}

func TestWorkOverridesRequireSameBaselineSettings(t *testing.T) {
	c, _, _, _ := actualCoordinator(t)
	req, _ := newWorkRequest(context.Background(), "turn/start", json.RawMessage(`{"threadId":"thread","model":"fixture"}`), 2048)
	if !errors.Is(c.validateWork(req), codexrpc.ErrRequest) {
		t.Fatal("accepted override without baseline")
	}
	c.settings = &codexlaunch.Native{Model: "fixture"}
	if err := c.validateWork(req); err != nil {
		t.Fatal("matching baseline rejected", err)
	}
	req, _ = newWorkRequest(context.Background(), "turn/start", json.RawMessage(`{"threadId":"thread","model":"different"}`), 2048)
	if !errors.Is(c.validateWork(req), codexrpc.ErrRequest) {
		t.Fatal("changed model accepted")
	}
}

func TestMetadataRefreshRejectsMalformedParameters(t *testing.T) {
	c, _, _, _ := actualCoordinator(t)
	for _, params := range []string{`[]`, `"text"`, `{"cwd":"a","cwd":"b"}`, `{`} {
		if !errors.Is(c.validateWork(&workRequest{method: "config/read", params: json.RawMessage(params)}), codexrpc.ErrRequest) {
			t.Fatal("accepted malformed metadata", params)
		}
	}
	for _, method := range []string{"config/value/write", "config/batchWrite", "thread/start", "thread/resume"} {
		if !errors.Is(c.validateWork(&workRequest{method: method, params: json.RawMessage(`{}`)}), codexrpc.ErrRequest) {
			t.Fatal("metadata scope allowed mutation", method)
		}
	}
}

func TestWorkScopeDiagnosticsNeverExposeValuesOrUnknownNames(t *testing.T) {
	c, _, _, _ := actualCoordinator(t)
	c.settings = &codexlaunch.Native{Model: "fixture"}
	for _, raw := range []string{`{"threadId":"thread","model":"secret-value"}`, `{"threadId":"thread","secret-field":"secret-value"}`} {
		err := c.validateWork(&workRequest{method: "turn/start", params: json.RawMessage(raw)})
		if !errors.Is(err, codexrpc.ErrRequest) || strings.Contains(err.Error(), "secret") {
			t.Fatal("unsafe diagnostic", err)
		}
	}
	err := c.validateWork(&workRequest{method: "turn/start", params: json.RawMessage(`{"threadId":"thread","model":"changed"}`)})
	if !strings.Contains(err.Error(), "baseline setting mismatch (model)") {
		t.Fatal("missing fixed diagnostic", err)
	}
}

func TestNativeHistoryPagesRemainThreadScoped(t *testing.T) {
	c, _, _, store := actualCoordinator(t)
	defer store.unblock()
	c.stream.lifecycles.thread = "thread"
	for _, method := range []string{"thread/turns/list", "thread/items/list"} {
		for _, params := range []string{`{"threadId":"thread","cursor":null,"limit":100,"sortDirection":"desc"}`, `{"threadId":"thread","cursor":"opaque-next-page","limit":1}`} {
			if err := c.validateWork(&workRequest{method: method, params: json.RawMessage(params)}); err != nil {
				t.Fatal(method, err)
			}
		}
		for _, params := range []string{`{"threadId":"other","limit":100}`, `{"threadId":"thread","limit":1001}`, `{"threadId":"thread","limit":0}`, `{"threadId":"thread","cursor":{"threadId":"other"}}`, `{"threadId":"thread","sortDirection":"bad"}`, `{"threadId":"thread","model":"override"}`} {
			if err := c.validateWork(&workRequest{method: method, params: json.RawMessage(params)}); !errors.Is(err, codexrpc.ErrRequest) {
				t.Fatal("accepted invalid history page", method, params, err)
			}
		}
	}
}
