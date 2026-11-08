package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/meshcrawler/hrx/internal/monitor"
	"github.com/meshcrawler/hrx/internal/session"
)

func trackLaunch(l *Launch) {
	env := l.Env
	if env == nil {
		env = os.Environ()
	}
	l.Env = childEnvironment(env, nil, []string{monitor.RunEnv})
	if l.Executable != "claude" && l.Executable != "codex" && l.Executable != "opencode" {
		return
	}
	if os.Getenv("HRX_MONITOR") == "off" {
		return
	}
	// Detached native runtimes do not share the launcher's lifetime. Until a
	// native process binding is available, leave them as session history only.
	for _, token := range nativeArguments(l.Executable, l.Passthrough) {
		if !token.option {
			continue
		}
		name, _, _ := strings.Cut(l.Passthrough[token.index], "=")
		if name == "--bg" || name == "--background" || name == "--tmux" {
			return
		}
	}
	store, err := monitor.Default()
	if err != nil {
		return
	}
	r := monitor.Record{Harness: l.Executable, Model: l.Identity, Agent: "Unnamed agent", Credential: monitor.Credential{Provider: "unmanaged"}}
	r.Pitput = monitor.NewPitputInfo(l.Pitput, "", nil, "configured")
	// Claude's SessionStart settings carry launch metadata in the child
	// environment rather than pendingSession. Read only that explicit field.
	if l.Pitput {
		for _, entry := range l.Env {
			if metadata, ok := strings.CutPrefix(entry, sessionMetadataEnv+"="); ok && len(metadata) <= 16384 {
				var saved session.Record
				if json.Unmarshal([]byte(metadata), &saved) == nil && saved.Pitput {
					r.Pitput = monitor.NewPitputInfo(true, saved.AgentIdentity, saved.PitputBoards, "configured")
				}
			}
		}
	}
	if l.pendingSession != nil && l.pendingSession.Pitput {
		r.Pitput = monitor.NewPitputInfo(true, l.pendingSession.AgentIdentity, l.pendingSession.PitputBoards, "configured")
	}
	monitor.BindTelemetry(&r, l.Env)
	if l.Executable == "claude" {
		for _, token := range nativeArguments("claude", l.Passthrough) {
			if !token.option {
				continue
			}
			name, value, equal := strings.Cut(l.Passthrough[token.index], "=")
			if name != "--name" {
				continue
			}
			if !equal && token.index+1 < len(l.Passthrough) {
				value = l.Passthrough[token.index+1]
			}
			if value != "" && len(value) <= 256 {
				r.Agent = value
			}
		}
	}
	if l.Yesod != nil {
		r.Agent = l.Yesod.Agent
	}
	if l.agentIdentity != "" {
		r.Agent = l.agentIdentity
	}
	if l.pitputRuntime != nil {
		r.Agent = l.pitputRuntime.identity
		r.Pitput = monitor.NewPitputInfo(true, l.pitputRuntime.identity, l.pitputRuntime.boards, "joined")
	}
	if l.Broker != nil {
		r.Agent = l.Broker.Identity
		r.Credential = l.Broker.Credential
	}
	if l.pendingSession != nil {
		r.SessionID = l.pendingSession.NativeID
	}
	r, err = store.Register(r)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hrx: local dashboard observation unavailable")
		return
	}
	l.Env = childEnvironment(l.Env, map[string]string{monitor.RunEnv: r.RunID}, nil)
	l.monitorRun = r.RunID
}

func finishLaunch(l Launch) {
	if l.monitorRun == "" {
		return
	}
	store, err := monitor.Default()
	if err == nil {
		err = store.Finish(l.monitorRun)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hrx: could not record session exit; dashboard will detect process termination")
	}
}

func recordActivity(event, id string) {
	run := os.Getenv(monitor.RunEnv)
	if run == "" {
		return
	}
	store, err := monitor.Default()
	if err != nil {
		return
	}
	_ = store.Activity(run, event, id)
}

