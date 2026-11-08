package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/meshcrawler/hrx/internal/catalog"
)

// Do not embed bytes.Buffer: its promoted ReadFrom lets io.Copy bypass Write.
type limitedBuffer struct{ buffer bytes.Buffer }

func (b *limitedBuffer) Len() int       { return b.buffer.Len() }
func (b *limitedBuffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *limitedBuffer) String() string { return b.buffer.String() }

type gatewayHTTPError struct{ Code int }

func (e *gatewayHTTPError) Error() string {
	return fmt.Sprintf("gateway returned HTTP %d (body suppressed)", e.Code)
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 65536 {
		return 0, errors.New("auth helper output too large")
	}
	return b.buffer.Write(p)
}

func credential(ctx context.Context, p Provider) (string, error) {
	var key string
	if p.ProfileCredential != "" {
		return readProfileCredential(p.ProfileCredential)
	} else if p.CredentialFile != "" {
		var err error
		key, err = openCodeCredential(p.CredentialFile)
		if err != nil {
			return "", err
		}
	} else if p.EnvKey != "" {
		key = os.Getenv(p.EnvKey)
	} else {
		timeout := time.Duration(p.Auth.TimeoutMS) * time.Millisecond
		if timeout <= 0 || timeout > 15*time.Second {
			timeout = 10 * time.Second
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, p.Auth.Command, p.Auth.Args...)
		var output limitedBuffer
		cmd.Stdout = &output
		cmd.Stderr = io.Discard
		cmd.WaitDelay = time.Second
		if err := cmd.Run(); err != nil {
			return "", errors.New("credential helper failed (output suppressed); check SSH or provider authentication")
		}
		key = output.String()
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.ContainsAny(key, "\r\n\t ") {
		return "", errors.New("provider returned an empty or invalid credential")
	}
	return key, nil
}

func getJSON(ctx context.Context, client *http.Client, url, key string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errors.New("invalid gateway endpoint")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("User-Agent", "hrx/0.1")
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("gateway request failed; check connectivity and TLS")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &gatewayHTTPError{Code: resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, catalog.MaxJSONBytes+1))
	if err != nil || len(data) > catalog.MaxJSONBytes {
		return errors.New("gateway response unreadable or too large")
	}
	if err = json.Unmarshal(data, target); err != nil {
		return errors.New("gateway returned invalid JSON")
	}
	return nil
}

func Refresh(ctx context.Context, s Settings) (catalog.Snapshot, error) {
	snapshot, _, err := refreshWithCredential(ctx, s)
	return snapshot, err
}

func refreshWithCredential(ctx context.Context, s Settings) (catalog.Snapshot, string, error) {
	return refreshWithCatalogCache(ctx, s, nil)
}

// A validate-all batch can share the public catalog for an endpoint, but must
// fetch authorization separately for every credential reference (including rotation).
func refreshWithCatalogCache(ctx context.Context, s Settings, public map[string]catalog.Dashboard) (catalog.Snapshot, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	client, err := gatewayHTTPClient(15 * time.Second)
	if err != nil {
		return catalog.Snapshot{}, "", err
	}
	defer client.CloseIdleConnections()
	key, err := credential(ctx, s.Provider)
	if err != nil {
		return catalog.Snapshot{}, "", err
	}
	d, cached := public[s.CatalogURL]
	if !cached {
		if err = getJSON(ctx, client, s.CatalogURL, "", &d); err != nil {
			return catalog.Snapshot{}, "", fmt.Errorf("model catalog: %w", err)
		}
		if public != nil {
			public[s.CatalogURL] = d
		}
	}
	var authorized catalog.Authorization
	if err = getJSON(ctx, client, s.Provider.BaseURL+"/models", key, &authorized); err != nil {
		return catalog.Snapshot{}, "", fmt.Errorf("model authorization: %w", err)
	}
	snapshot, err := catalog.Join(d, authorized, s.Binding, time.Now().UTC())
	if err != nil {
		return snapshot, "", err
	}
	if err = catalog.Write(s.CachePath, snapshot); err != nil {
		return snapshot, "", errors.New("cannot save model cache")
	}
	return snapshot, key, nil
}
