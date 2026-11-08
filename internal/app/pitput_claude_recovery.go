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

func recoverClaudeHandoff(ctx context.Context, input io.Reader, metadata, binding string,
	store session.Store, out io.Writer, run claudeTaskCommand) error {
	if metadata == "" || binding == "" {
		return nil
	}
	if len(metadata) > 16384 {
		return errors.New("invalid Claude recovery metadata")
	}
	var record session.Record
	decoder := json.NewDecoder(bytes.NewBufferString(metadata))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&record) != nil || decoder.Decode(&extra) != io.EOF {
		return errors.New("invalid Claude recovery metadata")
	}
	if record.Harness != "claude" || !record.Pitput {
		return nil
	}
	payload, err := io.ReadAll(io.LimitReader(input, 65537))
	if err != nil || len(payload) > 65536 {
		return errors.New("invalid Claude recovery event")
	}
	var event struct {
		Event string `json:"hook_event_name"`
		ID    string `json:"session_id"`
		CWD   string `json:"cwd"`
	}
	if json.Unmarshal(payload, &event) != nil || event.Event != "UserPromptSubmit" ||
		!pitputThreadID.MatchString(event.ID) || !filepath.IsAbs(event.CWD) {
		return errors.New("invalid Claude recovery event")
	}
	project, err := filepath.EvalSymlinks(event.CWD)
	if err != nil || project != record.Project {
		return errors.New("Claude recovery project mismatch")
	}
	data, err := readClaudePrivate(binding)
	if err != nil {
		return err
	}
	var b claudeTaskBinding
	if json.Unmarshal(data, &b) != nil || b.Owner != record.AgentIdentity || b.Origin != record.NativeID {
		return errors.New("Claude recovery ownership mismatch")
	}
	// A normal prompt does not re-prime or start a transition. Only a previously
	// validated clear intent, or its unpublished completion, can be recovered.
	if b.Pending == "" && !b.ContextPending && b.Current == event.ID {
		return nil
	}
	if b.Pending != event.ID && !(b.Pending == "" && b.ContextPending && b.Current == event.ID) {
		return errors.New("Claude recovery session mismatch")
	}
	prompt, err := handoffClaudeTasks(ctx, binding, record.AgentIdentity, record.NativeID, event.ID, run)
	if err != nil {
		return err
	}
	record.NativeID = event.ID
	if err := store.Save(record); err != nil {
		return err
	}
	writeResumeReceipt(event.ID)
	return publishClaudeHandoff(out, binding, event.ID, "UserPromptSubmit", prompt)
}

func runClaudeHandoffRecovery(input io.Reader, out io.Writer) error {
	metadata, binding := os.Getenv(sessionMetadataEnv), os.Getenv(claudeBindingEnv)
	if metadata == "" || binding == "" {
		return nil
	}
	store, err := session.Default()
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		err = recoverClaudeHandoff(ctx, input, metadata, binding, store, out, execClaudeTaskCommand)
	}
	if err != nil {
		return errors.New("hrx Claude handoff recovery failed; monitoring remains blocked")
	}
	return nil
}
