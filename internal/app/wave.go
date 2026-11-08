package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

var waveUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)

// waveCommand never exposes diagnostics (which can contain Wave authentication
// details). Both execution and output are bounded; this adapter is optional.
func waveCommand(ctx context.Context, executable string, input []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 100 * time.Millisecond
	output := &waveOutput{}
	cmd.Stdout = output
	err := cmd.Run()
	return output.Bytes(), err
}

type waveOutput struct{ bytes.Buffer }

func (b *waveOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 64*1024 - b.Len(); remaining > 0 {
		b.Buffer.Write(p[:min(remaining, n)])
	}
	return n, nil
}

// Require a local Wave process ancestor as well as matching authenticated block
// context. Inherited variables in SSH, CI, or another terminal are insufficient.
func localWaveAncestor(ctx context.Context) bool {
	pid := os.Getppid()
	for i := 0; i < 16 && pid > 1; i++ {
		out, err := waveCommand(ctx, "ps", nil, "-p", strconv.Itoa(pid), "-o", "ppid=", "-o", "comm=")
		if err != nil {
			return false
		}
		fields := strings.Fields(string(out))
		if len(fields) < 2 {
			return false
		}
		name := filepath.Base(strings.Join(fields[1:], " "))
		if name == "wavesrv" || name == "wavesrv.arm64" || name == "wavesrv.x64" {
			return true
		}
		pid, err = strconv.Atoi(fields[0])
		if err != nil {
			return false
		}
	}
	return false
}

func waveEnvironment(getenv func(string) string) (string, string, bool) {
	block, client := getenv("WAVETERM_BLOCKID"), getenv("WAVETERM_CLIENTID")
	if !waveUUID.MatchString(block) || !waveUUID.MatchString(client) || getenv("SSH_CONNECTION") != "" || getenv("SSH_TTY") != "" || getenv("CI") != "" {
		return "", "", false
	}
	token := getenv("WAVETERM_JWT")
	if len(token) > 16384 {
		return "", "", false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", false
	}
	var claims struct {
		Block string `json:"blockid"`
		Conn  string `json:"conn"`
	}
	// This is a consistency check, not authentication. wsh validates the JWT with
	// the live Wave server before either metadata operation succeeds.
	if json.Unmarshal(raw, &claims) != nil || claims.Block != block || claims.Conn != "" {
		return "", "", false
	}
	return block, client, true
}

func labelWavePane(l Launch) {
	block, client, ok := waveEnvironment(os.Getenv)
	if !ok {
		return
	}
	var group int32
	if terminalGroup(os.Stdin.Fd(), syscall.TIOCGPGRP, &group) != nil {
		return
	}
	config, _, err := loadUserConfig()
	if err != nil || config.Wave.Enabled != nil && !*config.Wave.Enabled {
		return
	}
	path, err := exec.LookPath("wsh")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	if !localWaveAncestor(ctx) {
		return
	}
	unlock := lockWavePane(block)
	if unlock == nil {
		return
	}
	defer unlock()
	applyWaveLabel(ctx, l, block, client, func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
		return waveCommand(ctx, path, input, args...)
	})
}

type waveCall func(context.Context, []byte, ...string) ([]byte, error)

func applyWaveLabel(ctx context.Context, l Launch, block, client string, call waveCall) {
	// A live client object confirms that the client identifier is not stale.
	if _, err := call(ctx, nil, "getmeta", "-b", "client:"+client); err != nil {
		return
	}
	raw, err := call(ctx, nil, "getmeta", "-b", "block:"+block, "view", "frame:text", "hrx:wave")
	if err != nil {
		return
	}
	var meta map[string]any
	if json.Unmarshal(raw, &meta) != nil || meta["view"] != "term" {
		return
	}
	// Never replace an explicitly named pane, including an earlier HRX label.
	// Wave has no compare-and-swap metadata API: avoiding exit-time writes also
	// guarantees cleanup cannot overwrite a user's subsequent edits.
	if title, exists := meta["frame:text"]; exists && title != nil && title != "" {
		return
	}
	if meta["hrx:wave"] != nil {
		return
	}
	agent := l.agentIdentity
	if l.Yesod != nil {
		agent = l.Yesod.Agent
	}
	if l.pitputRuntime != nil {
		agent = l.pitputRuntime.identity
	}
	if l.Broker != nil {
		agent = l.Broker.Identity
	}
	if agent == "" {
		return
	}
	title := waveRole(agent) + " | " + waveModel(l.Identity) + " | " + waveHarness(l.Executable)
	binding := map[string]string{"agent": waveText(agent), "harness": waveText(l.Executable), "model": waveText(l.Identity), "block": block, "client": client, "run": l.monitorRun}
	if l.pendingSession != nil {
		binding["session"] = waveText(l.pendingSession.NativeID)
	}
	// Normal terminal headers hide frame:title. frame:text is the visible
	// HeaderTextElems override; leave connection and other frame metadata alone.
	patch, _ := json.Marshal(map[string]any{"frame:text": title, "hrx:wave": binding})
	_, _ = call(ctx, patch, "setmeta", "-b", "block:"+block, "--json", "-")
}

func waveText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if b.Len()+len(string(r)) > 160 {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}
func waveHarness(s string) string {
	switch s {
	case "opencode":
		return "OpenCode"
	case "codex":
		return "Codex"
	case "claude":
		return "Claude"
	}
	return waveText(s)
}
func waveRole(identity string) string {
	parts := strings.Split(strings.Trim(identity, "/"), "/")
	role := parts[len(parts)-1]
	if role == "agent" && len(parts) > 1 {
		role = parts[len(parts)-2]
	}
	// [wave.role_labels] maps a role fragment to a display label; the longest
	// fragment contained in the role wins. Unmatched roles show as themselves.
	if config, _, err := loadUserConfig(); err == nil {
		best := ""
		for fragment := range config.Wave.RoleLabels {
			if fragment != "" && strings.Contains(role, fragment) && (len(fragment) > len(best) || (len(fragment) == len(best) && fragment < best)) {
				best = fragment
			}
		}
		if best != "" {
			return waveText(config.Wave.RoleLabels[best])
		}
	}
	return waveText(role)
}

// Serialize HRX launches targeting one pane without delaying another launch.
func lockWavePane(block string) func() {
	root, err := os.UserCacheDir()
	if err != nil {
		return nil
	}
	root = filepath.Join(root, "hrx", "wave-locks")
	if os.MkdirAll(root, 0700) != nil {
		return nil
	}
	fd, err := syscall.Open(filepath.Join(root, block+".lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil
	}
	if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		syscall.Close(fd)
		return nil
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = syscall.Close(fd) }
}

func waveModel(s string) string {
	if s == "kimi-k3" {
		return "Kimi K3"
	}
	return waveText(s)
}
