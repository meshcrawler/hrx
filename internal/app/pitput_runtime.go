package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/meshcrawler/hrx/internal/coveragecontroller"
	"github.com/meshcrawler/hrx/internal/monitor"
	"github.com/meshcrawler/hrx/internal/session"
)

var pitputBoardName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
var pitputAgentName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var pitputThreadID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// pitputDMKey names the direct-message bridge's directories. It can never collide
// with a board, because board names cannot start with "@".
const pitputDMKey = "@dm"

type pitputRuntime struct {
	// Supplied only by the disposable acceptance test; nil on normal launches.
	attentionTestHook func(context.Context, coveragecontroller.AttentionTestPoint) error
	identity          string
	boards            []string
	combined          bool
	admissionBinary   string
	targeted          bool
	followed          []string
	record            session.Record
	cli               string
	// dm delivers this identity's private direct messages through one more bridge.
	dm       bool
	dmNotice string
}

func preparePitputRuntime(ctx context.Context, o LaunchOptions, l *Launch) error {
	if err := validatePitputSelection(o, l.Executable); err != nil {
		return err
	}
	if !o.Pitput || (l.Executable != "codex" && l.Executable != "opencode" && l.Executable != "claude") || o.DryRun {
		return nil
	}
	if l.Executable == "claude" {
		for _, token := range nativeArguments("claude", o.Extra) {
			name, _, _ := strings.Cut(o.Extra[token.index], "=")
			if token.option && (name == "--print" || name == "-p") {
				return nil // Noninteractive CLI exchanges do not own background tasks.
			}
			if token.option && (name == "--fork-session" || name == "--continue" || name == "-c") {
				return errors.New("Claude Pitput requires an interactive session with an exact identity; use hrx resume for continuation")
			}
		}
	}
	if l.Executable == "opencode" && nativeCommand("opencode", o.Extra) == "run" {
		return errors.New("automatic Pitput delivery requires an interactive OpenCode session")
	}
	if l.Executable == "codex" && (nativeCommand("codex", o.Extra) == "exec" || nativeCommand("codex", o.Extra) == "e") {
		return errors.New("automatic Pitput delivery requires an interactive Codex session")
	}
	if l.Executable == "codex" && nativeHasOption("codex", o.Extra, "--no-daemon") {
		return errors.New("--pitput requires its private Codex server; remove explicit --no-daemon")
	}
	identity := o.AgentIdentity
	if identity == "" {
		identity = o.YesodAgent
	}
	if !pitputAgentName.MatchString(identity) {
		return errors.New("--pitput requires --agent-identity HOST/ROLE/QUALIFIER (or a saved identity on resume)")
	}
	cli, err := exec.LookPath("pitput")
	if err != nil {
		return errors.New("--pitput requires the Pitput CLI")
	}
	r := &pitputRuntime{identity: identity, cli: cli, targeted: o.PitputTargeted,
		combined: o.PitputCombinedPreview,
		followed: slices.Compact(slices.Sorted(slices.Values(o.PitputFollowThreads)))}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var status struct {
		Identity string
		Boards   []string
		Status   string
	}
	readStatus := func() bool {
		var output limitedBuffer
		cmd := exec.CommandContext(ctx, cli, "agent", "status", "--agent-id="+identity)
		cmd.Stdout, cmd.Stderr = &output, io.Discard
		return cmd.Run() == nil && json.Unmarshal(output.Bytes(), &status) == nil && status.Identity == identity && status.Status == "ready"
	}
	ready := readStatus()
	granted := ready
	for _, board := range o.PitputBoards {
		granted = granted && slices.Contains(status.Boards, board)
	}
	if len(o.PitputBoards) > 0 && !granted {
		args := []string{"join"}
		for _, board := range o.PitputBoards {
			args = append(args, "/b/"+board)
		}
		args = append(args, "--identity", identity)
		cmd := exec.CommandContext(ctx, cli, args...)
		diagnostic := &nativeDiagnosticTail{}
		cmd.Stdout, cmd.Stderr = io.Discard, diagnostic
		if cmd.Run() != nil {
			return errors.New("Pitput enrollment failed; check machine enrollment authority and requested boards")
		}
		ready = readStatus()
	}
	if !ready && o.PitputDM == "only" {
		cmd := exec.CommandContext(ctx, cli, "join", "--dm-only", "--identity", identity)
		diagnostic := &nativeDiagnosticTail{}
		cmd.Stdout, cmd.Stderr = io.Discard, diagnostic
		if cmd.Run() != nil {
			return errors.New("Pitput DM-only enrollment failed; the machine enrollment authority must allow DM-only identities")
		}
		ready = readStatus()
	}
	if !ready {
		return errors.New("Pitput identity is not ready; specify --pitput-board BOARD (or --pitput-dm-only) to enroll it")
	}
	if o.PitputDM != "only" {
		r.boards = o.PitputBoards
		if len(r.boards) == 0 {
			r.boards = status.Boards
		}
	}
	r.boards = slices.Compact(slices.Sorted(slices.Values(r.boards)))
	for _, topic := range r.followed {
		if !slices.Contains(r.boards, strings.Split(topic, "/")[2]) {
			return errors.New("followed Pitput topic must belong to a monitored board")
		}
	}
	for _, board := range r.boards {
		if !pitputBoardName.MatchString(board) || !slices.Contains(status.Boards, board) {
			return errors.New("Pitput board is invalid or not enrolled")
		}
	}
	r.dm = o.PitputDM != "off"
	if r.dm && !pitputDirectMessagesAvailable(ctx, cli, identity) {
		if len(r.boards) == 0 {
			return errors.New("Pitput direct messages are unavailable for this identity (server or CLI lacks dm.v1); specify --pitput-board BOARD")
		}
		// Direct messages are on by default, so an older server must not break board delivery.
		r.dm, r.dmNotice = false, "hrx: Pitput direct messages unavailable (server or CLI lacks dm.v1); delivering boards only"
	}
	if len(r.boards) == 0 && !r.dm {
		return errors.New("Pitput requires at least one board; specify --pitput-board BOARD")
	}
	l.pitputRuntime = r
	return nil
}

