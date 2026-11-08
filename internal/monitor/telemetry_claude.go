package monitor

import (
	"encoding/json"
	"math"
	"time"
)

// Claude's persisted usage describes one inference, not session-wide consumption.
// Its transcript does not establish the effective context limit, so Limit stays nil.
func parseClaudeTelemetry(line []byte) (telemetryEvent, bool) {
	var record struct {
		Type            string          `json:"type"`
		Subtype         string          `json:"subtype"`
		Session         string          `json:"sessionId"`
		UUID            string          `json:"uuid"`
		Version         string          `json:"version"`
		Timestamp       string          `json:"timestamp"`
		Sidechain       bool            `json:"isSidechain"`
		Message         json.RawMessage `json:"message"`
		CompactMetadata json.RawMessage `json:"compactMetadata"`
	}
	if json.Unmarshal(line, &record) != nil || record.Session == "" || record.Sidechain {
		return telemetryEvent{}, false
	}
	at, _ := time.Parse(time.RFC3339Nano, record.Timestamp)
	event := telemetryEvent{Session: record.Session, Version: record.Version, At: at}
	if record.Type == "system" && record.Subtype == "compact_boundary" {
		event.ResetContext = true
		event.CompactionID = record.UUID
		var metadata struct {
			PostTokens *int64 `json:"postTokens"`
		}
		if json.Unmarshal(record.CompactMetadata, &metadata) == nil && metadata.PostTokens != nil && *metadata.PostTokens >= 0 {
			event.Used = metadata.PostTokens
		}
		return event, true
	}
	if record.Type != "assistant" {
		return telemetryEvent{}, false
	}
	var message struct {
		Model string          `json:"model"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(record.Message, &message) != nil {
		return telemetryEvent{}, false
	}
	event.Model = message.Model
	var usage struct {
		Input      *int64 `json:"input_tokens"`
		CacheRead  int64  `json:"cache_read_input_tokens"`
		CacheWrite int64  `json:"cache_creation_input_tokens"`
	}
	if json.Unmarshal(message.Usage, &usage) == nil && usage.Input != nil && *usage.Input >= 0 && usage.CacheRead >= 0 && usage.CacheWrite >= 0 {
		used := *usage.Input
		if used <= math.MaxInt64-usage.CacheRead {
			used += usage.CacheRead
			if used <= math.MaxInt64-usage.CacheWrite {
				used += usage.CacheWrite
				event.Used = &used
			}
		}
	}
	return event, event.Model != "" || event.Used != nil
}
