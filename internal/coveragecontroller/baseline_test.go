package coveragecontroller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func completeBootstrap(t *testing.T) *bootstrap {
	t.Helper()
	b, err := newBootstrap(testConnection, 1, "fixture", 8, 8192, 2048)
	if err != nil {
		t.Fatal(err)
	}
	frames, methods := bootstrapFrames()
	for i, f := range frames {
		if err := b.accept(f, methods[i]); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func TestBaselineStagesBeforePublicationAndRetainsReceiveFrontier(t *testing.T) {
	g := &fakePublicationGate{id: testConnection, revision: 1, lost: make(chan struct{})}
	s := fakeStore()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := newBaselineCoordinator(ctx, g, completeBootstrap(t), s, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.unblock(); c.stop(context.Background()) }()
	stage := <-s.entered
	var evidence baselineEvidence
	if json.Unmarshal(stage.Payload, &evidence) != nil || evidence.Kind != "fresh-baseline" || evidence.Thread != "thread" || evidence.Session != "session" || stage.Prefix != 5 || stage.Revision != 1 || stage.Operation != "1" {
		t.Fatal("wrong baseline evidence", stage, evidence)
	}
	feedPair(t, c, 6, "after-baseline")
	if err := c.pump(ctx); err != nil || len(g.published) != 0 || len(c.stream.candidates) != 1 {
		t.Fatal("storage blocked publication or receiving incorrectly", err)
	}
	finishStage(t, c, s)
	if err := c.pump(ctx); err != nil || len(g.published) != 1 || g.published[0] != 5 {
		t.Fatal("baseline did not publish first", err, g.published)
	}
	next := <-s.entered
	if next.Operation != "2" || next.Prefix != 7 {
		t.Fatal("baseline operation identity reused", next)
	}
}

func TestBaselineLossOrCancellationRejectsLateDurableStage(t *testing.T) {
	for _, mode := range []string{"loss", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			g := &fakePublicationGate{id: testConnection, revision: 1, lost: make(chan struct{})}
			s := fakeStore()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c, err := newBaselineCoordinator(ctx, g, completeBootstrap(t), s, 8, 2)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { s.unblock(); c.stop(context.Background()) }()
			<-s.entered
			if mode == "loss" {
				close(g.lost)
			} else {
				cancel()
			}
			if err := c.pump(ctx); err == nil || !g.revoked {
				t.Fatal("loss did not revoke before storage returned")
			}
			finishStage(t, c, s)
			if err := c.pump(ctx); err == nil || len(g.published) != 0 {
				t.Fatal("late baseline restored publication")
			}
		})
	}
}

func TestBaselineCannotRebindStartupToSuccessorRevision(t *testing.T) {
	b := completeBootstrap(t)
	g := &fakePublicationGate{id: testConnection, revision: 2, lost: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := newBaselineCoordinator(ctx, g, b, fakeStore(), 8, 2); !errors.Is(err, errStageBinding) || g.revoked {
		t.Fatal("old evidence rebound or successor revoked", err)
	}
	g.revision = 1
	if _, err := newBaselineCoordinator(context.Background(), g, b, fakeStore(), 8, 2); err == nil {
		t.Fatal("unbounded baseline write accepted")
	}
}

func TestBaselineCanceledOwnerCannotPublishReadyStage(t *testing.T) {
	g := &fakePublicationGate{id: testConnection, revision: 1, lost: make(chan struct{})}
	s := fakeStore()
	stageBudget, cancelStage := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStage()
	c, err := newBaselineCoordinator(stageBudget, g, completeBootstrap(t), s, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.unblock(); c.stop(context.Background()) }()
	<-s.entered
	finishStage(t, c, s)
	owner, cancelOwner := context.WithCancel(context.Background())
	cancelOwner()
	if err := c.pump(owner); !errors.Is(err, context.Canceled) || !g.revoked || len(g.published) != 0 {
		t.Fatal("canceled owner published ready baseline", err, g.published)
	}
}