// A read-only inbox listing proves the CLI has `dm` and the server advertises dm.v1;
// any failure means direct messages are unavailable, never a reason to guess.
func pitputDirectMessagesAvailable(ctx context.Context, cli, identity string) bool {
	cmd := exec.CommandContext(ctx, cli, "--agent", identity, "dm", "inbox", "--unhandled")
	cmd.Stdout, cmd.Stderr, cmd.WaitDelay = io.Discard, io.Discard, 100*time.Millisecond
	return cmd.Run() == nil
}

func (r *pitputRuntime) targets() string {
	targets := slices.Clone(r.boards)
	if r.dm {
		targets = append(targets, "direct messages")
	}
	return strings.Join(targets, ", ")
}

// directMessageConfig writes the identity's one direct-message bridge entry.
func (r *pitputRuntime) directMessageConfig(root, id string, config bridgeConfig) (string, error) {
	return r.directMessageConfigState(root, root, id, config)
}

func (r *pitputRuntime) directMessageConfigState(root, stateRoot, id string, config bridgeConfig) (string, error) {
	dir := filepath.Join(root, id, pitputDMKey)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	state, err := r.bridgeState(stateRoot, pitputDMKey)
	if err != nil {
		return "", err
	}
	config.StateDirectory = state
	config.Agents = []bridgeAgent{{AgentID: r.identity, ThreadID: id, Source: "dm"}}
	return writeBridgeConfig(dir, config)
}

type pitputProcess struct {
	diagnostic *nativeDiagnosticTail
	cmd        *exec.Cmd
	done       chan error
}

func startPitputProcess(ctx context.Context, binary string, args, env []string, dir string) (*pitputProcess, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env, cmd.Dir = env, dir
	diagnostic := &nativeDiagnosticTail{}
	cmd.Stdout, cmd.Stderr = io.Discard, diagnostic
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &pitputProcess{cmd: cmd, done: make(chan error, 1), diagnostic: diagnostic}
	go func() { p.done <- cmd.Wait() }()
	return p, nil
}

