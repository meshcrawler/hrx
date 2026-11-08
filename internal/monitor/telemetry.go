package monitor

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// SessionMetrics contains only allowlisted native metadata. Context is a latest
// inference estimate, never the session's cumulative billing usage.
type SessionMetrics struct {
	Session      string            `json:"session"`
	Source       string            `json:"source"`
	Version      string            `json:"version,omitempty"`
	Availability string            `json:"availability"`
	CollectedAt  time.Time         `json:"collected_at"`
	Context      *ContextMetric    `json:"context,omitempty"`
	Compactions  *CompactionMetric `json:"compactions,omitempty"`
}
type ContextMetric struct {
	Used     int64     `json:"used_tokens"`
	Limit    *int64    `json:"limit_tokens,omitempty"`
	At       time.Time `json:"observed_at"`
	Accuracy string    `json:"accuracy"`
}
type CompactionMetric struct {
	Count    int64     `json:"count"`
	Coverage string    `json:"coverage"`
	At       time.Time `json:"observed_at"`
}

// This state stays in the private host observation file; it is never part of
// snapshots or hub requests. Replaying a session computes an absolute count,
// so a new HRX run or a collector restart cannot add to a prior run's count.
type telemetryState struct {
	Root            string          `json:"root"`
	Session         string          `json:"session,omitempty"`
	Path            string          `json:"path,omitempty"`
	FileID          string          `json:"file_id,omitempty"`
	PrefixHash      string          `json:"prefix_hash,omitempty"`
	PrefixBytes     int             `json:"prefix_bytes,omitempty"`
	Offset          int64           `json:"offset,omitempty"`
	Bound           bool            `json:"bound,omitempty"`
	Partial         bool            `json:"partial,omitempty"`
	Model           string          `json:"model,omitempty"`
	Version         string          `json:"version,omitempty"`
	Seen            map[string]bool `json:"seen,omitempty"`
	Context         *ContextMetric  `json:"context,omitempty"`
	CompactedAt     time.Time       `json:"compacted_at,omitempty"`
	AttemptedAt     time.Time       `json:"attempted_at,omitempty"`
	Discarding      bool            `json:"discarding,omitempty"`
	LastUsage       string          `json:"last_usage,omitempty"`
	Frozen          bool            `json:"frozen,omitempty"`
	NativeStartedAt time.Time       `json:"native_started_at,omitempty"`
}
type telemetryEvent struct {
	Session, Model, Version string
	UsageID                 string
	At                      time.Time
	Used, Limit             *int64
	CompactionID            string
	ResetContext, Started   bool
}

var nativeSessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var nativeVersion = regexp.MustCompile(`^[0-9][A-Za-z0-9.+_-]{0,63}$`)

const maxTelemetryLine = 2 * 1024 * 1024

func newTelemetryState(harness string, env []string) *telemetryState {
	v := map[string]string{}
	for _, e := range env {
		k, value, _ := strings.Cut(e, "=")
		v[k] = value
	}
	root := ""
	switch harness {
	case "codex":
		root = v["CODEX_HOME"]
		if root == "" {
			root = filepath.Join(v["HOME"], ".codex")
		}
	case "claude":
		root = v["CLAUDE_CONFIG_DIR"]
		if root == "" {
			root = filepath.Join(v["HOME"], ".claude")
		}
	}
	if !filepath.IsAbs(root) || len(root) > 4096 {
		root = ""
	}
	if root != "" {
		ancestor := root
		for {
			if real, err := filepath.EvalSymlinks(ancestor); err == nil {
				suffix, _ := filepath.Rel(ancestor, root)
				root = filepath.Join(real, suffix)
				break
			}
			parent := filepath.Dir(ancestor)
			if parent == ancestor {
				break
			}
			ancestor = parent
		}
	}
	return &telemetryState{Root: root}
}

// BindTelemetry records only the harness storage root from the actual launch
// environment. Credentials and arbitrary environment entries are discarded.
func BindTelemetry(r *Record, env []string) { r.Telemetry = newTelemetryState(r.Harness, env) }

