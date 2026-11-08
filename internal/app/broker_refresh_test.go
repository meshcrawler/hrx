package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/monitor"
)

// rotatingBroker issues a new token on every request, as a broker does after the
// launch token has expired.
func rotatingBroker(t *testing.T) *httptest.Server {
	t.Helper()
	secret := strings.Repeat("r", 40)
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	var issued atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if r.Method != "POST" || r.URL.Path != "/v1/credentials" || r.Header.Get("Authorization") != "Bearer "+secret ||
			json.NewDecoder(r.Body).Decode(&body) != nil || body["identity"] != "host/hrx/agent" {
			w.WriteHeader(403)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"identity": body["identity"], "username": "hrx_fixture", "user_id": 42,
			"token": fmt.Sprintf("fresh-token-%d", issued.Add(1)), "host": "gitlab.example",
			"expires_at": time.Now().Add(72 * time.Hour).Format("2006-01-02")})
	}))
	t.Cleanup(s.Close)
	t.Setenv("HRX_GITLAB_BROKER_TOKEN_FILE", path)
	t.Setenv("HRX_GITLAB_BROKER_URL", s.URL)
	t.Setenv("HRX_AGENT_IDENTITY", "host/hrx/agent")
	t.Setenv("HRX_GITLAB_CREDENTIAL_HOST", "gitlab.example")
	t.Setenv("HRX_GITLAB_CREDENTIAL_USER", "hrx_fixture")
	t.Setenv("GITLAB_TOKEN", "expired-launch-token")
	t.Setenv("HRX_GITLAB_LAUNCH_TOKEN_SHA256", launchTokenDigest("expired-launch-token"))
	t.Setenv("HRX_GITLAB_LAUNCH_TOKEN_EXPIRES", time.Now().UTC().Format("2006-01-02")) // Expiring today.
	return s
}

func TestBrokerRefreshOnlyNearExpiry(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		expires string
		want    bool
	}{
		{"2026-11-04", false}, // 36 h left
		{"2026-11-03", false}, // 12 h left, outside the 6 h margin
		{"2026-11-02", true},  // already expired
		{"not-a-date", true},  // unknown expiry: allow refresh rather than fail
	}
	for _, tc := range cases {
		if got := launchTokenExpiring(tc.expires, now); got != tc.want {
			t.Fatalf("expires %s: got %v", tc.expires, got)
		}
	}
	if !launchTokenExpiring("2026-11-03", time.Date(2026, 11, 2, 18, 0, 0, 0, time.UTC)) {
		t.Fatal("6 h before expiry should refresh")
	}
	// A launch token that is not expiring never reaches the broker.
	rotatingBroker(t)
	t.Setenv("HRX_GITLAB_LAUNCH_TOKEN_EXPIRES", time.Now().UTC().Add(72*time.Hour).Format("2006-01-02"))
	if got := helperPassword(t); got != "expired-launch-token" {
		t.Fatalf("refreshed a token that was not expiring: %q", got)
	}
}

func helperPassword(t *testing.T) string {
	t.Helper()
	var out strings.Builder
	if err := gitCredential([]string{"get"}, strings.NewReader("protocol=https\nhost=gitlab.example\n\n"), &out); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if value, ok := strings.CutPrefix(line, "password="); ok {
			return value
		}
	}
	return ""
}

func TestBrokerGitCredentialRefresh(t *testing.T) {
	s := rotatingBroker(t)
	if got := helperPassword(t); got != "fresh-token-1" {
		t.Fatalf("git helper answered %q, want the refreshed token", got)
	}
	if got := helperPassword(t); got != "fresh-token-2" {
		t.Fatalf("git helper did not refresh per request: %q", got)
	}
	// A token the caller set explicitly is used as given, even when the broker is up.
	t.Setenv("GITLAB_TOKEN", "deliberately-invalid")
	if got := helperPassword(t); got != "deliberately-invalid" {
		t.Fatalf("replaced an explicitly set token: %q", got)
	}
	t.Setenv("GITLAB_TOKEN", "expired-launch-token")
	// A credential for another account must never replace the launch's own.
	t.Setenv("HRX_GITLAB_CREDENTIAL_USER", "hrx_other")
	if got := helperPassword(t); got != "expired-launch-token" {
		t.Fatalf("accepted a refreshed credential for another account: %q", got)
	}
	t.Setenv("HRX_GITLAB_CREDENTIAL_USER", "hrx_fixture")
	s.Close() // Broker unavailable: fall back to the launch token.
	if got := helperPassword(t); got != "expired-launch-token" {
		t.Fatalf("fallback answered %q", got)
	}
}

