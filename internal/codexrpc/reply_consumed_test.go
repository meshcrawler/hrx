package codexrpc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestReplyAtConsumedRequiresReducedServerRequest(t *testing.T) {
	c, w, p := publishedClient(t)
	before, err := c.RenewConsumed(p, p.Through)
	if err != nil {
		t.Fatal(err)
	}
	w.in <- packet{raw: []byte(`{"id":91,"method":"item/commandExecution/requestApproval","params":{}}`)}
	request, err := c.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(`{"decision":"accept"}`)
	if err := c.ReplyAtConsumed(context.Background(), before, request, result, nil); !errors.Is(err, ErrPublicationRefused) {
		t.Fatal(err)
	}
	if len(w.out) != 0 {
		t.Fatal("unreduced approval reached native")
	}
	after, err := c.RenewConsumed(p, request.Ordinal)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ReplyAtConsumed(context.Background(), after, request, result, nil); err != nil {
		t.Fatal(err)
	}
	var reply struct {
		ID     int
		Result json.RawMessage
	}
	if json.Unmarshal(receive(t, w.out), &reply) != nil || reply.ID != 91 || string(reply.Result) != string(result) {
		t.Fatal(reply)
	}
	if err := c.Revoke(p.Binding); err != nil {
		t.Fatal(err)
	}
	if err := c.ReplyAtConsumed(context.Background(), after, request, result, nil); !errors.Is(err, ErrPublicationRefused) {
		t.Fatal(err)
	}
	if len(w.out) != 0 {
		t.Fatal("revoked approval reached native")
	}
}

func TestReplyAtConsumedCannotBypassWriterWaitRevocation(t *testing.T) {
	c, w, p := publishedClient(t)
	w.in <- packet{raw: []byte(`{"id":2,"method":"item/fileChange/requestApproval","params":{}}`)}
	request, err := c.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	frontier, err := c.RenewConsumed(p, request.Ordinal)
	if err != nil {
		t.Fatal(err)
	}
	<-c.writeToken
	done := make(chan error, 1)
	go func() {
		done <- c.ReplyAtConsumed(context.Background(), frontier, request, json.RawMessage(`{"decision":"accept"}`), nil)
	}()
	if err := c.Revoke(p.Binding); err != nil {
		t.Fatal(err)
	}
	c.writeToken <- struct{}{}
	if err := receive(t, done); !errors.Is(err, ErrPublicationRefused) {
		t.Fatal(err)
	}
	if len(w.out) != 0 {
		t.Fatal("approval crossed revoked writer fence")
	}
}
