package coveragecontroller

import (
	"context"
	"encoding/hex"
	"encoding/json"

	"github.com/meshcrawler/hrx/internal/coveragejournal"
)

type baselineEvidence struct {
	RequestHash string   `json:"request_hash,omitempty"`
	Schema      int      `json:"schema"`
	Kind        string   `json:"kind"`
	Thread      string   `json:"thread"`
	Session     string   `json:"session"`
	Name        string   `json:"name"`
	StartHash   [32]byte `json:"start_hash"`
	ReadHash    [32]byte `json:"read_hash"`
}

// newBaselineCoordinator transfers a complete startup prefix to the live reducer
// and starts the first historical stage. ctx must have a bounded stage deadline.
// The sole owner must have checked effective launch intent before calling this.
// No UI/queue release is provided; it must drive serve/pump while storage runs.
// Constructor errors leave transport teardown with the caller. A different gate
// revision is never revoked here, because it may belong to a successor owner.
func newBaselineCoordinator(ctx context.Context, g publicationGate, b *bootstrap, store stageStore, items, candidates int) (*coordinator, error) {
	if g == nil || b == nil || store == nil {
		return nil, errBootstrap
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return nil, errBootstrap
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, revision := g.identity()
	if id != b.prefix.connection || revision != b.revision {
		return nil, errStageBinding
	}
	r, err := b.seal()
	if err != nil {
		return nil, err
	}
	s, err := newStream(id, r.identity.ThreadID, b.prefix.maxMessages, b.prefix.maxBytes, b.prefix.maxFrame, items, candidates)
	if err != nil {
		return nil, err
	}
	// Preserve the exact consumed frontier, not a newly asserted starting offset.
	s.prefix = b.prefix
	s.events = b.events
	c, err := newCoordinator(g, s, store)
	if err != nil {
		return nil, err
	}
	settings := b.native
	c.settings = &settings
	s.settings = &settings
	evidence := baselineEvidence{Schema: 1, Kind: "fresh-baseline", Thread: r.identity.ThreadID, Session: r.identity.SessionID, Name: b.name, StartHash: r.startHash, ReadHash: r.readHash}
	if b.resumeThread != "" {
		evidence.Kind = "resume-baseline"
	}
	if b.requestHash != ([32]byte{}) {
		evidence.RequestHash = hex.EncodeToString(b.requestHash[:])
	}
	payload, err := json.Marshal(evidence)
	if err != nil {
		return c, c.fail(err)
	}
	stage := coveragejournal.Stage{Epoch: id.epoch, Connection: id.nonce, Generation: id.generation, Revision: r.revision, Operation: "1", Prefix: r.through, Payload: payload}
	if err := c.writer.begin(ctx, stage); err != nil {
		return c, c.fail(err)
	}
	c.serial, c.flight, c.flightContext = 1, &stage, ctx
	return c, nil
}
