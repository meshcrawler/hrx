package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/meshcrawler/hrx/internal/hub"
	"github.com/meshcrawler/hrx/internal/monitor"
)

func prepareHubLaunch(o LaunchOptions, l *Launch) error {
	config, _, err := loadUserConfig()
	if err != nil {
		return err
	}
	if !config.Hub.Enabled {
		return nil
	}
	identity := o.AgentIdentity
	if identity == "" {
		identity = o.YesodAgent
	}
	if !pitputAgentName.MatchString(identity) {
		return errors.New("hub-enabled launches require --agent-identity HOST/ROLE/QUALIFIER (or a saved identity)")
	}
	if os.Getenv("HRX_MONITOR") == "off" {
		return errors.New("hub ownership requires local monitoring; disable hub mode for a fully local untracked launch")
	}
	for _, token := range nativeArguments(l.Executable, o.Extra) {
		if token.option {
			option, _, _ := strings.Cut(o.Extra[token.index], "=")
			if option == "--bg" || option == "--background" || option == "--tmux" {
				return errors.New("hub ownership requires a foreground supervised harness")
			}
		}
	}
	l.agentIdentity = identity
	l.Hub = true
	if o.DryRun {
		return nil
	}
	l.hubClient, err = configuredHubClient(false)
	return err
}

// The collector gets host configuration, never the per-agent launch environment
// containing broker or model credentials. Its flock allows only one forwarder.
func ensureHubCollector(out io.Writer) error {
	store, err := monitor.Default()
	if err != nil {
		return err
	}
	dir := filepath.Join(filepath.Dir(store.Dir), "hub")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "collector.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil
		}
		return err
	}
	lock.Close()
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := filepath.Join(dir, "collector.log")
	if info, err := os.Stat(logPath); err == nil && info.Size() > 1024*1024 {
		_ = os.Rename(logPath, logPath+".previous")
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	var environment []string
	for _, name := range []string{"HOME", "PATH", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "HRX_CONFIG", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(name); ok {
			environment = append(environment, name+"="+value)
		}
	}
	cmd := exec.Command(self, "hub", "collect")
	cmd.Env = environment
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		return errors.New("cannot start HRX hub collector")
	}
	_ = cmd.Process.Release()
	fmt.Fprintln(out, "hrx: started the host's hub collector")
	return nil
}

func startHubOwnership(l *Launch, out io.Writer) (func(), error) {
	if l.monitorRun == "" {
		return nil, errors.New("hub ownership requires a recorded local run")
	}
	client := l.hubClient
	store, err := monitor.Default()
	if err != nil {
		return nil, err
	}
	host, err := store.HostID()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	lease, err := client.Acquire(ctx, l.agentIdentity, l.monitorRun)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("cannot acquire hub identity ownership: %w", err)
	}
	owned := true
	defer func() {
		if owned {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = client.Release(ctx, lease)
		}
	}()
	if lease.HostID != host {
		return nil, errors.New("hub credential belongs to another host")
	}
	deadline := start.Add(lease.ExpiresAt.Sub(lease.ServerTime) - 5*time.Second)
	if !deadline.After(time.Now()) {
		return nil, errors.New("hub lease arrived too late to start safely")
	}
	ctx, cancel = context.WithCancel(context.Background())
	done := make(chan struct{})
	if l.sidecarErrors == nil {
		l.sidecarErrors = make(chan error, 4)
	}
	failures := l.sidecarErrors
	generation := lease.Generation
	fail := func(err error) {
		select {
		case failures <- err:
		case <-ctx.Done():
		}
	}
	go func() {
		defer close(done)
		next := time.Now().Add(10 * time.Second)
		for {
			wait := time.Until(next)
			if until := time.Until(deadline); until < wait {
				wait = until
			}
			if wait < 0 {
				wait = 0
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			if !deadline.After(time.Now()) {
				fail(errors.New("hub ownership lease expired; stopping this agent"))
				return
			}
			started := time.Now()
			attempt, c := context.WithDeadline(ctx, minTime(started.Add(5*time.Second), deadline))
			renewed, err := client.Renew(attempt, lease)
			c()
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				if renewed.HostID != host {
					fail(errors.New("hub returned a different host owner"))
					return
				}
				lease = renewed
				deadline = started.Add(lease.ExpiresAt.Sub(lease.ServerTime) - 5*time.Second)
				next = time.Now().Add(10 * time.Second)
			} else if errors.Is(err, hub.ErrConflict) || errors.Is(err, hub.ErrUnauthorized) {
				fail(errors.New("hub identity ownership was lost or revoked; stopping this agent"))
				return
			} else {
				next = time.Now().Add(time.Second)
			}
		}
	}()
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			cancel()
			<-done
			ctx, c := context.WithTimeout(context.Background(), 3*time.Second)
			defer c()
			if err := client.Release(ctx, lease); err != nil {
				fmt.Fprintln(out, "hrx: hub release unavailable; the ownership lease will expire")
			}
		})
	}
	if err = ensureHubCollector(out); err != nil {
		cleanup()
		owned = false
		return nil, err
	}
	owned = false
	fmt.Fprintf(out, "hrx: hub ownership acquired for %s (generation %d)\n", l.agentIdentity, generation)
	return cleanup, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
