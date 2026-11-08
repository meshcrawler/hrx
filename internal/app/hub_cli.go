package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/meshcrawler/hrx/internal/hub"
	"github.com/meshcrawler/hrx/internal/monitor"
)

type hubPrefixes []string

func (p *hubPrefixes) String() string         { return strings.Join(*p, ",") }
func (p *hubPrefixes) Set(value string) error { *p = append(*p, value); return nil }

func runHub(ctx context.Context, args []string, out io.Writer) (err error) {
	defer func() {
		if errors.Is(err, flag.ErrHelp) {
			err = nil
		}
	}()
	if len(args) == 0 || args[0] == "--help" {
		fmt.Fprintln(out, "hrx hub serve --database-url-file FILE --admin-token-file FILE --viewer-token-file FILE [--listen 127.0.0.1:8790] [--tls-cert FILE --tls-key FILE]\nhrx hub enroll --url ORIGIN --admin-token-file FILE --prefix team/ [--prefix team/role/]\nhrx hub status [--url ORIGIN --token-file FILE]\nhrx hub collect [--once]\nhrx hub revoke --host-id ID --url ORIGIN --admin-token-file FILE\nhrx hub rotate --host-id ID --new-token-file FILE --url ORIGIN --admin-token-file FILE")
		return nil
	}
	if args[0] == "serve" {
		return serveHub(ctx, args[1:], out)
	}
	if args[0] == "collect" {
		return collectHub(ctx, args[1:], out)
	}
	f := flag.NewFlagSet("hrx hub "+args[0], flag.ContinueOnError)
	f.SetOutput(out)
	origin := f.String("url", "", "hub origin")
	admin := f.String("admin-token-file", "", "private administrator credential file")
	tokenFile := f.String("token-file", "", "private viewer or host credential file")
	hostID := f.String("host-id", "", "host installation ID")
	newToken := f.String("new-token-file", "", "replacement private credential file")
	var prefixes hubPrefixes
	f.Var(&prefixes, "prefix", "authorized identity prefix, repeatable")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected hub argument")
	}
	config, dir, err := loadUserConfig()
	if err != nil {
		return err
	}
	if *origin == "" {
		*origin = config.Hub.URL
	}
	resolve := func(path string) string {
		if path != "" && !filepath.IsAbs(path) {
			return filepath.Join(dir, path)
		}
		return path
	}
	if args[0] == "enroll" {
		if len(prefixes) == 0 || *admin == "" {
			return errors.New("hub enrollment requires --admin-token-file and at least one --prefix")
		}
		credential, err := hub.ReadPrivate(*admin)
		if err != nil {
			return err
		}
		client, err := hub.NewClient(*origin, credential)
		if err != nil {
			return err
		}
		observations, err := monitor.Default()
		if err != nil {
			return err
		}
		id, err := observations.HostID()
		if err != nil {
			return err
		}
		path := resolve(config.Hub.TokenFile)
		if path == "" {
			path = filepath.Join(dir, "hub", "host.token")
		}
		if *tokenFile != "" {
			path = resolve(*tokenFile)
		}
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		// Keep the generated token on a lost response so enrollment is retryable.
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			data := make([]byte, 32)
			if _, err = rand.Read(data); err == nil {
				_, err = file.WriteString(hex.EncodeToString(data) + "\n")
			}
			closeErr := file.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		} else if !os.IsExist(err) {
			return err
		}
		token, err := hub.ReadPrivate(path)
		if err != nil {
			return err
		}
		name, _ := os.Hostname()
		e := hub.Enrollment{Version: hub.Version, HostID: id, Name: name, Prefixes: prefixes, TokenDigest: hub.Digest(token)}
		if err = client.Call(ctx, "/api/v1/hosts/enroll", e, nil); err != nil {
			return err
		}
		if err = saveHubConfig(dir, *origin, path); err != nil {
			return err
		}
		fmt.Fprintf(out, "HRX host enrolled: %s\nHub enabled in HRX configuration. Agent launches now require an authorized --agent-identity.\n", id)
		return nil
	}
	if args[0] == "revoke" || args[0] == "rotate" {
		credential, err := hub.ReadPrivate(*admin)
		if err != nil {
			return err
		}
		client, err := hub.NewClient(*origin, credential)
		if err != nil {
			return err
		}
		body := map[string]any{"version": hub.Version, "host_id": *hostID}
		if args[0] == "rotate" {
			replacement, err := hub.ReadPrivate(*newToken)
			if err != nil {
				return err
			}
			body["token_digest"] = hub.Digest(replacement)
		}
		if err = client.Call(ctx, "/api/v1/hosts/"+args[0], body, nil); err != nil {
			return err
		}
		fmt.Fprintln(out, "Host credential policy updated.")
		return nil
	}
	if args[0] == "status" {
		path := *tokenFile
		if path == "" {
			path = config.Hub.ViewerTokenFile
		}
		credential, err := hub.ReadPrivate(resolve(path))
		if err != nil {
			return err
		}
		client, err := hub.NewClient(*origin, credential)
		if err != nil {
			return err
		}
		snapshot, err := client.Snapshot(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(snapshot)
	}
	return errors.New("unknown hub command; run hrx hub --help")
}