func telemetryPath(ctx context.Context, s *telemetryState, harness, session string) string {
	base := filepath.Join(s.Root, "sessions")
	if harness == "claude" {
		base = filepath.Join(s.Root, "projects")
	}
	entries := 0
	complete := true
	var matches []string
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		entries++
		if entries > 20000 || ctx.Err() != nil {
			complete = false
			return fs.SkipAll
		}
		if err != nil {
			complete = false
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			// Claude subagents are distinct sessions; never bind the parent to them.
			if d.Name() == "subagents" {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if (harness == "codex" && strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, "-"+session+".jsonl")) || (harness == "claude" && name == session+".jsonl") {
			matches = append(matches, path)
		}
		return nil
	})
	if !complete || len(matches) != 1 {
		return ""
	}
	return matches[0]
}

func collectTelemetry(ctx context.Context, r *Record, budget *int64) {
	if r.RunID == "" {
		return
	}
	if r.Status == "exited" && r.EndedAt == nil {
		return
	}
	if *budget <= 0 || ctx.Err() != nil {
		return
	}
	if r.Harness != "codex" && r.Harness != "claude" {
		r.Metrics = &SessionMetrics{Session: r.SessionID, Source: "unsupported", Availability: "unsupported"}
		return
	}
	if !nativeSessionID.MatchString(r.SessionID) {
		r.Metrics = nil
		return
	}
	s := r.Telemetry
	if s == nil || s.Root == "" {
		r.Metrics = &SessionMetrics{Session: r.SessionID, Source: r.Harness + "-jsonl", Availability: "unavailable"}
		return
	}
	if s.Session != r.SessionID {
		root := s.Root
		*s = telemetryState{Root: root, Session: r.SessionID}
	}
	if s.Frozen && r.EndedAt != nil {
		return
	}
	m := &SessionMetrics{Session: r.SessionID, Source: r.Harness + "-jsonl", Version: s.Version, Availability: "unavailable"}
	r.Metrics = m
	if s.Path == "" {
		if time.Since(s.AttemptedAt) < 30*time.Second {
			return
		}
		s.AttemptedAt = time.Now().UTC()
		s.Path = telemetryPath(ctx, s, r.Harness, r.SessionID)
		if s.Path == "" {
			return
		}
	}
	// A local cursor is not authority to read outside the captured harness root.
	rel, err := filepath.Rel(s.Root, s.Path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return
	}
	real, err := filepath.EvalSymlinks(s.Path)
	if err != nil || real != s.Path {
		s.Path = ""
		return
	}
	f, err := os.OpenFile(s.Path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		s.Path = ""
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	idBytes, _ := json.Marshal([]uint64{uint64(stat.Dev), uint64(stat.Ino)})
	fileID := string(idBytes)
	prefixChanged := false
	if s.PrefixBytes > 0 && s.PrefixBytes <= 256 {
		prefix := make([]byte, s.PrefixBytes)
		n, _ := f.ReadAt(prefix, 0)
		h := sha256.Sum256(prefix[:n])
		prefixChanged = hex.EncodeToString(h[:]) != s.PrefixHash
	}
	if s.FileID != "" && (s.FileID != fileID || info.Size() < s.Offset || prefixChanged) {
		root, path, session := s.Root, s.Path, s.Session
		*s = telemetryState{Root: root, Path: path, Session: session, Partial: true}
	}
	s.FileID = fileID
	if s.PrefixBytes == 0 && info.Size() > 0 {
		prefix := make([]byte, min(int64(256), info.Size()))
		n, _ := f.ReadAt(prefix, 0)
		h := sha256.Sum256(prefix[:n])
		s.PrefixBytes = n
		s.PrefixHash = hex.EncodeToString(h[:])
	}
	if s.Seen == nil {
		s.Seen = map[string]bool{}
	}
	if _, err = f.Seek(s.Offset, io.SeekStart); err != nil {
		return
	}
	allowance := min(*budget, int64(4*1024*1024))
	reader := bufio.NewReaderSize(io.LimitReader(f, allowance), 64*1024)
	consumed := int64(0)
	for consumed < allowance && ctx.Err() == nil {
		line, readErr := readTelemetryLine(reader)
		consumed += int64(len(line))
		if readErr != nil {
			if consumed >= allowance && (len(line) > maxTelemetryLine || s.Discarding) {
				s.Offset += int64(len(line))
				s.Discarding = true
				s.Partial = true
				s.Context = nil
			}
			break // A truncated final line is retried after append.
		}
		s.Offset += int64(len(line))
		if s.Discarding {
			s.Discarding = false
			s.Partial = true
			s.Context = nil
			continue
		}
		if len(line) > maxTelemetryLine {
			s.Partial = true
			s.Context = nil
			continue
		}
		var e telemetryEvent
		var recognized bool
		if r.Harness == "codex" {
			e, recognized = parseCodexTelemetry(line)
		} else {
			e, recognized = parseClaudeTelemetry(line)
		}
		if !json.Valid(line) {
			s.Partial = true
			s.Context = nil
			continue
		}
		if !recognized {
			continue
		}
		if r.Harness == "codex" && e.Started {
			if e.Session != r.SessionID || e.At.IsZero() {
				s.Bound = false
				return
			}
			s.Bound = true
			if s.NativeStartedAt.IsZero() {
				s.NativeStartedAt = e.At
			}
		}
		if r.Harness == "claude" {
			if e.Session != r.SessionID {
				continue
			}
			s.Bound = true
		}
		if !s.Bound || (r.EndedAt != nil && e.At.After(*r.EndedAt)) {
			continue
		}
		if !s.NativeStartedAt.IsZero() && !e.At.IsZero() && e.At.Before(s.NativeStartedAt) {
			continue
		}
		if nativeVersion.MatchString(e.Version) {
			s.Version = e.Version
		}
		// Codex also emits token_count for rate-limit/status changes with the
		// preceding response's usage. Those are not fresh context observations.
		if e.UsageID != "" {
			if e.UsageID == s.LastUsage {
				if s.Context != nil && !sameLimit(e.Limit, s.Context.Limit) {
					s.Context = nil
				}
				continue
			}
			s.LastUsage = e.UsageID
		}
		if e.CompactionID != "" {
			h := sha256.Sum256([]byte(e.CompactionID))
			if s.Seen[hex.EncodeToString(h[:])] {
				continue
			}
		}
		if e.ResetContext || (e.Model != "" && s.Model != "" && e.Model != s.Model) {
			s.Context = nil
		}
		if e.Model != "" && len(e.Model) <= 256 {
			s.Model = e.Model
		}
		if e.CompactionID != "" && !e.At.IsZero() {
			h := sha256.Sum256([]byte(e.CompactionID))
			key := hex.EncodeToString(h[:])
			if len(s.Seen) < 1024 || s.Seen[key] {
				s.Seen[key] = true
				s.CompactedAt = e.At
			} else {
				s.Partial = true
			}
		}
		// A resumed run may change model. Do not reuse its predecessor's context
		// reading/limit until the native session reports new usage for this run.
		if e.Used != nil && *e.Used >= 0 && *e.Used <= 1e12 && !e.At.Before(r.StartedAt) && !e.At.IsZero() {
			limit := e.Limit
			if limit != nil && (*limit <= 0 || *limit > 1e12) {
				limit = nil
			}
			s.Context = &ContextMetric{Used: *e.Used, Limit: limit, At: e.At, Accuracy: "estimate"}
		}
	}
	*budget -= consumed
	if !s.Bound {
		return
	}
	m.Version = s.Version
	m.CollectedAt = time.Now().UTC()
	m.Context = s.Context
	m.Availability = "available"
	if s.Offset < info.Size() {
		m.Availability = "catching-up"
	}
	if s.Partial {
		m.Availability = "partial"
	}
	// Local logs can be pruned or imported. Never label these observations as
	// complete lifetime history, even when we reached the current end of file.
	m.Compactions = &CompactionMetric{Count: int64(len(s.Seen)), Coverage: "observed", At: s.CompactedAt}
	if r.EndedAt != nil && s.Offset >= info.Size() {
		s.Frozen = true
	}
}

// ValidateMetrics also protects authenticated hub ingestion from arbitrary
// strings or malformed numbers masquerading as telemetry metadata.
func ValidateMetrics(m *SessionMetrics, session, harness string) bool {
	if m == nil {
		return true
	}
	if m.Session != session || (m.Source != harness+"-jsonl" && m.Source != "unsupported") {
		return false
	}
	if (m.Source == "unsupported") != (m.Availability == "unsupported") {
		return false
	}
	if m.Version != "" && !nativeVersion.MatchString(m.Version) {
		return false
	}
	switch m.Availability {
	case "available", "unavailable", "unsupported", "partial", "catching-up":
	default:
		return false
	}
	if c := m.Context; c != nil {
		if m.Availability == "unsupported" || m.Availability == "unavailable" || m.CollectedAt.IsZero() {
			return false
		}
		if c.Used < 0 || c.Used > 1e12 || c.At.IsZero() || c.Accuracy != "estimate" {
			return false
		}
		if c.Limit != nil && (*c.Limit <= 0 || *c.Limit > 1e12) {
			return false
		}
	}
	if c := m.Compactions; c != nil {
		if m.Availability == "unsupported" || m.Availability == "unavailable" || m.CollectedAt.IsZero() {
			return false
		}
		if c.Count < 0 || c.Count > 1024 || c.Coverage != "observed" {
			return false
		}
	}
	return true
}

func sameLimit(a, b *int64) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

func readTelemetryLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := r.ReadSlice('\n')
		line = append(line, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

func parseCodexTelemetry(line []byte) (telemetryEvent, bool) {
	var d struct {
		Type      string    `json:"type"`
		Timestamp time.Time `json:"timestamp"`
		Payload   struct {
			Type       string `json:"type"`
			ID         string `json:"id"`
			Model      string `json:"model"`
			Version    string `json:"cli_version"`
			WindowID   string `json:"window_id"`
			ResponseID string `json:"compaction_response_id"`
			Info       *struct {
				Total *struct {
					Total *int64 `json:"total_tokens"`
				} `json:"total_token_usage"`
				Last *struct {
					Total *int64 `json:"total_tokens"`
				} `json:"last_token_usage"`
				Limit *int64 `json:"model_context_window"`
			} `json:"info"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &d) != nil {
		var envelope struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &envelope) == nil {
			switch envelope.Type {
			case "compacted", "turn_context", "event_msg":
				return telemetryEvent{ResetContext: true}, true
			}
		}
		return telemetryEvent{}, false
	}
	e := telemetryEvent{At: d.Timestamp}
	switch d.Type {
	case "session_meta":
		e.Started = true
		e.Session = d.Payload.ID
		e.Version = d.Payload.Version
	case "turn_context":
		e.Model = d.Payload.Model
	case "compacted":
		e.ResetContext = true
		e.CompactionID = d.Payload.WindowID
		if e.CompactionID == "" {
			e.CompactionID = d.Payload.ResponseID
		}
		if e.CompactionID == "" && !e.At.IsZero() {
			e.CompactionID = e.At.Format(time.RFC3339Nano)
		}
	case "event_msg":
		if d.Payload.Type != "token_count" || d.Payload.Info == nil || d.Payload.Info.Last == nil {
			return e, false
		}
		e.Used = d.Payload.Info.Last.Total
		e.Limit = d.Payload.Info.Limit
		if e.Used != nil && d.Payload.Info.Total != nil && d.Payload.Info.Total.Total != nil {
			data, _ := json.Marshal([]int64{*e.Used, *d.Payload.Info.Total.Total})
			h := sha256.Sum256(data)
			e.UsageID = hex.EncodeToString(h[:])
		}
	default:
		return e, false
	}
	return e, true
}
