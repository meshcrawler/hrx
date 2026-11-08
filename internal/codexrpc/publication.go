package codexrpc

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
)

// Publication gate (dormant; issue 23 notes 4056/4088). A controller may install an
// immutable publication describing a consumed message prefix, but only under the
// same Client.mu that finishLocked/cancelPending use to invalidate the transport.
// Publication, revocation and terminal invalidation are therefore totally ordered,
// and a reader that observes Done also observes the publication cleared.
//
// This is NOT baseline, subscription, durability or coverage certification. A
// Published value describes one publication instant; it is never a reusable
// permission for a later external side effect. Nothing here performs I/O,
// invokes callbacks or retains caller-mutable data under the mutex.

var (
	ErrNotAdmitted         = errors.New("observer publication not admitted")
	ErrPublicationRefused  = errors.New("observer publication refused")
	ErrRevisionsExhausted  = errors.New("observer admission revisions exhausted")
	errEpochUnavailable    = errors.New("observer process epoch unavailable")
	processEpoch, epochErr = randomID()
)

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Binding identifies one admission: this process, this Client instance (a random
// nonce, never pointer identity), its generation and a revision that is never
// reused by the Client. Reusing a generation string never reuses authority.
type Binding struct {
	Epoch      string
	Nonce      string
	Generation string
	Revision   uint64
}

// Published is an immutable value copy: it holds no reference to caller data.
// Through is the highest connection-local message ordinal the controller claims
// to have consumed; it is not a native sequence, replay cursor or watermark.
type Published struct {
	Binding  Binding
	Through  uint64
	Digest   [32]byte
	Sequence uint64
}

// Consumed is live admission state, not a journal receipt. Publication remains
// the exact immutable durable evidence; Through is the controller's separately
// validated contiguous consumption frontier since that publication.
type Consumed struct {
	Publication Published
	Through     uint64
}

// RenewConsumed never changes the publication or claims new durability. The
// sole receive owner must establish contiguous reduction with no open semantic
// boundary or pending stage before renewing. Delivered is only a sanity bound.
func (c *Client) RenewConsumed(p Published, through uint64) (Consumed, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != TransportOpen {
		return Consumed{}, c.cause
	}
	if !c.admitted || c.publication == nil || *c.publication != p ||
		through < p.Through || through > c.delivered || c.consumed != nil && through < c.consumed.Through {
		return Consumed{}, ErrPublicationRefused
	}
	f := Consumed{Publication: p, Through: through}
	c.consumed = &f
	return f, nil
}

// Admit opens a new admission revision and returns its binding. Any current
// admission and publication are superseded. It fails closed when the transport
// is not open or revisions are exhausted.
func (c *Client) Admit() (Binding, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != TransportOpen {
		return Binding{}, c.cause
	}
	c.consumed = nil
	if c.revision == ^uint64(0) {
		c.admitted, c.publication = false, nil
		return Binding{}, ErrRevisionsExhausted
	}
	c.revision++
	c.admitted, c.publication = true, nil
	return c.bindingLocked(), nil
}

func (c *Client) bindingLocked() Binding {
	return Binding{Epoch: processEpoch, Nonce: c.nonce, Generation: c.generation, Revision: c.revision}
}

// Publish installs an immutable publication for the current admission. It is
// refused unless the transport is open, the binding is exactly the current open
// admission, and Through does not exceed the highest ordinal delivered by Call
// or Next and does not move backwards. This is only a sanity bound: those paths
// can return out of order, so the controller must prove contiguous consumption.
// O(1); no I/O or callbacks under the mutex.
func (c *Client) Publish(b Binding, through uint64, digest [32]byte) (Published, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != TransportOpen {
		return Published{}, c.cause
	}
	if !c.admitted || b != c.bindingLocked() {
		return Published{}, ErrPublicationRefused
	}
	if through > c.delivered || c.publication != nil && through < c.publication.Through {
		return Published{}, ErrPublicationRefused
	}
	if c.published == ^uint64(0) {
		c.admitted, c.publication = false, nil
		c.consumed = nil
		return Published{}, ErrRevisionsExhausted
	}
	c.published++
	p := Published{Binding: b, Through: through, Digest: digest, Sequence: c.published}
	c.publication = &p
	c.consumed = nil
	return p, nil
}

// Current returns the current publication. It reports false once the transport
// is terminal, the admission was revoked or superseded, or nothing is published.
func (c *Client) Current() (Published, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != TransportOpen || !c.admitted || c.publication == nil {
		return Published{}, false
	}
	return *c.publication, true
}

// Revoke ends the given admission without closing the transport (controller-only
// invalidation: persistence or baseline failure, generation replacement). A late
// Publish for that binding is refused. Revoking a stale or foreign binding is
// refused so it cannot clear a newer admission.
func (c *Client) Revoke(b Binding) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != TransportOpen {
		return c.cause
	}
	if !c.admitted || b != c.bindingLocked() {
		return ErrNotAdmitted
	}
	c.admitted, c.publication = false, nil
	c.consumed = nil
	return nil
}
