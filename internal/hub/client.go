package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/meshcrawler/hrx/internal/monitor"
)

type Client struct {
	origin, token string
	http          *http.Client
}

func ReadPrivate(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", errors.New("cannot open hub credential file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("hub credential files must be private regular files (0600)")
	}
	data, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(data) > 16384 {
		return "", errors.New("invalid hub credential file")
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", errors.New("empty hub credential file")
	}
	return value, nil
}

func NewClient(origin, token string) (*Client, error) {
	if err := monitor.ValidateSource(origin); err != nil {
		return nil, err
	}
	if len(token) < 32 || len(token) > 512 || strings.ContainsAny(token, "\r\n\t ") {
		return nil, errors.New("invalid hub credential")
	}
	return &Client{origin: strings.TrimRight(origin, "/"), token: token, http: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) Call(ctx context.Context, path string, request, response any) error {
	method := http.MethodPost
	var input io.Reader
	if request == nil {
		method = http.MethodGet
	} else {
		data, err := json.Marshal(request)
		if err != nil {
			return ErrInvalid
		}
		if len(data) > MaxRequestBytes {
			return ErrInvalid
		}
		input = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.origin+path, input)
	if err != nil {
		return ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "hrx-hub/1")
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("hub connection unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		switch resp.StatusCode {
		case 400:
			return ErrInvalid
		case 401, 403:
			return ErrUnauthorized
		case 409:
			var detail struct {
				Owner *OwnershipConflict `json:"owner"`
			}
			data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
			if err == nil && len(data) <= 4096 && json.Unmarshal(data, &detail) == nil && detail.Owner != nil {
				owner := detail.Owner
				if identifier.MatchString(owner.HostID) && identifier.MatchString(owner.RunID) && owner.Generation > 0 && !owner.ExpiresAt.IsZero() {
					return owner
				}
			}
			return ErrConflict
		case 429:
			return errors.New("hub rate limit; retry later")
		default:
			return errors.New("hub request unavailable")
		}
	}
	if response == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024+1))
	if err != nil || len(data) > 4*1024*1024 || json.Unmarshal(data, response) != nil {
		return errors.New("invalid hub response")
	}
	return nil
}

func (c *Client) Snapshot(ctx context.Context) (monitor.Snapshot, error) {
	var result monitor.Snapshot
	err := c.Call(ctx, "/api/v1/agents", nil, &result)
	if err == nil && (result.Version != Version || result.GeneratedAt.IsZero()) {
		err = errors.New("unsupported hub snapshot")
	}
	return result, err
}

func (c *Client) Host(ctx context.Context) (Host, error) {
	var h Host
	err := c.Call(ctx, "/api/v1/host", nil, &h)
	if err == nil && (!identifier.MatchString(h.ID) || h.LastSequence < 0) {
		err = errors.New("invalid hub host response")
	}
	return h, err
}

func (c *Client) Acquire(ctx context.Context, identity, run string) (Lease, error) {
	var lease Lease
	err := c.Call(ctx, "/api/v1/leases/acquire", LeaseRequest{Version: Version, Identity: identity, RunID: run}, &lease)
	if err == nil {
		err = validateLease(lease, identity, run, 0)
	}
	return lease, err
}

func (c *Client) Renew(ctx context.Context, prior Lease) (Lease, error) {
	var lease Lease
	err := c.Call(ctx, "/api/v1/leases/renew", LeaseRequest{Version: Version, Identity: prior.Identity, RunID: prior.RunID, Generation: prior.Generation}, &lease)
	if err == nil {
		err = validateLease(lease, prior.Identity, prior.RunID, prior.Generation)
	}
	return lease, err
}

func validateLease(l Lease, identity, run string, generation int64) error {
	ttl := l.ExpiresAt.Sub(l.ServerTime)
	if l.Identity != identity || l.RunID != run || !identifier.MatchString(l.HostID) || l.Generation < 1 || generation > 0 && generation != l.Generation || ttl < 20*time.Second || ttl > 5*time.Minute {
		return errors.New("invalid hub lease response")
	}
	return nil
}

func (c *Client) Release(ctx context.Context, l Lease) error {
	return c.Call(ctx, "/api/v1/leases/release", LeaseRequest{Version: Version, Identity: l.Identity, RunID: l.RunID, Generation: l.Generation}, nil)
}