type sourceFlags map[string]string

func (s sourceFlags) String() string { return "NAME=URL" }
func (s sourceFlags) Set(value string) error {
	name, origin, ok := strings.Cut(value, "=")
	if !ok || !brokerUsername.MatchString(name) || name == "local" {
		return errors.New("source must be NAME=URL; local is reserved")
	}
	if _, exists := s[name]; exists {
		return errors.New("duplicate source name")
	}
	if err := monitor.ValidateSource(origin); err != nil {
		return err
	}
	s[name] = origin
	return nil
}

func runDashboard(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("hrx dashboard", flag.ContinueOnError)
	flags.SetOutput(out)
	listen := flags.String("listen", "127.0.0.1:8789", "loopback listen address")
	useHub := flags.Bool("hub", false, "show the configured hub using its private viewer credential")
	sources := sourceFlags{}
	flags.Var(sources, "source", "additional collector NAME=URL (repeatable; SSH tunnel or HTTPS)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected dashboard argument")
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return errors.New("dashboard listen must be a loopback IP:port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("dashboard only binds loopback; remote hosting requires an authenticated hub")
	}
	store, err := monitor.Default()
	if err != nil {
		return err
	}
	hostID, err := store.HostID()
	if err != nil {
		return errors.New("cannot initialize dashboard state")
	}
	hostname, _ := os.Hostname()
	local := func(ctx context.Context) ([]monitor.Record, error) {
		if err := store.Recover(ctx); err != nil {
			return nil, err
		}
		rows, err := store.Snapshot(ctx)
		if err != nil {
			return rows, err
		}
		seen := map[string]bool{}
		for _, r := range rows {
			if r.SessionID != "" {
				seen[r.Harness+"/"+r.SessionID] = true
			}
		}
		sessions, e := session.Default()
		if e != nil {
			return rows, e
		}
		history, e := sessions.List()
		if e != nil {
			return rows, e
		}
		for _, h := range history {
			if seen[h.Harness+"/"+h.NativeID] {
				continue
			}
			agent := h.Agent
			if h.AgentIdentity != "" {
				agent = h.AgentIdentity
			}
			if agent == "" {
				agent = "Identity not recorded"
			}
			var forums *monitor.PitputInfo
			if h.Pitput {
				forums = monitor.NewPitputInfo(true, h.AgentIdentity, h.PitputBoards, "configured")
			}
			rows = append(rows, monitor.Record{RunID: "history-" + h.Harness + "-" + h.NativeID, HostID: hostID, Host: hostname, Agent: agent, Harness: h.Harness, Model: h.Identity, SessionID: h.NativeID, StartedAt: h.CreatedAt, LastSeen: h.UpdatedAt, LastActivity: h.UpdatedAt, Activity: "historical session", Status: "untracked", Credential: monitor.Credential{Provider: "unknown"}, Pitput: forums})
		}
		return rows, nil
	}
	collector := &monitor.Collector{Local: local, Peers: sources}
	if *useHub {
		if _, exists := sources["hub"]; exists {
			return errors.New("source name hub is reserved with --hub")
		}
		client, err := configuredHubClient(true)
		if err != nil {
			return err
		}
		// Hub already includes this installation. Avoid showing each local run
		// twice, and keep the local-only endpoint from exporting pooled data.
		collector.Local = func(context.Context) ([]monitor.Record, error) { return nil, nil }
		collector.Fetchers = map[string]func(context.Context) (monitor.Snapshot, error){"hub": client.Snapshot}
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	collector.Refresh(ctx)
	go collector.Run(ctx)
	server := &http.Server{Handler: collector.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		stop, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = server.Shutdown(stop)
	}()
	fmt.Fprintf(out, "HRX dashboard: http://%s\nCredentials are metadata only. Refreshes every 3 seconds.\n", listener.Addr())
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