// The installed Pitput bridge targets unix://. This private, credential-free
// adapter pins that existing queue contract to this launch's isolated socket.
func pitputQueueAdapter(binary, endpoint string) string {
	return "#!/bin/sh\nset -eu\n[ \"$#\" -ge 3 ] && [ \"$1\" = queue ] && [ \"$2\" = --remote ] && [ \"$3\" = unix:// ] || exit 64\nshift 3\nexec " + shellWord(binary) + " queue --remote " + shellWord(endpoint) + " \"$@\"\n"
}

// Remote Codex resume rejects explicit TUI permission overrides. Apply the
// requested yolo permissions to the owned server, then let resume inherit them.
func pitputTerminalArgs(args []string) []string {
	if !nativeResume("codex", args) {
		return slices.Clone(args)
	}
	drop := map[int]bool{}
	for _, token := range nativeArguments("codex", args) {
		if token.option && (args[token.index] == "--yolo" || args[token.index] == "--dangerously-bypass-approvals-and-sandbox") {
			drop[token.index] = true
		}
	}
	result := make([]string, 0, len(args))
	for i, arg := range args {
		if !drop[i] {
			result = append(result, arg)
		}
	}
	return result
}

func pitputServerArgs(args []string) []string {
	server := []string{"app-server"}
	yolo := false
	for _, token := range nativeArguments("codex", args) {
		if !token.option {
			continue
		}
		i := token.index
		if args[i] == "--yolo" || args[i] == "--dangerously-bypass-approvals-and-sandbox" {
			yolo = true
			continue
		}
		name, value, equal := strings.Cut(args[i], "=")
		if name != "-c" && name != "--config" && name != "--enable" && name != "--disable" {
			continue
		}
		if !equal {
			if i+1 >= len(args) {
				continue
			}
			value = args[i+1]
		}
		if name == "--config" {
			name = "-c"
		}
		server = append(server, name, value)
	}
	// Typed native flags override generic -c configuration regardless of order.
	if yolo {
		server = append(server, "-c", `approval_policy="never"`, "-c", `sandbox_mode="danger-full-access"`)
	}
	return server
}

