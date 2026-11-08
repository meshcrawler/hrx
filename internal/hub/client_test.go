package hub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClientTransportAndCredentials(t *testing.T) {
	token := strings.Repeat("secret", 8)
	if _, err := NewClient("http://example.com", token); err == nil {
		t.Fatal("accepted plaintext remote")
	}
	for _, status := range []int{401, 403, 409, 500, 302} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+token {
					t.Error("missing authentication")
				}
				w.Header().Set("Location", "http://example.com")
				w.WriteHeader(status)
				w.Write([]byte(token))
			}))
			defer s.Close()
			c, _ := NewClient(s.URL, token)
			err := c.Call(context.Background(), "/", nil, nil)
			if err == nil || strings.Contains(err.Error(), token) {
				t.Fatalf("unsafe error: %v", err)
			}
			if (status == 401 || status == 403) && !errors.Is(err, ErrUnauthorized) {
				t.Fatal(err)
			}
		})
	}
}

func TestReadPrivateHubCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("credential\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadPrivate(path); err != nil || got != "credential" {
		t.Fatalf("%q %v", got, err)
	}
	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivate(link); err == nil {
		t.Fatal("followed symlink")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivate(path); err == nil {
		t.Fatal("accepted public credential")
	}
}

func TestClientRejectsUnsafeOwnershipDiagnostics(t *testing.T) {
	for _, body := range []string{
		`{"owner":{"host_id":"SECRET_CANARY","run_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","generation":1,"expires_at":"2099-01-01T00:00:00Z"}}`,
		`{"owner":{"host_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","run_id":"SECRET_CANARY","generation":1,"expires_at":"2099-01-01T00:00:00Z"}}`,
		strings.Repeat("x", 4097),
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(body))
		}))
		c, _ := NewClient(s.URL, strings.Repeat("a", 64))
		err := c.Call(context.Background(), "/api/v1/leases/acquire", nil, nil)
		s.Close()
		if err != ErrConflict {
			t.Fatalf("unvalidated diagnostic escaped: %v", err)
		}
	}
}

func TestClientLeaseValidation(t *testing.T) {
	now := time.Now()
	l := Lease{Identity: "team/dev/agent", HostID: strings.Repeat("a", 32), RunID: strings.Repeat("b", 32), Generation: 2, ServerTime: now, ExpiresAt: now.Add(45 * time.Second)}
	if err := validateLease(l, l.Identity, l.RunID, 2); err != nil {
		t.Fatal(err)
	}
	if err := validateLease(l, l.Identity, l.RunID, 1); err == nil {
		t.Fatal("accepted different generation")
	}
	l.ExpiresAt = now.Add(time.Second)
	if err := validateLease(l, l.Identity, l.RunID, 2); err == nil {
		t.Fatal("accepted lease too short to supervise")
	}
}
