package monitor

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var assets embed.FS

type Source struct {
	ID          string    `json:"id"`
	Status      string    `json:"status"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	Error       string    `json:"error,omitempty"`
}
type Row struct {
	Record
	Source       string     `json:"source"`
	SourceStatus string     `json:"source_status"`
	Ownership    *Ownership `json:"ownership,omitempty"`
}
type Ownership struct {
	State      string     `json:"state"`
	Generation int64      `json:"generation"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}
type Snapshot struct {
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	Agents      []Row     `json:"agents"`
	Sources     []Source  `json:"sources"`
}
type Collector struct {
	refreshMu sync.Mutex
	Local     func(context.Context) ([]Record, error)
	Peers     map[string]string
	// Fetchers keep authenticated source credentials in the local process.
	Fetchers map[string]func(context.Context) (Snapshot, error)
	mu       sync.RWMutex
	local    Snapshot
	all      Snapshot
	cache    map[string]Snapshot
}

func ValidateSource(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("source must be an HTTPS origin or loopback HTTP origin")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return errors.New("remote sources require HTTPS; use loopback SSH tunnels for private hosts")
	}
	return nil
}

func (c *Collector) Refresh(ctx context.Context) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	now := time.Now().UTC()
	local := Snapshot{Version: 1, GeneratedAt: now, Agents: []Row{}, Sources: []Source{{ID: "local", Status: "connected", LastSuccess: now}}}
	rows, err := c.Local(ctx)
	if err != nil {
		local.Sources[0].Status = "error"
		local.Sources[0].Error = "Local observations unavailable"
	}
	for _, r := range rows {
		local.Agents = append(local.Agents, Row{Record: r, Source: "local", SourceStatus: local.Sources[0].Status})
	}
	all := Snapshot{Version: 1, GeneratedAt: now, Agents: append([]Row{}, local.Agents...), Sources: append([]Source{}, local.Sources...)}
	type result struct {
		id   string
		snap Snapshot
		err  error
	}
	ch := make(chan result, len(c.Peers)+len(c.Fetchers))
	for id, fetch := range c.Fetchers {
		go func(id string, fetch func(context.Context) (Snapshot, error)) {
			attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			snap, err := fetch(attempt)
			ch <- result{id, snap, err}
		}(id, fetch)
	}
	for id, origin := range c.Peers {
		go func(id, origin string) {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(origin, "/")+"/api/v1/local", nil)
			var snap Snapshot
			if err == nil {
				client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
				resp, e := client.Do(req)
				err = e
				if err == nil {
					defer resp.Body.Close()
					b, e := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024+1))
					err = e
					if err == nil && (resp.StatusCode != 200 || len(b) > 4*1024*1024 || json.Unmarshal(b, &snap) != nil || snap.Version != 1 || snap.GeneratedAt.IsZero() || now.Sub(snap.GeneratedAt) > 15*time.Second || snap.GeneratedAt.Sub(now) > 30*time.Second) {
						err = errors.New("invalid or stale source snapshot")
					}
					for _, source := range snap.Sources {
						if source.Status != "connected" {
							err = errors.New("source collector unavailable")
						}
					}
				}
			}
			ch <- result{id, snap, err}
		}(id, origin)
	}
	if c.cache == nil {
		c.cache = map[string]Snapshot{}
	}
	for range len(c.Peers) + len(c.Fetchers) {
		r := <-ch
		source := Source{ID: r.id, Status: "connected", LastSuccess: now}
		if r.err != nil {
			source.Status = "unreachable"
			source.Error = "Source unavailable or snapshot stale"
			r.snap = c.cache[r.id]
			source.LastSuccess = r.snap.GeneratedAt
		} else {
			c.cache[r.id] = r.snap
		}
		all.Sources = append(all.Sources, source)
		for _, row := range r.snap.Agents {
			row.Source = r.id
			if source.Status != "connected" || row.SourceStatus == "" {
				row.SourceStatus = source.Status
			}
			all.Agents = append(all.Agents, row)
		}
	}
	sort.SliceStable(all.Agents, func(i, j int) bool {
		a, b := all.Agents[i], all.Agents[j]
		if a.LastActivity.Equal(b.LastActivity) {
			return a.Source+a.HostID+a.RunID < b.Source+b.HostID+b.RunID
		}
		return a.LastActivity.After(b.LastActivity)
	})
	sort.Slice(all.Sources, func(i, j int) bool { return all.Sources[i].ID < all.Sources[j].ID })
	c.mu.Lock()
	c.local, c.all = local, all
	c.mu.Unlock()
}

func (c *Collector) Run(ctx context.Context) {
	c.Refresh(ctx)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Refresh(ctx)
		}
	}
}

func (c *Collector) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, path := range []string{"/api/v1/agents", "/api/v1/local"} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				http.Error(w, "read-only endpoint", 405)
				return
			}
			c.mu.RLock()
			defer c.mu.RUnlock()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			if r.URL.Path == "/api/v1/local" {
				_ = json.NewEncoder(w).Encode(c.local)
			} else {
				_ = json.NewEncoder(w).Encode(c.all)
			}
		})
	}
	web, _ := fs.Sub(assets, "web")
	mux.Handle("/", http.FileServer(http.FS(web)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This local, unauthenticated UI is intentionally not a network hub.
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "loopback host required", 403)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
			http.Error(w, "cross-origin request rejected", 403)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
}
