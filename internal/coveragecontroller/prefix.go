// Package coveragecontroller is a dormant coverage-controller implementation in
// progress. No runtime uses it, and prefix assembly alone is not coverage proof.
package coveragecontroller

import (
	"bytes"
	"errors"
)

var (
	errPrefixBinding   = errors.New("coverage prefix binding mismatch")
	errPrefixOrder     = errors.New("coverage prefix ordinal invalid or duplicated")
	errPrefixLimit     = errors.New("coverage prefix buffer exhausted")
	errPrefixReduction = errors.New("coverage prefix reduction not in order")
)

type frameKind uint8

const (
	responseFrame frameKind = iota + 1
	notificationFrame
	serverRequestFrame
)

// The coordinator supplies this identity from its exact owned connection, never
// from an untrusted frame. Generation text alone does not identify a connection.
type connectionID struct{ epoch, nonce, generation string }
type frame struct {
	connection connectionID
	ordinal    uint64
	kind       frameKind
	raw        []byte
}

// prefix is confined to the controller's receiving/reducing goroutine. Disk I/O
// runs elsewhere. Bytes remain charged until successful reduction; a writer
// stall cannot turn this map into an unbounded second queue.
type prefix struct {
	connection                      connectionID
	maxMessages, maxBytes, maxFrame int
	pending                         map[uint64]frame
	bytes                           int
	consumed                        uint64
	peeked                          uint64
	cause                           error
}

func newPrefix(id connectionID, messages, totalBytes, frameBytes int) (*prefix, error) {
	if id.epoch == "" || id.nonce == "" || id.generation == "" || messages <= 0 || totalBytes <= 0 || frameBytes <= 0 || frameBytes > totalBytes {
		return nil, errPrefixBinding
	}
	return &prefix{connection: id, maxMessages: messages, maxBytes: totalBytes, maxFrame: frameBytes, pending: map[uint64]frame{}}, nil
}

func (p *prefix) invalidate(err error) error {
	if p.cause == nil {
		p.cause = err
		p.pending = nil
		p.bytes, p.peeked = 0, 0
	}
	return p.cause
}

func (p *prefix) add(f frame) error {
	if p.cause != nil {
		return p.cause
	}
	if f.connection != p.connection {
		return p.invalidate(errPrefixBinding)
	}
	if f.ordinal == 0 || f.ordinal <= p.consumed {
		return p.invalidate(errPrefixOrder)
	}
	if _, exists := p.pending[f.ordinal]; exists {
		return p.invalidate(errPrefixOrder)
	}
	if f.kind < responseFrame || f.kind > serverRequestFrame || len(f.raw) == 0 {
		return p.invalidate(errPrefixBinding)
	}
	if len(f.raw) > p.maxFrame || len(p.pending) >= p.maxMessages || len(f.raw) > p.maxBytes-p.bytes {
		return p.invalidate(errPrefixLimit)
	}
	f.raw = bytes.Clone(f.raw)
	p.pending[f.ordinal] = f
	p.bytes += len(f.raw)
	return nil
}

// next does not consume. The reducer must finish validating/applying this exact
// frame before commit. A hole returns no frame, never a fabricated ordinal.
func (p *prefix) next() (frame, bool, error) {
	if p.cause != nil {
		return frame{}, false, p.cause
	}
	if p.consumed == ^uint64(0) {
		return frame{}, false, p.invalidate(errPrefixOrder)
	}
	f, ok := p.pending[p.consumed+1]
	if !ok {
		return frame{}, false, nil
	}
	p.peeked = f.ordinal
	f.raw = bytes.Clone(f.raw)
	return f, true, nil
}

func (p *prefix) commit(ordinal uint64) error {
	if p.cause != nil {
		return p.cause
	}
	if ordinal == 0 || ordinal != p.peeked || ordinal != p.consumed+1 {
		return p.invalidate(errPrefixReduction)
	}
	f, ok := p.pending[ordinal]
	if !ok {
		return p.invalidate(errPrefixReduction)
	}
	delete(p.pending, ordinal)
	p.bytes -= len(f.raw)
	p.consumed, p.peeked = ordinal, 0
	return nil
}

// frontier is usable only by the owning coordinator alongside its admitted
// binding and semantic reducer state. It is not a replay cursor or gate token.
func (p *prefix) frontier() (uint64, error) {
	if p.cause != nil {
		return 0, p.cause
	}
	return p.consumed, nil
}
