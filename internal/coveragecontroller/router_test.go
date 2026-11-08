package coveragecontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestRouterAllSourcesUseOneAdmissionAndPrivateRequestIDs(t *testing.T) {
	c, g, w, s := actualCoordinator(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	router, err := newWorkRouter(g, 3, 2048)
	if err != nil {
		t.Fatal(err)
	}
	reports := make(chan serveReport, 1)
	go func() { reports <- c.serveWork(ctx, g, time.Second, time.Second, router.requests) }()
	sources := []workSource{terminalSource, boardSource, directMessageSource}
	for _, source := range sources {
		raw := []byte(`{"id":1,"method":"thread/queue/add","params":{"threadId":"thread","input":[],"clientUserMessageId":"early"}}`)
		reply, err := router.dispatch(ctx, source, raw)
		var result map[string]json.RawMessage
		if err != nil || json.Unmarshal(reply, &result) != nil || len(result["error"]) == 0 {
			t.Fatal("pre-baseline ingress admitted", source, string(reply), err)
		}
		select {
		case raw := <-w.out:
			t.Fatal("pre-baseline native write", string(raw))
		default:
		}
	}
	sendPair(w)
	<-s.entered
	s.unblock()
	waitCurrent(t, g)
	type outcome struct {
		source workSource
		raw    []byte
		err    error
	}
	results := make(chan outcome, 3)
	for _, source := range sources {
		go func(source workSource) {
			method := "thread/queue/add"
			if source == terminalSource {
				method = "turn/start"
			}
			raw := []byte(fmt.Sprintf(`{"id":1,"method":%q,"params":{"threadId":"thread","input":[],"clientUserMessageId":"source-%d"}}`, method, source))
			reply, err := router.dispatch(ctx, source, raw)
			results <- outcome{source, reply, err}
		}(source)
	}
	seen := map[string]bool{}
	for range sources {
		var req struct {
			ID     string
			Params struct {
				Tag string `json:"clientUserMessageId"`
			}
		}
		select {
		case raw := <-w.out:
			if err := json.Unmarshal(raw, &req); err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("router stalled")
		}
		if req.ID == "" || req.ID == "1" || seen[req.ID] {
			t.Fatal("external/duplicate native ID", req.ID)
		}
		seen[req.ID] = true
		raw, _ := json.Marshal(map[string]any{"id": req.ID, "result": map[string]string{"source": req.Params.Tag}})
		w.in <- raw
	}
	for range sources {
		out := <-results
		var reply struct {
			ID     int
			Result struct{ Source string }
		}
		if out.err != nil || json.Unmarshal(out.raw, &reply) != nil || reply.ID != 1 || reply.Result.Source != fmt.Sprintf("source-%d", out.source) {
			t.Fatal("crossed client responses", out, string(out.raw))
		}
	}
	if err := g.revoke(); err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		reply, err := router.dispatch(ctx, source, []byte(`{"id":"after","method":"thread/queue/add","params":{"threadId":"thread"}}`))
		var out map[string]json.RawMessage
		if err == nil && (json.Unmarshal(reply, &out) != nil || len(out["error"]) == 0) {
			t.Fatal("post-revocation success", string(reply))
		}
	}
	select {
	case raw := <-w.out:
		t.Fatal("post-revocation native write", string(raw))
	default:
	}
	cancel()
	report := serveResult(t, reports)
	if report.CleanupError != nil {
		t.Fatal(report)
	}
}
func TestRouterRejectsAmbiguousIDsAndSourceEscalation(t *testing.T) {
	g, _ := actualGate(t)
	r, _ := newWorkRouter(g, 1, 2048)
	for _, raw := range []string{
		`{"id":null,"method":"thread/read","params":{}}`,
		`{"id":true,"method":"thread/read","params":{}}`,
		`{"id":1.5,"method":"thread/read","params":{}}`,
		`{"id":1,"id":2,"method":"thread/read","params":{}}`,
		`{"id":1,"source":"terminal","method":"thread/read","params":{}}`,
		`{"id":1,"method":"turn/start","params":{"threadId":"thread"}}`,
	} {
		if _, err := r.dispatch(context.Background(), boardSource, []byte(raw)); err == nil {
			t.Fatal("accepted", raw)
		}
	}
}

func TestTerminalMetadataRefreshUsesAdmissionFence(t *testing.T) {
	c, g, w, store := actualCoordinator(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	router, err := newWorkRouter(g, 3, 2048)
	if err != nil {
		t.Fatal(err)
	}
	reports := make(chan serveReport, 1)
	go func() { reports <- c.serveWork(ctx, g, time.Second, time.Second, router.requests) }()
	raw := []byte(`{"id":"metadata","method":"model/list","params":{}}`)
	for _, source := range []workSource{boardSource, directMessageSource, terminalSource} {
		reply, err := router.dispatch(ctx, source, raw)
		var envelope map[string]json.RawMessage
		if err == nil && (json.Unmarshal(reply, &envelope) != nil || len(envelope["error"]) == 0) {
			t.Fatal("metadata admitted before baseline", source, string(reply))
		}
	}
	select {
	case out := <-w.out:
		t.Fatal("early native write", string(out))
	default:
	}
	sendPair(w)
	<-store.entered
	store.unblock()
	waitCurrent(t, g)
	for _, source := range []workSource{boardSource, directMessageSource} {
		if _, err := router.dispatch(ctx, source, raw); err == nil {
			t.Fatal("auxiliary metadata access", source)
		}
	}
	type outcome struct {
		reply []byte
		err   error
	}
	result := make(chan outcome, 1)
	go func() { reply, err := router.dispatch(ctx, terminalSource, raw); result <- outcome{reply, err} }()
	var request struct {
		ID     string
		Method string
	}
	select {
	case out := <-w.out:
		if json.Unmarshal(out, &request) != nil || request.Method != "model/list" || request.ID == "metadata" {
			t.Fatal("wrong native request", string(out))
		}
	case <-ctx.Done():
		t.Fatal("metadata stalled")
	}
	response, _ := json.Marshal(map[string]any{"id": request.ID, "result": map[string]any{"data": []any{}}})
	w.in <- response
	got := <-result
	var reply struct {
		ID     string
		Result json.RawMessage
		Error  json.RawMessage
	}
	if got.err != nil || json.Unmarshal(got.reply, &reply) != nil || reply.ID != "metadata" || len(reply.Result) == 0 || len(reply.Error) != 0 {
		t.Fatal("metadata reply", string(got.reply), got.err)
	}
	if err := g.revoke(); err != nil {
		t.Fatal(err)
	}
	after, err := router.dispatch(ctx, terminalSource, raw)
	var envelope map[string]json.RawMessage
	if err == nil && (json.Unmarshal(after, &envelope) != nil || len(envelope["error"]) == 0) {
		t.Fatal("metadata escaped revoked fence", string(after))
	}
	select {
	case out := <-w.out:
		t.Fatal("revoked native write", string(out))
	default:
	}
	cancel()
	report := serveResult(t, reports)
	if report.CleanupError != nil {
		t.Fatal(report)
	}
}
