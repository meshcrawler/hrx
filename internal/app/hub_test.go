package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/hub"
	"github.com/meshcrawler/hrx/internal/monitor"
)

// Cross-host fixture entrypoint: uses the production ownership/supervision path
// with a local inert harness, so the smoke test never makes model calls.
func TestHubProcessFixture(t *testing.T) {
	if os.Getenv("HRX_HUB_PROCESS_FIXTURE") != "1" {
		t.Skip("explicit cross-host fixture only")
	}
	trustHubFixtureCA(t)
	client, err := configuredHubClient(false)
	if err != nil {
		t.Fatal(err)
	}
	launch := Launch{Executable: "claude", Identity: "fixture-model", hubClient: client, Hub: true, agentIdentity: os.Getenv("HRX_HUB_FIXTURE_IDENTITY")}
	if err := execHarness(launch); err != nil {
		t.Fatal(err)
	}
}

func trustHubFixtureCA(t *testing.T) {
	t.Helper()
	if path := os.Getenv("HRX_INTEGRATION_CA"); path != "" {
		cert, err := os.ReadFile(path)
		pool := x509.NewCertPool()
		if err != nil || !pool.AppendCertsFromPEM(cert) {
			t.Fatal("fixture CA unavailable")
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		original := http.DefaultTransport
		t.Cleanup(func() { transport.CloseIdleConnections(); http.DefaultTransport = original })
		http.DefaultTransport = transport
	}
}

func TestHubOwnershipCleanupAndRevocation(t *testing.T) {
	for _, wrongHost := range []bool{true, false} {
		t.Run(map[bool]string{true: "wrong host", false: "revoked"}[wrongHost], func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			store, _ := monitor.Default()
			host, err := store.HostID()
			if err != nil {
				t.Fatal(err)
			}
			// Represent a running collector without spawning the test binary.
			dir := filepath.Join(filepath.Dir(store.Dir), "hub")
			if err = os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			lock, err := os.OpenFile(filepath.Join(dir, "collector.lock"), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			var released atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/leases/acquire":
					var request hub.LeaseRequest
					json.NewDecoder(r.Body).Decode(&request)
					owner := host
					if wrongHost {
						owner = strings.Repeat("b", 32)
					}
					now := time.Now()
					json.NewEncoder(w).Encode(hub.Lease{Identity: request.Identity, RunID: request.RunID, HostID: owner, Generation: 1, ServerTime: now, ExpiresAt: now.Add(45 * time.Second)})
				case "/api/v1/leases/renew":
					http.Error(w, "revoked", 403)
				case "/api/v1/leases/release":
					released.Add(1)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client, _ := hub.NewClient(server.URL, strings.Repeat("a", 32))
			launch := Launch{hubClient: client, monitorRun: strings.Repeat("c", 32), agentIdentity: "team/dev/agent"}
			stop, err := startHubOwnership(&launch, io.Discard)
			if wrongHost {
				if err == nil || released.Load() != 1 {
					t.Fatalf("failed acquire cleanup: %v releases=%d", err, released.Load())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			select {
			case err := <-launch.sidecarErrors:
				if !strings.Contains(err.Error(), "revoked") {
					t.Fatal(err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("revocation did not stop ownership")
			}
			stop()
			stop()
			if released.Load() != 1 {
				t.Fatalf("release count: %d", released.Load())
			}
		})
	}
}

func TestHubConfigurationPreservesStandaloneSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	t.Setenv("HRX_CONFIG", path)
	if err := os.WriteFile(path, []byte("[permissions]\nyolo=true\n[codex]\nprovider_name='example'\n[hub]\nviewer_token_file='viewer.token'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := saveHubConfig(dir, "https://hub.example", filepath.Join(dir, "host.token")); err != nil {
		t.Fatal(err)
	}
	c, _, err := loadUserConfig()
	if err != nil || !c.Hub.Enabled || !c.Permissions.Yolo || c.Codex.ProviderName != "example" || c.Hub.ViewerTokenFile != "viewer.token" {
		t.Fatalf("lost settings: %+v %v", c, err)
	}
	if err := prepareHubLaunch(LaunchOptions{AgentIdentity: "team/dev/agent", DryRun: true}, &Launch{}); err != nil {
		t.Fatal(err)
	}
	if err := prepareHubLaunch(LaunchOptions{DryRun: true}, &Launch{}); err == nil {
		t.Fatal("hub launch lacks identity")
	}
	if err := os.WriteFile(path, []byte("[hub]\nenabled=false\nurl='not-a-url'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	l := &Launch{}
	if err := prepareHubLaunch(LaunchOptions{}, l); err != nil || l.hubClient != nil {
		t.Fatal("standalone depends on hub", err)
	}
	if err := runHub(context.Background(), []string{"--help"}, io.Discard); err != nil {
		t.Fatal(err)
	}
}