// The coverage owner receives an uninitialized socket and owns the native
// handshake. No production discovery connection competes for responses.
func connectPitputSocket(ctx context.Context, socket string) (*websocket.Conn, error) {
	d := websocket.Dialer{NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}, HandshakeTimeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, response, err := d.DialContext(ctx, "ws://localhost/", http.Header{})
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("private Codex server did not become ready")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func startPitputRuntime(l *Launch, receipt string, out io.Writer) (func(), error) {
	r := l.pitputRuntime
	if r.dmNotice != "" {
		fmt.Fprintln(out, r.dmNotice)
	}
	store, err := session.Default()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(filepath.Dir(store.Dir), "pitput", fmt.Sprintf("%x", sha256.Sum256([]byte(r.identity))))
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root, "launch.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("this Pitput identity already has an HRX session running")
	}
	// Short paths keep Unix sockets within the macOS sockaddr limit.
	tmp, err := os.MkdirTemp("/tmp", "hrx-pitput-")
	if err != nil {
		lock.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	var processes []*pitputProcess
	var coverage *coveragecontroller.Runtime
	workerDone := make(chan struct{})
	workerStarted := false
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			cancel()
			joinedCoverage := true
			if coverage != nil {
				stop, done := context.WithTimeout(context.Background(), 6*time.Second)
				if err := coverage.Close(stop); err != nil {
					joinedCoverage = false
					fmt.Fprintln(out, "hrx: coverage cleanup could not be verified; retaining owned runtime files")
				}
				done()
			}
			if workerStarted {
				<-workerDone
			}
			for _, p := range processes {
				_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
				select {
				case <-p.done:
				case <-time.After(4 * time.Second):
					_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
					select {
					case <-p.done:
					case <-time.After(4 * time.Second):
						joinedCoverage = false
						fmt.Fprintln(out, "hrx: Pitput child exit could not be verified; retaining owned runtime files")
					}
				}
			}
			if joinedCoverage {
				os.RemoveAll(tmp)
			}
			lock.Close()
		})
	}
	ok := false
	defer func() {
		if !ok {
			cleanup()
		}
	}()
	// Capture new-board history before the harness can post or receive its prompt.
	// This preparation never transfers ownership of an existing delivery.
	canonicalTmp, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		return nil, err
	}
	var bootstrap []string
	if r.combined && l.Executable == "codex" {
		binary, lookupErr := exec.LookPath("codex")
		if lookupErr != nil {
			return nil, lookupErr
		}
		binary, lookupErr = filepath.EvalSymlinks(binary)
		if lookupErr != nil {
			return nil, lookupErr
		}
		bootstrap, err = r.combinedConfig(root, binary,
			"unix://"+filepath.Join(canonicalTmp, "bootstrap-unavailable.sock"),
			"00000000-0000-0000-0000-000000000000", strings.Repeat("0", 64))
	} else {
		bootstrap, err = r.openCodeBridgeConfigs(root, "http://127.0.0.1:1", filepath.Join(canonicalTmp, "bootstrap-unused-password"), "ses_hrx_bootstrap")
	}
	if err != nil {
		return nil, err
	}
	for _, config := range bootstrap {
		if err := r.prepareBridge(ctx, config, true); err != nil {
			return nil, err
		}
	}
	if l.Executable == "claude" {
		if err := startClaudePitput(ctx, l, root, out); err != nil {
			return nil, err
		}
		ok = true
		return cleanup, nil
	}
	if l.Executable == "opencode" {
		processes, err = startOpenCodePitput(ctx, l, root, tmp, receipt, out)
		if err != nil {
			return nil, err
		}
		if l.sidecarErrors == nil {
			l.sidecarErrors = make(chan error, 4)
		}
		failures := l.sidecarErrors
		workerStarted = true
		go func() {
			defer close(workerDone)
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
				for _, p := range processes {
					select {
					case err := <-p.done:
						p.done <- err
						if ctx.Err() == nil {
							select {
							case failures <- errors.New("Pitput bridge or private OpenCode server exited; resume with hrx to reconnect"):
							case <-ctx.Done():
							}
						}
						return
					default:
					}
				}
			}
		}()
		ok = true
		return cleanup, nil
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		return nil, err
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return nil, err
	}
	profileInstructions, err := codexProfileInstructions(l.Args, l.Env, r.record.Project)
	if err != nil {
		return nil, err
	}
	socket := filepath.Join(tmp, "server.sock")
	endpoint := "unix://" + socket
	args := append(pitputServerArgs(l.Args), "--listen", endpoint)
	p, err := startPitputProcess(ctx, binary, args, l.Env, r.record.Project)
	if err != nil {
		return nil, errors.New("cannot start private Codex server")
	}
	processes = append(processes, p)
	conn, err := connectPitputSocket(ctx, socket)
	if err != nil {
		return nil, err
	}
	journalRoot := filepath.Join(root, "coverage-journal")
	if err := os.MkdirAll(journalRoot, 0700); err != nil {
		conn.Close()
		return nil, err
	}
	coverage, err = coveragecontroller.NewRuntime(ctx, conn, coveragecontroller.RuntimeOptions{VerifyOwnedPending: r.pendingVerifier(root, l.Env, r.record.Project), CombinedResumeDrain: r.combined, AttentionTestHook: r.attentionTestHook, ExpectedThreadID: r.record.NativeID, ExpectedModel: r.record.Route, ExpectedWorkingDirectory: r.record.Project, ProfileDeveloperInstructions: profileInstructions, SocketRoot: tmp, JournalRoot: journalRoot, Name: r.identity})
	if err != nil {
		return nil, err
	}
	l.Args = slices.DeleteFunc(l.Args, func(arg string) bool { return arg == "--no-daemon" })
	l.Args = append([]string{"--remote", coverage.Terminal}, pitputTerminalArgs(l.Args)...)
	if l.sidecarErrors == nil {
		l.sidecarErrors = make(chan error, 4)
	}
	failures := l.sidecarErrors
	workerStarted = true
	go func() {
		defer close(workerDone)
		fail := func(message string) {
			if ctx.Err() == nil {
				failures <- errors.New(message)
			}
		}
		identity, err := coverage.Ready(ctx)
		if err != nil {
			fail(fmt.Sprintf("Pitput coverage could not admit the native Codex session: %v (%s)", err, p.diagnosticSummary()))
			return
		}
		id := identity.ThreadID
		if !pitputThreadID.MatchString(id) || r.record.NativeID != "" && r.record.NativeID != id {
			fail("Pitput coverage returned a different native session")
			return
		}
		record := r.record
		record.NativeID = id
		if store.Save(record) != nil {
			fail("Pitput could not save session binding")
			return
		}
		for _, entry := range l.Env {
			if run, ok := strings.CutPrefix(entry, monitor.RunEnv+"="); ok {
				if observations, err := monitor.Default(); err == nil {
					_ = observations.Activity(run, "session-start", id)
				}
			}
		}
		if receipt != "" {
			_ = os.WriteFile(receipt, []byte(id), 0600)
		}
		var configs []string
		if r.combined {
			configs, err = r.combinedConfig(root, binary, coverage.Board, id, coverage.AttentionBinding())
		} else {
			configs, err = r.bridgeConfigsForEndpoints(root, binary, coverage.Board, coverage.DirectMessage, id)
		}
		if err != nil {
			fail("Pitput could not prepare bridge configuration")
			return
		}
		for _, config := range configs {
			if err := r.prepareBridge(ctx, config, false); err != nil {
				fail(err.Error())
				return
			}
			p, err := startPitputProcess(ctx, r.cli, []string{"bridge", "run", "--config", config}, l.Env, r.record.Project)
			if err != nil {
				fail("Pitput bridge failed to start")
				return
			}
			processes = append(processes, p)
		}
		fmt.Fprintf(out, "hrx: Pitput bridge connected: %s -> %s (%s)\n", r.identity, id, r.targets())
		// Controller loss revokes both terminal and queued input immediately.
		for {
			select {
			case <-ctx.Done():
				return
			case <-coverage.Done():
				message := "Pitput coverage controller stopped; native input admission was revoked"
				if cause, stopped := coverage.StoppedCause(); stopped && cause != nil {
					message = fmt.Sprintf("%s: %v", message, cause)
				}
				fail(message)
				return
			case <-time.After(time.Second):
			}
			for _, p := range processes {
				select {
				case err := <-p.done:
					p.done <- err
					fail("Pitput bridge or private Codex server exited; resume with hrx to reconnect")
					return
				default:
				}
			}
		}
	}()
	ok = true
	return cleanup, nil
}

