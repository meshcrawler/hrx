package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Select the ID before launch, then let SessionStart verify what Claude opened.
// A resume retains its ID; literal prompt text never selects a session.
func bindClaudeTaskSession(l *Launch, resume string) (string, error) {
	id := resume
	found := false
	for _, token := range nativeArguments("claude", l.Args) {
		name, value, equal := strings.Cut(l.Args[token.index], "=")
		if !token.option || name != "--session-id" {
			continue
		}
		if found || resume != "" {
			return "", errors.New("Claude Pitput requires one unambiguous native session")
		}
		found = true
		if !equal && token.index+1 < len(l.Args) {
			value = l.Args[token.index+1]
		}
		id = value
	}
	if id == "" && !found {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
		id = fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
	}
	if !pitputThreadID.MatchString(id) {
		return "", errors.New("Claude Pitput requires a canonical native session UUID")
	}
	if !found && resume == "" {
		l.Args = append([]string{"--session-id", id}, l.Args...)
	}
	return id, nil
}

// Each launch has a private config root. Derivation makes a failed handoff
// retry stable without ever overwriting or reactivating predecessor commands.
func claudeTaskGeneration(seed, id string) string {
	b := sha256.Sum256([]byte("hrx-claude-task-generation\x00" + seed + "\x00" + id))
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func claudeTaskBridge(home, root, id string) bridgeConfig {
	return bridgeConfig{Backend: "claude-task", ClaudeHome: home, MonitorTimeout: 20,
		ClaudeTaskGeneration: claudeTaskGeneration(root, id)}
}

func (r *pitputRuntime) claudeTaskConfigs(root, stateRoot, home, id string) ([]string, error) {
	if !pitputThreadID.MatchString(id) || !filepath.IsAbs(home) {
		return nil, errors.New("invalid Claude task binding")
	}
	if r.combined {
		return r.nativeCombinedConfig(root, stateRoot, id, claudeTaskBridge(home, root, id))
	}
	var paths []string
	for _, board := range r.boards {
		dir := filepath.Join(root, id, board)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		state, err := r.bridgeState(stateRoot, board)
		if err != nil {
			return nil, err
		}
		config := claudeTaskBridge(home, root, id)
		config.StateDirectory, config.Agents = state, []bridgeAgent{r.boardAgent(board, id)}
		path, err := writeBridgeConfig(dir, config)
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	if r.dm {
		path, err := r.directMessageConfigState(root, stateRoot, id, claudeTaskBridge(home, root, id))
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func startClaudePitput(ctx context.Context, l *Launch, root string, out io.Writer) error {
	r := l.pitputRuntime
	protocolCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var protocol limitedBuffer
	cmd := exec.CommandContext(protocolCtx, r.cli, "attention", "protocol")
	cmd.Env, cmd.Stdout, cmd.Stderr = l.Env, &protocol, io.Discard
	cmd.WaitDelay = 100 * time.Millisecond
	if err := cmd.Run(); err != nil || protocol.Len() > 8192 ||
		!strings.HasPrefix(protocol.String(), "Pitput attention protocol attention-v1. ") {
		return errors.New("Claude compact tasks require Pitput attention protocol support")
	}
	home := r.record.ClaudeConfigDir
	if home == "" {
		for _, entry := range l.Env {
			if strings.HasPrefix(entry, "CLAUDE_CONFIG_DIR=") {
				home = strings.TrimPrefix(entry, "CLAUDE_CONFIG_DIR=")
			}
		}
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		home = filepath.Join(userHome, ".claude")
	}
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		return errors.New("Claude Pitput requires an existing private Claude configuration directory")
	}
	configRoot, err := os.MkdirTemp(root, ".claude-launch-*")
	if err != nil {
		return err
	}
	paths, err := r.claudeTaskConfigs(configRoot, root, home, r.record.NativeID)
	if err != nil {
		return err
	}
	for _, path := range paths {
		activateCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		err := execClaudeTaskCommand(activateCtx, r.cli, "bridge", "claude-task", "--config", path,
			"--agent-id", r.identity, "--session-id", r.record.NativeID, "--activate")
		cancel()
		if err != nil {
			return errors.New("Claude Pitput task generation activation failed; monitoring remains blocked")
		}
		if err := r.prepareBridge(ctx, path, false); err != nil {
			return err
		}
	}
	binding, err := newClaudeBinding(configRoot, r.cli, r.identity, r.record.NativeID, protocol.String(), paths)
	if err != nil {
		return err
	}
	prompt := claudeTaskPrompt(r.cli, r.identity, r.record.NativeID, protocol.String(), paths)
	l.Args, err = appendClaudeSystemPrompt(l.Args, prompt)
	if err != nil {
		return err
	}
	encoded, _ := json.Marshal(paths)
	l.Env = childEnvironment(l.Env, map[string]string{"HRX_CLAUDE_TASK_CONFIGS": string(encoded), claudeBindingEnv: binding}, nil)
	fmt.Fprintf(out, "hrx: Claude Pitput tasks prepared: %s -> %s (%s); native background tasks start in-session\n", r.identity, r.record.NativeID, r.targets())
	return nil
}

// Coalesce real options so duplicate flags cannot discard owned guidance.
// Option values and everything after -- remain literal user data.
func appendClaudeSystemPrompt(args []string, prompt string) ([]string, error) {
	remove := make(map[int]bool)
	var parts []string
	for _, token := range nativeArguments("claude", args) {
		name, value, equal := strings.Cut(args[token.index], "=")
		if !token.option || name != "--append-system-prompt" {
			continue
		}
		remove[token.index] = true
		if !equal {
			if token.index+1 == len(args) {
				return nil, errors.New("Claude append-system-prompt requires a value")
			}
			value = args[token.index+1]
			remove[token.index+1] = true
		}
		parts = append(parts, value)
	}
	parts = append(parts, prompt)
	result := []string{"--append-system-prompt", strings.Join(parts, "\n")}
	for i, arg := range args {
		if !remove[i] {
			result = append(result, arg)
		}
	}
	return result, nil
}

func claudeTaskPrompt(cli, owner, id, protocol string, paths []string) string {
	prompt := "\n### HRX owned Claude compact tasks\nHRX prepared the following source bindings for this native session. Start exactly one Bash background task per listed command when monitoring is authorized. Each task waits through quiet polls and returns one metadata-only presentation. Read only the output of your own task; follow its handle read and explicit handling ACK instructions. After handling, re-arm the same command. Never start a separate cursor monitor or ACK work you have not handled. Task completion is not handling.\n"
	prompt += "After native /clear, use only replacement task commands supplied by the HRX SessionStart hook. They supersede earlier bindings. Never reuse predecessor task commands or outputs. If the hook reports failure, monitoring is blocked until the handoff is recovered.\n"
	// The native system prompt is retained across supported Claude compaction;
	// do not depend on a one-time tool result surviving context summarization.
	prompt += "\nCanonical protocol retained in this session's system instructions:\n" + strings.TrimSpace(protocol) + "\n"
	for _, path := range paths {
		command := shellWord(cli) + " bridge claude-task --config " + shellWord(path) +
			" --agent-id " + shellWord(owner) + " --session-id " + shellWord(id)
		prompt += "```sh\numask 077\nwhile :; do\n  event=$(" + command + ") || exit \"$?\"\n  if [ -n \"$event\" ]; then printf '%s\\n' \"$event\"; break; fi\ndone\n```\n"
	}
	return prompt
}