func TestBrokerGlabShimRefresh(t *testing.T) {
	s := rotatingBroker(t)
	dir := t.TempDir()
	executable := filepath.Join(dir, "hrx's helper")
	if output, err := exec.Command("go", "build", "-o", executable, "../../cmd/hrx").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	seen := filepath.Join(dir, "seen")
	fake := "#!/bin/sh\nprintf '%s %s' \"$GITLAB_TOKEN\" \"$*\" > '" + seen + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "glab"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	base := childEnvironment(os.Environ(), map[string]string{"PATH": bin + ":/usr/bin:/bin"}, nil)
	env, err := installGlabShim(base, executable)
	if err != nil {
		t.Fatal(err)
	}
	// exec.Command resolves names with the parent's PATH, so start the shim by path.
	shim, err := lookPathIn("glab", environmentValue(env, "PATH"))
	if err != nil || filepath.Dir(shim) == bin {
		t.Fatal("shim is not first on the child's PATH")
	}
	run := func(extra ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, shim, "api", "user")
		cmd.Env = append(append([]string{}, env...), extra...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("glab shim: %v: %s", err, output)
		}
		data, err := os.ReadFile(seen)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if got := run(); got != "fresh-token-1 api user" {
		t.Fatalf("real glab saw %q, want a refreshed token and the original arguments", got)
	}
	if got := run("GITLAB_TOKEN=deliberately-invalid"); got != "deliberately-invalid api user" {
		t.Fatalf("shim replaced an explicitly set token: %q", got)
	}
	s.Close()
	if got := run(); got != "expired-launch-token api user" {
		t.Fatalf("fallback saw %q", got)
	}
	if unchanged, err := installGlabShim(childEnvironment(os.Environ(), map[string]string{"PATH": t.TempDir()}, nil), executable); err != nil ||
		strings.Contains(strings.Join(unchanged, "\n"), "HRX_GLAB_REAL=") {
		t.Fatal("installed a shim without a real glab")
	}
}

// What a broker launch writes is exactly what the helper processes read back.
func TestGitlabLaunchContractRoundTrip(t *testing.T) {
	_, o := brokerFixture(t)
	l := Launch{}
	if _, err := prepareBroker(context.Background(), o, &l); err != nil {
		t.Fatal(err)
	}
	for _, entry := range l.Env {
		if name, value, ok := strings.Cut(entry, "="); ok {
			t.Setenv(name, value)
		}
	}
	got := currentGitlabLaunch()
	if got.Identity != "host/hrx/agent" || got.Host != "gitlab.example" || got.Username != "hrx_fixture" ||
		got.Token != "fixture-service-token" || got.LaunchTokenSHA256 != launchTokenDigest("fixture-service-token") ||
		got.LaunchTokenExpires != time.Now().Add(72*time.Hour).Format("2006-01-02") {
		t.Fatalf("launch contract did not round-trip: identity=%q host=%q user=%q expires=%q",
			got.Identity, got.Host, got.Username, got.LaunchTokenExpires)
	}
	if strings.Contains(strings.Join(l.Env, "\n"), envLaunchTokenSHA256+"=fixture-service-token") {
		t.Fatal("launch contract stored the token instead of its digest")
	}
}

func TestBrokerRefreshRecordsNewExpiryOnOwnRun(t *testing.T) {
	rotatingBroker(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HRX_MONITOR", "")
	store, err := monitor.Default()
	if err != nil {
		t.Fatal(err)
	}
	launchExpiry := time.Now().UTC().Format("2006-01-02")
	r, err := store.Register(monitor.Record{Agent: "host/hrx/agent", Harness: "claude", Credential: monitor.Credential{Provider: "gitlab", Host: "gitlab.example", Username: "hrx_fixture", ExpiresAt: launchExpiry}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(monitor.RunEnv, r.RunID)
	if refreshedBrokerToken(currentGitlabLaunch()) == "" {
		t.Fatal("expected a refreshed token")
	}
	rows, err := store.Snapshot(context.Background())
	want := time.Now().Add(72 * time.Hour).Format("2006-01-02")
	if err != nil || len(rows) != 1 || rows[0].Credential.ExpiresAt != want {
		t.Fatalf("dashboard expiry not refreshed: %v %+v", err, rows)
	}
}
