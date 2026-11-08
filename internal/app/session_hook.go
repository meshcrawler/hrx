package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/meshcrawler/hrx/internal/session"
)

const sessionMetadataEnv = "HRX_SESSION_METADATA"

// receiveSessionStart consumes the public hook contract, not native transcript
// files. It produces no model-visible output and never saves the input payload.
func receiveSessionStart(input io.Reader, metadata string, store session.Store) error {
	return receiveSessionStartHandoff(input, metadata, store, nil)
}

func receiveSessionStartHandoff(input io.Reader, metadata string, store session.Store,
	handoff func(session.Record, string) error) error {
	if metadata == "" {
		return nil
	} // A persistent hook outside hrx is inert.
	if len(metadata) > 16384 {
		return errors.New("invalid hrx session launch metadata")
	}
	var record session.Record
	decoder := json.NewDecoder(bytes.NewBufferString(metadata))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&record) != nil || decoder.Decode(&extra) != io.EOF {
		return errors.New("invalid hrx session launch metadata")
	}
	payload, err := io.ReadAll(io.LimitReader(input, 65537))
	if err != nil || len(payload) > 65536 {
		return errors.New("invalid session-start event")
	}
	var event struct {
		Event  string `json:"hook_event_name"`
		ID     string `json:"session_id"`
		CWD    string `json:"cwd"`
		Model  string `json:"model"`
		Source string `json:"source"`
	}
	if json.Unmarshal(payload, &event) != nil || event.Event != "SessionStart" || event.ID == "" || !filepath.IsAbs(event.CWD) {
		return errors.New("invalid session-start event")
	}
	if event.Source != "startup" && event.Source != "resume" && event.Source != "clear" {
		return nil // Compaction must not make an old launch look newly resumed.
	}
	clear := event.Source == "clear" && record.Harness == "claude" && handoff != nil
	if record.NativeID != "" && record.NativeID != event.ID && !clear {
		return errors.New("harness started a different session than requested")
	}
	if event.Model != "" && event.Model != record.Route && event.Model != record.Identity {
		return errors.New("session-start model differs from the validated launch route")
	}
	project, err := filepath.EvalSymlinks(event.CWD)
	if err != nil {
		return errors.New("cannot resolve session-start project directory")
	}
	if clear {
		if record.Project != "" && filepath.Clean(project) != record.Project {
			return errors.New("Claude clear changed the launch project")
		}
		if err := handoff(record, event.ID); err != nil {
			return err
		}
	}
	record.NativeID, record.Project = event.ID, filepath.Clean(project)
	if err := store.Save(record); err != nil {
		return err
	}
	recordActivity("session-start", event.ID)
	writeResumeReceipt(record.NativeID)
	return nil
}

func runSessionStartHook(input io.Reader) error {
	metadata := os.Getenv(sessionMetadataEnv)
	if metadata == "" {
		return nil
	}
	store, err := session.Default()
	if err != nil {
		return errors.New("cannot locate private hrx session index")
	}
	var prompt, nextID string
	var handoff func(session.Record, string) error
	if binding := os.Getenv(claudeBindingEnv); binding != "" {
		handoff = func(record session.Record, next string) error {
			nextID = next
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			var err error
			prompt, err = handoffClaudeTasks(ctx, binding, record.AgentIdentity, record.NativeID, next, execClaudeTaskCommand)
			return err
		}
	}
	if err := receiveSessionStartHandoff(input, metadata, store, handoff); err != nil {
		// Paths, payloads and lower-level parser/OS diagnostics are not safe
		// to echo into a harness transcript.
		return errors.New("hrx session metadata capture failed; session was not indexed")
	}
	if prompt != "" {
		if err := publishClaudeHandoff(os.Stdout, os.Getenv(claudeBindingEnv), nextID, "SessionStart", prompt); err != nil {
			return errors.New("hrx Claude handoff context publication failed; retry on next prompt")
		}
	}
	return nil
}