func configuredHubClient(viewer bool) (*hub.Client, error) {
	config, dir, err := loadUserConfig()
	if err != nil {
		return nil, err
	}
	path := config.Hub.TokenFile
	if viewer {
		path = config.Hub.ViewerTokenFile
	}
	if path != "" && !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	token, err := hub.ReadPrivate(path)
	if err != nil {
		return nil, err
	}
	return hub.NewClient(config.Hub.URL, token)
}

func collectHub(ctx context.Context, args []string, out io.Writer) error {
	f := flag.NewFlagSet("hrx hub collect", flag.ContinueOnError)
	f.SetOutput(out)
	once := f.Bool("once", false, "send current observations and exit")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected collector argument")
	}
	config, _, err := loadUserConfig()
	if err != nil {
		return err
	}
	if !config.Hub.Enabled {
		return errors.New("hub is not enabled for this host")
	}
	client, err := configuredHubClient(false)
	if err != nil {
		return err
	}
	observations, err := monitor.Default()
	if err != nil {
		return err
	}
	forwarder := &hub.Forwarder{Client: client, Monitor: observations, Dir: filepath.Join(filepath.Dir(observations.Dir), "hub")}
	defer forwarder.Close()
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	lastError := ""
	for {
		attempt, cancelAttempt := context.WithTimeout(ctx, 5*time.Second)
		err = forwarder.Open(attempt)
		cancelAttempt()
		if err == nil {
			break
		}
		if *once || errors.Is(err, hub.ErrCollectorRunning) {
			if errors.Is(err, hub.ErrCollectorRunning) {
				return nil
			}
			return err
		}
		if err.Error() != lastError {
			fmt.Fprintln(out, "hrx hub collector: waiting for authenticated hub connection")
			lastError = err.Error()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
	for {
		attempt, cancelAttempt := context.WithTimeout(ctx, 10*time.Second)
		err = forwarder.Sync(attempt)
		cancelAttempt()
		if *once {
			return err
		}
		if err != nil && err.Error() != lastError {
			fmt.Fprintln(out, "hrx hub collector: forwarding interrupted; durable observations will retry")
			lastError = err.Error()
		}
		if err == nil {
			lastError = ""
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
}

func saveHubConfig(dir, origin, tokenPath string) error {
	path := os.Getenv("HRX_CONFIG")
	if path == "" {
		path = filepath.Join(dir, "config.toml")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	var config map[string]any
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(data) > 0 && toml.Unmarshal(data, &config) != nil {
		return errors.New("cannot update HRX configuration")
	}
	if config == nil {
		config = map[string]any{}
	}
	section, _ := config["hub"].(map[string]any)
	if section == nil {
		section = map[string]any{}
	}
	section["enabled"], section["url"], section["token_file"] = true, origin, tokenPath
	config["hub"] = section
	data, err = toml.Marshal(config)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".hub-config-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func serveHub(ctx context.Context, args []string, out io.Writer) error {
	f := flag.NewFlagSet("hrx hub serve", flag.ContinueOnError)
	f.SetOutput(out)
	database := f.String("database-url-file", "", "private PostgreSQL connection string file")
	adminFile := f.String("admin-token-file", "", "private administrator credential file")
	viewerFile := f.String("viewer-token-file", "", "private viewer credential file")
	listen := f.String("listen", "127.0.0.1:8790", "listen address")
	cert, key := f.String("tls-cert", "", "TLS certificate"), f.String("tls-key", "", "TLS key")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected hub serve argument")
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return errors.New("hub listen requires IP:port")
	}
	ip := net.ParseIP(host)
	if (*cert == "") != (*key == "") {
		return errors.New("supply both TLS certificate and key")
	}
	if (ip == nil || !ip.IsLoopback()) && *cert == "" {
		return errors.New("non-loopback hubs require TLS; use a loopback tunnel otherwise")
	}
	dsn, err := hub.ReadPrivate(*database)
	if err != nil {
		return err
	}
	admin, err := hub.ReadPrivate(*adminFile)
	if err != nil {
		return err
	}
	viewer, err := hub.ReadPrivate(*viewerFile)
	if err != nil {
		return err
	}
	if len(admin) < 32 || len(viewer) < 32 {
		return errors.New("hub tokens must have at least 32 characters")
	}
	startup, cancelStartup := context.WithTimeout(ctx, 10*time.Second)
	defer cancelStartup()
	store, err := hub.Open(startup, dsn)
	if err != nil {
		return err
	}
	defer store.Pool.Close()
	api, err := hub.NewServer(store, hub.Digest(admin), hub.Digest(viewer))
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	server := &http.Server{Handler: api, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	maintenanceDone := make(chan struct{})
	go func() {
		defer close(maintenanceDone)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			attempt, stop := context.WithTimeout(ctx, 10*time.Second)
			err := store.Compact(attempt, 30*24*time.Hour)
			stop()
			if err != nil && ctx.Err() == nil {
				fmt.Fprintln(out, "hrx hub: history compaction unavailable; retrying")
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	defer func() { cancel(); <-maintenanceDone }()
	go func() {
		<-ctx.Done()
		stop, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = server.Shutdown(stop)
	}()
	fmt.Fprintf(out, "HRX hub listening on %s (API v%d)\n", listener.Addr(), hub.Version)
	if *cert != "" {
		err = server.ServeTLS(listener, *cert, *key)
	} else {
		err = server.Serve(listener)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
