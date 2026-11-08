package hub

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Server struct {
	Store                     *Store
	AdminDigest, ViewerDigest string
	active                    chan struct{}
	mu                        sync.Mutex
	rates                     map[string]rate
}

type rate struct {
	at        time.Time
	remaining int
}

func NewServer(store *Store, adminDigest, viewerDigest string) (*Server, error) {
	if store == nil || !digestPattern.MatchString(adminDigest) || !digestPattern.MatchString(viewerDigest) || adminDigest == viewerDigest {
		return nil, errors.New("hub requires separate administrator and viewer credentials")
	}
	return &Server{Store: store, AdminDigest: adminDigest, ViewerDigest: viewerDigest, active: make(chan struct{}, 64), rates: map[string]rate{}}, nil
}

func (s *Server) allow(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	b := s.rates[key]
	if now.Sub(b.at) >= time.Second {
		b = rate{at: now, remaining: 30}
	}
	if b.remaining <= 0 {
		return false
	}
	b.remaining--
	if len(s.rates) >= 10000 {
		for k, v := range s.rates {
			if now.Sub(v.at) > time.Minute {
				delete(s.rates, k)
			}
		}
		if len(s.rates) >= 10000 {
			return false
		}
	}
	s.rates[key] = b
	return true
}

func decode(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return ErrInvalid
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return ErrInvalid
	}
	return nil
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, err error) {
	var owner *OwnershipConflict
	if errors.As(err, &owner) {
		reply(w, http.StatusConflict, map[string]any{"error": ErrConflict.Error(), "owner": owner})
		return
	}
	status, message := http.StatusServiceUnavailable, "hub storage unavailable"
	switch {
	case errors.Is(err, ErrInvalid):
		status, message = 400, ErrInvalid.Error()
	case errors.Is(err, ErrUnauthorized):
		status, message = 403, ErrUnauthorized.Error()
	case errors.Is(err, ErrConflict):
		status, message = 409, ErrConflict.Error()
	case errors.Is(err, ErrCapacity):
		status, message = 503, ErrCapacity.Error()
	}
	reply(w, status, map[string]string{"error": message})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/healthz" && r.Method == "GET" {
		reply(w, 200, map[string]any{"version": Version, "status": "ok"})
		return
	}
	if r.Header.Get("Origin") != "" {
		fail(w, ErrUnauthorized)
		return
	}
	select {
	case s.active <- struct{}{}:
		defer func() { <-s.active }()
	default:
		reply(w, 503, map[string]string{"error": "hub busy"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if r.Method != "POST" && !(r.Method == "GET" && (r.URL.Path == "/api/v1/agents" || r.URL.Path == "/api/v1/host")) {
		reply(w, 405, map[string]string{"error": "unsupported method"})
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || len(token) < 32 || len(token) > 512 {
		reply(w, 401, map[string]string{"error": "hub credential required"})
		return
	}
	digest := Digest(token)
	admin := subtle.ConstantTimeCompare([]byte(digest), []byte(s.AdminDigest)) == 1
	viewer := subtle.ConstantTimeCompare([]byte(digest), []byte(s.ViewerDigest)) == 1
	if r.URL.Path == "/api/v1/agents" {
		if r.Method != "GET" {
			reply(w, 405, map[string]string{"error": "read-only endpoint"})
			return
		}
		if !admin && !viewer {
			fail(w, ErrUnauthorized)
			return
		}
		if !s.allow(digest) {
			reply(w, 429, map[string]string{"error": "hub rate limit"})
			return
		}
		snapshot, err := s.Store.Snapshot(ctx)
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, snapshot)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/hosts/") {
		if !admin {
			fail(w, ErrUnauthorized)
			return
		}
		if !s.allow(digest) {
			reply(w, 429, map[string]string{"error": "hub rate limit"})
			return
		}
		var err error
		switch r.URL.Path {
		case "/api/v1/hosts/enroll":
			var e Enrollment
			err = decode(w, r, &e)
			if err == nil {
				err = s.Store.Enroll(ctx, e)
			}
		case "/api/v1/hosts/revoke", "/api/v1/hosts/rotate":
			var e struct {
				Version     int    `json:"version"`
				HostID      string `json:"host_id"`
				TokenDigest string `json:"token_digest,omitempty"`
			}
			err = decode(w, r, &e)
			if err == nil && e.Version != Version {
				err = ErrInvalid
			}
			if err == nil {
				if r.URL.Path == "/api/v1/hosts/revoke" {
					err = s.Store.Revoke(ctx, e.HostID)
				} else {
					err = s.Store.Rotate(ctx, e.HostID, e.TokenDigest)
				}
			}
		default:
			reply(w, 404, map[string]string{"error": "unknown endpoint"})
			return
		}
		if err != nil {
			fail(w, err)
			return
		}
		reply(w, 200, map[string]bool{"ok": true})
		return
	}
	host, err := s.Store.Authenticate(ctx, token)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			reply(w, 401, map[string]string{"error": "invalid host credential"})
		} else {
			fail(w, err)
		}
		return
	}
	if !s.allow(host.ID) {
		reply(w, 429, map[string]string{"error": "hub rate limit"})
		return
	}
	if r.URL.Path == "/api/v1/host" && r.Method == "GET" {
		reply(w, 200, host)
		return
	}
	switch r.URL.Path {
	case "/api/v1/heartbeat":
		var e struct {
			Version  int      `json:"version"`
			LiveRuns []string `json:"live_runs"`
		}
		err = decode(w, r, &e)
		if err == nil && e.Version != Version {
			err = ErrInvalid
		}
		if err == nil {
			err = s.Store.Heartbeat(ctx, host, e.LiveRuns)
		}
	case "/api/v1/observations":
		var b Batch
		err = decode(w, r, &b)
		if err == nil {
			err = s.Store.Ingest(ctx, host, b)
		}
	case "/api/v1/leases/acquire", "/api/v1/leases/renew", "/api/v1/leases/release":
		var e LeaseRequest
		err = decode(w, r, &e)
		if err == nil {
			if r.URL.Path == "/api/v1/leases/release" {
				err = s.Store.Release(ctx, host, e)
			} else {
				var lease Lease
				if r.URL.Path == "/api/v1/leases/acquire" {
					lease, err = s.Store.Acquire(ctx, host, e)
				} else {
					lease, err = s.Store.Renew(ctx, host, e)
				}
				if err == nil {
					reply(w, 200, lease)
					return
				}
			}
		}
	default:
		reply(w, 404, map[string]string{"error": "unknown endpoint"})
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, 200, map[string]bool{"ok": true})
}