func (r *pitputRuntime) bridgeConfigs(root, binary, endpoint, id string) ([]string, error) {
	return r.bridgeConfigsForEndpoints(root, binary, endpoint, endpoint, id)
}

// The controller assigns ingress identity by socket, so board and DM adapters
// must retain their separate endpoints even when they target the same thread.
func (r *pitputRuntime) bridgeConfigsForEndpoints(root, binary, boardEndpoint, dmEndpoint, id string) ([]string, error) {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(user, ".codex")
	}
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, board := range r.boards {
		dir := filepath.Join(root, id, board)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		adapter := filepath.Join(dir, "codex-queue")
		if err := os.WriteFile(adapter, []byte(pitputQueueAdapter(binary, boardEndpoint)), 0700); err != nil {
			return nil, err
		}
		state, err := r.bridgeState(root, board)
		if err != nil {
			return nil, err
		}
		observer, err := json.Marshal(codexObserveRequest{Endpoint: boardEndpoint, Session: id, Project: r.record.Project})
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, "native-observer.json"), observer, 0600); err != nil {
			return nil, err
		}
		path, err := writeBridgeConfig(dir, bridgeConfig{CodexBinary: adapter, CodexHome: home, StateDirectory: state,
			RetrySeconds: 2, Agents: []bridgeAgent{r.boardAgent(board, id)}})
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	if r.dm {
		adapter := filepath.Join(root, id, pitputDMKey, "codex-queue")
		if err := os.MkdirAll(filepath.Dir(adapter), 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(adapter, []byte(pitputQueueAdapter(binary, dmEndpoint)), 0700); err != nil {
			return nil, err
		}
		path, err := r.directMessageConfig(root, id, bridgeConfig{CodexBinary: adapter, CodexHome: home, RetrySeconds: 2})
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}
