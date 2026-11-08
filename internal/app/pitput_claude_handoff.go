package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"
)

const claudeBindingEnv = "HRX_CLAUDE_TASK_BINDING"

type claudeTaskBinding struct {
	CLI, Owner, Root, Origin, Current, Pending, Protocol string
	Paths, Retired                                       []string
	ContextPending                                       bool
}

func saveClaudeBinding(path string, binding claudeTaskBinding) error {
	data, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	if err := writePrivateFile(path, data); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func newClaudeBinding(root, cli, owner, id, protocol string, paths []string) (string, error) {
	f, err := os.CreateTemp(root, ".claude-binding-*.json")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		return "", err
	}
	b := claudeTaskBinding{CLI: cli, Owner: owner, Root: root, Origin: id,
		Current: id, Protocol: protocol, Paths: paths}
	return path, saveClaudeBinding(path, b)
}

func readClaudePrivate(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("unsafe Claude task binding")
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(data) > 65536 {
		return nil, errors.New("invalid Claude task binding size")
	}
	return data, nil
}

// Preserve source/filter selection while changing the session and generation.
// The predecessor file is immutable because stale tasks still use it.
func successorClaudeConfig(data []byte, owner, oldID, newID string) ([]byte, error) {
	config, err := parseBridgeConfig(data)
	if err != nil || len(config.Agents) != 1 || config.Backend != "claude-task" {
		return nil, errors.New("invalid Claude source configuration")
	}
	agent := &config.Agents[0]
	if agent.AgentID != owner || agent.ThreadID != oldID || !pitputThreadID.MatchString(newID) {
		return nil, errors.New("invalid Claude successor ownership")
	}
	agent.ThreadID = newID
	if !pitputThreadID.MatchString(config.ClaudeTaskGeneration) {
		return nil, errors.New("invalid Claude task generation")
	}
	config.ClaudeTaskGeneration = claudeTaskGeneration(config.ClaudeTaskGeneration, newID)
	return config.marshal()
}

type claudeTaskCommand func(context.Context, string, ...string) error

func execClaudeTaskCommand(ctx context.Context, cli string, args ...string) error {
	cmd := exec.CommandContext(ctx, cli, args...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.WaitDelay = 100 * time.Millisecond
	return cmd.Run()
}

func handoffClaudeTasks(ctx context.Context, path, owner, origin, next string, run claudeTaskCommand) (string, error) {
	if !filepath.IsAbs(path) || !pitputThreadID.MatchString(next) {
		return "", errors.New("invalid Claude handoff")
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return "", err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	data, err := readClaudePrivate(path)
	if err != nil {
		return "", err
	}
	var b claudeTaskBinding
	if json.Unmarshal(data, &b) != nil || b.Owner != owner || b.Origin != origin ||
		!filepath.IsAbs(b.Root) || !pitputThreadID.MatchString(b.Current) || len(b.Paths) == 0 ||
		len(b.Paths) > 64 || slices.Contains(b.Retired, next) {
		return "", errors.New("invalid Claude handoff binding")
	}
	if b.Pending != "" && b.Pending != next {
		return "", errors.New("Claude handoff requires retry of pending session")
	}
	if b.Current == next {
		return claudeTaskPrompt(b.CLI, b.Owner, b.Current, b.Protocol, b.Paths), nil
	}
	// Record intent before any source is retired. A retry can finish a partial
	// handoff, but cannot silently switch to another target after a failure.
	b.Pending = next
	if err := saveClaudeBinding(path, b); err != nil {
		return "", err
	}
	var wg sync.WaitGroup
	errs := make([]error, len(b.Paths))
	for i, config := range b.Paths {
		wg.Add(1)
		go func(i int, config string) {
			defer wg.Done()
			errs[i] = run(ctx, b.CLI, "bridge", "claude-task", "--config", config,
				"--agent-id", b.Owner, "--session-id", b.Current, "--retire")
		}(i, config)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return "", err
	}
	paths := make([]string, len(b.Paths))
	for i, old := range b.Paths {
		source := filepath.Base(filepath.Dir(old))
		if old != filepath.Join(b.Root, b.Current, source, "bridge.toml") {
			return "", errors.New("invalid Claude source path")
		}
		data, err := readClaudePrivate(old)
		if err != nil {
			return "", err
		}
		data, err = successorClaudeConfig(data, b.Owner, b.Current, next)
		if err != nil {
			return "", err
		}
		paths[i] = filepath.Join(b.Root, next, source, "bridge.toml")
		if err := os.MkdirAll(filepath.Dir(paths[i]), 0700); err != nil {
			return "", err
		}
		if err := writePrivateFile(paths[i], data); err != nil {
			return "", err
		}
		if err := run(ctx, b.CLI, "bridge", "claude-task", "--config", paths[i],
			"--agent-id", b.Owner, "--session-id", next, "--activate"); err != nil {
			return "", err
		}
		if err := run(ctx, b.CLI, "bridge", "prepare", "--config", paths[i], "--start", "latest"); err != nil {
			return "", err
		}
	}
	b.Retired = append(b.Retired, b.Current)
	b.Current, b.Paths, b.Pending = next, paths, ""
	b.ContextPending = true
	if err := saveClaudeBinding(path, b); err != nil {
		return "", err
	}
	return claudeTaskPrompt(b.CLI, b.Owner, b.Current, b.Protocol, b.Paths), nil
}

// A flushed response is publication to the hook interface, not proof of model
// consumption. An interrupted flush remains replayable.
func publishClaudeHandoff(out io.Writer, path, next, event, prompt string) error {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	data, err := readClaudePrivate(path)
	if err != nil {
		return err
	}
	var b claudeTaskBinding
	if json.Unmarshal(data, &b) != nil || b.Current != next || b.Pending != "" {
		return errors.New("Claude handoff changed before hook publication")
	}
	if err := json.NewEncoder(out).Encode(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": event, "additionalContext": "HRX has retired the predecessor session tasks. These bindings replace all earlier HRX task commands. Do not read or reuse predecessor task outputs.\n" + prompt,
	}}); err != nil {
		return err
	}
	b.ContextPending = false
	return saveClaudeBinding(path, b)
}
