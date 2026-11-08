package coveragecontroller

import "errors"

var (
	errLifecycleBinding = errors.New("compaction lifecycle binding mismatch")
	errLifecycleOrder   = errors.New("compaction lifecycle incomplete or duplicated")
	errLifecycleLimit   = errors.New("compaction lifecycle state exhausted")
)

// Normalized from pinned rust-v0.163.0 ItemStarted/CompletedNotification and
// ThreadItem::ContextCompaction. Parsing and baseline admission belong to the
// coordinator. Native timestamps are evidence, not a monotonic clock or cause.
type compactionEvent struct {
	connection         connectionID
	thread, turn, item string
	ordinal            uint64
	started            bool
	timeMS             int64
}

type itemKey struct{ turn, item string }
type lifecycle struct {
	start     compactionEvent
	completed bool
}
type compactionPair struct {
	thread, turn, item             string
	startOrdinal, completedOrdinal uint64
	startedAtMS, completedAtMS     int64
	// No cause field: this protocol supplies no authoritative cause.
}

// compactions belongs to exactly one admitted baseline/connection. Reconnect
// constructs a new instance; there is deliberately no import/recovery method.
// Completed keys remain bounded tombstones to reject duplicate completions.
type compactions struct {
	connection connectionID
	thread     string
	last       uint64
	limit      int
	items      map[itemKey]lifecycle
	cause      error
}

func newCompactions(id connectionID, thread string, limit int) (*compactions, error) {
	if id.epoch == "" || id.nonce == "" || id.generation == "" || thread == "" || len(thread) > 512 || limit <= 0 {
		return nil, errLifecycleBinding
	}
	return &compactions{connection: id, thread: thread, limit: limit, items: map[itemKey]lifecycle{}}, nil
}

func (c *compactions) invalidate(err error) error {
	if c.cause == nil {
		c.cause = err
		c.items = nil
	}
	return c.cause
}

// apply runs only after the prefix assembler has made this frame the next
// contiguous frame. Unrelated frames are handled by the coordinator, so ordinals
// of compaction events need only increase, not be adjacent. Returned pairs are
// candidates for staging; they are not published or certified here.
func (c *compactions) apply(e compactionEvent) (*compactionPair, error) {
	if c.cause != nil {
		return nil, c.cause
	}
	if e.connection != c.connection || e.thread != c.thread || e.turn == "" || e.item == "" || len(e.turn) > 512 || len(e.item) > 512 {
		return nil, c.invalidate(errLifecycleBinding)
	}
	if e.ordinal == 0 || e.ordinal <= c.last {
		return nil, c.invalidate(errLifecycleOrder)
	}
	k := itemKey{e.turn, e.item}
	old, exists := c.items[k]
	if e.started {
		if exists {
			return nil, c.invalidate(errLifecycleOrder)
		}
		if len(c.items) >= c.limit {
			return nil, c.invalidate(errLifecycleLimit)
		}
		c.items[k] = lifecycle{start: e}
		c.last = e.ordinal
		return nil, nil
	}
	if !exists || old.completed {
		return nil, c.invalidate(errLifecycleOrder)
	}
	old.completed = true
	c.items[k] = old
	c.last = e.ordinal
	return &compactionPair{thread: e.thread, turn: e.turn, item: e.item, startOrdinal: old.start.ordinal, completedOrdinal: e.ordinal, startedAtMS: old.start.timeMS, completedAtMS: e.timeMS}, nil
}
