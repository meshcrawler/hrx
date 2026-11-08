package coveragecontroller

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"github.com/meshcrawler/hrx/internal/codexlaunch"
)

var (
	errEventFormat    = errors.New("invalid coverage event")
	errServerRequest  = errors.New("coverage server request requires explicit handling")
	errCandidateLimit = errors.New("coverage candidate queue exhausted")
	errThreadBoundary = errors.New("observed thread lifecycle invalidated")
)

// Pinned protocol/common.rs lifecycle boundaries invalidate the observed
// baseline, configuration evidence or subscription. Before startup identity is
// known, none is safe. Settings updates need fresh comparison and admission;
// their notification alone cannot renew the original baseline's authority.
func checkThreadBoundary(method string, raw json.RawMessage, target string) error {
	switch method {
	case "thread/closed", "thread/archived", "thread/deleted", "thread/reverted", "thread/settings/updated":
		var params struct {
			Thread string `json:"threadId"`
		}
		if json.Unmarshal(raw, &params) != nil || params.Thread == "" {
			return errEventFormat
		}
		if target == "" || params.Thread == target {
			return errThreadBoundary
		}
	}
	return nil
}

// checkJSON rejects duplicate keys and excessive nesting before typed parsing.
// Ambiguous lifecycle identities or timestamps must not be last-key-wins.
func checkJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errEventFormat
		}
		t, err := d.Token()
		if err != nil {
			return errEventFormat
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return errEventFormat
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return errEventFormat
				}
				seen[s] = true
				if err = value(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return errEventFormat
			}
		case '[':
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return errEventFormat
			}
		default:
			return errEventFormat
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errEventFormat
	}
	return nil
}

// decodeCompaction consumes the exact pinned native notification shape. It
// retains timestamps verbatim, requires identity, and infers no public cause.
func decodeCompaction(f frame, target string) (compactionEvent, bool, error) {
	if err := checkJSON(f.raw); err != nil {
		return compactionEvent{}, false, err
	}
	if f.kind == responseFrame {
		return compactionEvent{}, false, nil
	}
	if f.kind == serverRequestFrame {
		return compactionEvent{}, false, errServerRequest
	}
	var msg struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(f.raw, &msg) != nil || msg.Method == "" {
		return compactionEvent{}, false, errEventFormat
	}
	if err := checkThreadBoundary(msg.Method, msg.Params, target); err != nil {
		return compactionEvent{}, false, err
	}
	if msg.Method != "item/started" && msg.Method != "item/completed" {
		return compactionEvent{}, false, nil
	}
	var params struct {
		Thread string `json:"threadId"`
		Turn   string `json:"turnId"`
		Item   struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"item"`
		Started   *int64 `json:"startedAtMs"`
		Completed *int64 `json:"completedAtMs"`
	}
	if json.Unmarshal(msg.Params, &params) != nil || params.Thread == "" || params.Turn == "" || params.Item.Type == "" || params.Item.ID == "" {
		return compactionEvent{}, false, errEventFormat
	}
	if params.Thread != target || params.Item.Type != "contextCompaction" {
		return compactionEvent{}, false, nil
	}
	started := msg.Method == "item/started"
	stamp := params.Completed
	if started {
		stamp = params.Started
	}
	if stamp == nil {
		return compactionEvent{}, false, errEventFormat
	}
	return compactionEvent{f.connection, params.Thread, params.Turn, params.Item.ID, f.ordinal, started, *stamp}, true, nil
}

// stream joins the bounded assembler and reducer. It has no baseline/publication
// API. Candidates still require durable staging and the exact live gate binding.
// The coordinator must invalidate this stream on ANY Call error lacking its
// response ordinal, transport loss, failed baseline, or stage timeout/error.
type stream struct {
	settings      *codexlaunch.Native
	events        *eventQueue
	approvals     *approvalBook
	prefix        *prefix
	lifecycles    *compactions
	candidates    []compactionPair
	maxCandidates int
	cause         error
}

func newStream(id connectionID, thread string, messages, totalBytes, frameBytes, items, candidates int) (*stream, error) {
	if candidates <= 0 {
		return nil, errCandidateLimit
	}
	p, err := newPrefix(id, messages, totalBytes, frameBytes)
	if err != nil {
		return nil, err
	}
	c, err := newCompactions(id, thread, items)
	if err != nil {
		return nil, err
	}
	return &stream{prefix: p, lifecycles: c, maxCandidates: candidates}, nil
}

func (s *stream) invalidate(err error) error {
	if s.cause == nil {
		s.cause = err
		if s.events != nil {
			s.events.stop(err)
		}
		s.prefix.invalidate(err)
		s.lifecycles.invalidate(err)
		s.candidates = nil
	}
	return s.cause
}

func (s *stream) accept(f frame) error {
	if s.cause != nil {
		return s.cause
	}
	if err := s.prefix.add(f); err != nil {
		return s.invalidate(err)
	}
	for {
		next, ok, err := s.prefix.next()
		if err != nil {
			return s.invalidate(err)
		}
		if !ok {
			return nil
		}
		var e compactionEvent
		var relevant bool
		if next.kind == serverRequestFrame && s.approvals != nil {
			err = s.approvals.reduce(next, s.lifecycles.thread)
		} else {
			e, relevant, err = decodeCompaction(next, s.lifecycles.thread)
			if errors.Is(err, errThreadBoundary) && s.settings != nil && checkJSON(next.raw) == nil {
				var notice struct {
					Method string
					Params struct {
						ThreadID       string
						ThreadSettings json.RawMessage
					}
				}
				if json.Unmarshal(next.raw, &notice) == nil && notice.Method == "thread/settings/updated" && notice.Params.ThreadID == s.lifecycles.thread {
					if field := s.settings.ThreadSettingsMismatch(notice.Params.ThreadSettings); field == "" {
						err = nil
					} else {
						err = fmt.Errorf("%w: settings %s", errThreadBoundary, field)
					}
				}
			}
		}
		if err != nil {
			return s.invalidate(err)
		}
		if next.kind == notificationFrame && s.approvals != nil {
			if err := s.approvals.resolve(next, s.lifecycles.thread); err != nil {
				return s.invalidate(err)
			}
		}
		if relevant {
			pair, err := s.lifecycles.apply(e)
			if err != nil {
				return s.invalidate(err)
			}
			if pair != nil {
				if len(s.candidates) >= s.maxCandidates {
					return s.invalidate(errCandidateLimit)
				}
				s.candidates = append(s.candidates, *pair)
			}
		}
		if err := s.prefix.commit(next.ordinal); err != nil {
			return s.invalidate(err)
		}
		if (next.kind == notificationFrame || next.kind == serverRequestFrame) && s.events != nil {
			if err := s.events.push(next); err != nil {
				return s.invalidate(err)
			}
		}
	}
}
