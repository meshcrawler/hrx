package coveragecontroller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/meshcrawler/hrx/internal/codexlaunch"
	"github.com/meshcrawler/hrx/internal/coveragejournal"
)

var (
	errCoordinatorLost = errors.New("coverage publication connection lost")
	errStageBinding    = errors.New("coverage stage result binding mismatch")
)

// publicationGate is bound to one admitted revision by its adapter. Native
// baseline admission and the codexrpc adapter are not implemented here. Publish
// must check that exact binding under the same lock as transport invalidation;
// an implementation using Snapshot followed by a write cannot satisfy this API.
type publicationGate interface {
	identity() (connectionID, uint64)
	done() <-chan struct{}
	publish(uint64, [32]byte) error
	revoke() error
}

type pairEvidence struct {
	Schema      int    `json:"schema"`
	Thread      string `json:"thread"`
	Turn        string `json:"turn"`
	Item        string `json:"item"`
	Start       uint64 `json:"start_ordinal"`
	End         uint64 `json:"completed_ordinal"`
	StartedMS   int64  `json:"started_at_ms"`
	CompletedMS int64  `json:"completed_at_ms"`
}

// coordinator is single-owner. It combines receive/reduction and a separate
// disk writer, but never owns the gate mutex or calls storage from that gate.
// Its caller must keep driving pump while idle and route every receive/Call
// failure to fail. These ownership obligations await the native run-loop adapter.
type coordinator struct {
	settings           *codexlaunch.Native
	gate               publicationGate
	stream             *stream
	writer             *stageWriter
	connection         connectionID
	revision, serial   uint64
	flight             *coveragejournal.Stage
	flightContext      context.Context
	cause, revokeError error
}

func newCoordinator(g publicationGate, s *stream, store stageStore) (*coordinator, error) {
	if g == nil || s == nil || store == nil {
		return nil, errStageBinding
	}
	id, revision := g.identity()
	if revision == 0 || id != s.prefix.connection || s.cause != nil {
		return nil, errStageBinding
	}
	select {
	case <-g.done():
		return nil, errCoordinatorLost
	default:
	}
	return &coordinator{gate: g, stream: s, writer: newStageWriter(store), connection: id, revision: revision}, nil
}

func (c *coordinator) fail(err error) error {
	if c.cause == nil {
		c.cause = err
		// Authority is synchronously revoked before clearing state or waiting on
		// any worker. A stuck journal cannot postpone this call.
		c.revokeError = c.gate.revoke()
		c.stream.invalidate(err)
		c.writer.halt()
	}
	return c.cause
}

func (c *coordinator) check() error {
	if c.cause != nil {
		return c.cause
	}
	select {
	case <-c.gate.done():
		return c.fail(errCoordinatorLost)
	default:
	}
	if c.flightContext != nil {
		if err := c.flightContext.Err(); err != nil {
			return c.fail(err)
		}
	}
	return nil
}

func (c *coordinator) accept(f frame) error {
	if err := c.check(); err != nil {
		return err
	}
	if err := c.stream.accept(f); err != nil {
		return c.fail(err)
	}
	return nil
}

func stageDigest(s coveragejournal.Stage) [32]byte { b, _ := json.Marshal(s); return sha256.Sum256(b) }

// pump never blocks on storage. ctx is the budget for a newly launched stage;
// that context remains attached to the flight on later calls. A successful
// return does not mean current coverage: even a publication can be revoked
// immediately afterwards. Only the gate's own current read can sample authority.
func (c *coordinator) pump(ctx context.Context) error {
	if err := c.check(); err != nil {
		return err
	}
	// Bootstrap staging may carry a deadline independent of the receive owner.
	// A canceled owner must not publish an already-ready durable result.
	if err := ctx.Err(); err != nil {
		return c.fail(err)
	}
	if result, ok := c.writer.poll(); ok {
		if result.err != nil {
			return c.fail(result.err)
		}
		if c.flight == nil || result.durability != coveragejournal.Durable || result.digest != stageDigest(*c.flight) || stageDigest(result.stage) != result.digest {
			return c.fail(errStageBinding)
		}
		if err := c.check(); err != nil {
			return err
		}
		if err := c.gate.publish(c.flight.Prefix, result.digest); err != nil {
			return c.fail(err)
		}
		c.flight, c.flightContext = nil, nil
	}
	if c.flight != nil || len(c.stream.candidates) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return c.fail(err)
	}
	if c.serial == ^uint64(0) {
		return c.fail(errStageBinding)
	}
	pair := c.stream.candidates[0]
	frontier, err := c.stream.prefix.frontier()
	if err != nil || pair.completedOrdinal > frontier {
		return c.fail(errStageBinding)
	}
	payload, err := json.Marshal(pairEvidence{1, pair.thread, pair.turn, pair.item, pair.startOrdinal, pair.completedOrdinal, pair.startedAtMS, pair.completedAtMS})
	if err != nil {
		return c.fail(err)
	}
	c.serial++
	s := coveragejournal.Stage{Epoch: c.connection.epoch, Connection: c.connection.nonce, Generation: c.connection.generation, Revision: c.revision, Operation: strconv.FormatUint(c.serial, 10), Prefix: pair.completedOrdinal, Payload: payload}
	if err := c.writer.begin(ctx, s); err != nil {
		return c.fail(err)
	}
	c.flight, c.flightContext = &s, ctx
	copy(c.stream.candidates, c.stream.candidates[1:])
	c.stream.candidates[len(c.stream.candidates)-1] = compactionPair{}
	c.stream.candidates = c.stream.candidates[:len(c.stream.candidates)-1]
	return nil
}

// stop revokes before waiting, and never conflates a requested stop with a
// joined disk worker. Native reader/transport shutdown is a separate obligation.
func (c *coordinator) stop(ctx context.Context) error {
	c.fail(errStageStopped)
	return errors.Join(c.revokeError, c.writer.join(ctx))
}
