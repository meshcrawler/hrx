package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/catalog"
)

func doctorFixture(t *testing.T, code int) (Settings, doctorDeps, *int) {
	t.Helper()
	s := testSettings(t)
	s.TemplatePath = filepath.Join(t.TempDir(), "templates.json")
	if err := os.WriteFile(s.TemplatePath, []byte(`{"models":[{"slug":"example"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" {
			t.Error("doctor mutated gateway")
		}
		switch r.URL.Path {
		case "/dashboard/models.json":
			if r.Header.Get("Authorization") != "" {
				t.Error("credential sent to discovery")
			}
			cap := snapshot().Models[0].Harnesses
			json.NewEncoder(w).Encode(map[string]any{"schema_version": 1, "generated_at": time.Now(), "coverage": map[string]any{"providers": []any{map[string]any{"source": "gateway", "ok": true}}}, "models": []any{map[string]any{"canonical_id": "example", "available": true, "sources": []any{map[string]any{"gateway_route": "provider/example", "available": true, "details": map[string]any{"harnesses": cap}}}}}})
		case "/v1/models":
			if r.Header.Get("Authorization") != "Bearer secret-value" {
				t.Error("missing auth")
			}
			if code != 200 {
				http.Error(w, "secret-value from gateway", code)
				return
			}
			ioData := `{"data":[{"id":"provider/example"}]}`
			w.Write([]byte(ioData))
		default:
			t.Error("unexpected endpoint", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	s.Provider.BaseURL = server.URL + "/v1"
	s.CatalogURL = server.URL + "/dashboard/models.json"
	deps := doctorDeps{
		load:   func(string) (Settings, error) { return s, nil },
		lookup: func(name string) (string, error) { return name, nil },
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if name == "yesod" {
				switch strings.Join(args, " ") {
				case "mail claim --help", "mail renew --help", "mail release --help":
					return []byte("--as --token-stdin"), nil
				}
				if strings.Join(args, " ") != "mail roster --json" {
					t.Error("mutating yesod args", args)
				}
				return []byte(`[]`), nil
			}
			if strings.Join(args, " ") != "--version" {
				t.Error("unexpected harness launch", args)
			}
			return []byte("codex-cli 0.162.1\nsecret-value"), nil
		},
		credential: func(context.Context, Provider) (string, error) { return "secret-value", nil },
		client:     server.Client(),
	}
	return s, deps, &calls
}

func TestDoctorRequiresEveryLeaseCommand(t *testing.T) {
	for _, operation := range []string{"claim", "renew", "release"} {
		for _, malformed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/malformed=%t", operation, malformed), func(t *testing.T) {
				_, d, _ := doctorFixture(t, 200)
				original := d.run
				d.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
					if name == "yesod" && strings.Join(args, " ") == "mail "+operation+" --help" {
						if malformed {
							return []byte("secret-value generic help"), nil
						}
						return []byte("secret-value"), errors.New("secret-value")
					}
					return original(ctx, name, args...)
				}
				r := diagnose(context.Background(), doctorOptions{harness: "codex", yesod: true, timeout: time.Second}, d)
				if r.OK || !hasDoctorState(r, "yesod-leases", "unsupported") || !hasDoctorState(r, "yesod", "reachable") {
					t.Fatalf("%+v", r)
				}
				data, _ := json.Marshal(r)
				if strings.Contains(string(data), "secret-value") {
					t.Fatal("lease help diagnostics leaked")
				}
			})
		}
	}
}

func hasDoctorState(r DoctorReport, name, state string) bool {
	for _, c := range r.Checks {
		if c.Name == name && c.State == state {
			return true
		}
	}
	return false
}

func TestDoctorReadOnlyAndSecretSafe(t *testing.T) {
	s, d, calls := doctorFixture(t, 200)
	if err := catalog.Write(s.CachePath, snapshot()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.CachePath)
	r := diagnose(context.Background(), doctorOptions{harness: "codex", yesod: true, timeout: time.Second}, d)
	data, _ := json.Marshal(r)
	if !r.OK || *calls != 2 || !hasDoctorState(r, "model", "declared-compatible") || !hasDoctorState(r, "verification", "not-tested") || !hasDoctorState(r, "yesod", "reachable") {
		t.Fatalf("%s calls=%d", data, *calls)
	}
	if strings.Contains(string(data), "secret-value") || strings.Contains(string(data), s.Provider.BaseURL) {
		t.Fatal("sensitive diagnostics", string(data))
	}
	after, _ := os.ReadFile(s.CachePath)
	if !bytes.Equal(before, after) {
		t.Fatal("doctor rewrote cache")
	}
	entries, _ := os.ReadDir(filepath.Dir(s.CachePath))
	if len(entries) != 1 {
		t.Fatal("doctor created artifacts")
	}
}

func TestDoctorFailureCategories(t *testing.T) {
	for _, tc := range []struct {
		code  int
		state string
	}{{401, "not-authorized"}, {403, "not-authorized"}, {429, "rate-limited"}, {500, "unavailable"}} {
		t.Run(tc.state+string(rune(tc.code)), func(t *testing.T) {
			_, d, _ := doctorFixture(t, tc.code)
			r := diagnose(context.Background(), doctorOptions{harness: "codex", timeout: time.Second}, d)
			if r.OK || !hasDoctorState(r, "authorization", tc.state) {
				t.Fatalf("%+v", r)
			}
			data, _ := json.Marshal(r)
			if strings.Contains(string(data), "secret-value") {
				t.Fatal("secret leaked")
			}
		})
	}
}

func TestDoctorConfigurationCredentialOptionalAndUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name                string
		setup               func(*doctorDeps)
		opts                doctorOptions
		wantName, wantState string
		ok                  bool
	}{
		{"bad config", func(d *doctorDeps) {
			d.load = func(string) (Settings, error) { return Settings{}, errors.New("secret-value") }
		}, doctorOptions{harness: "codex"}, "configuration", "invalid", false},
		{"credential", func(d *doctorDeps) {
			d.credential = func(context.Context, Provider) (string, error) { return "", errors.New("secret-value") }
		}, doctorOptions{harness: "codex"}, "credential", "unavailable", false},
		{"missing templates", func(d *doctorDeps) {
			load := d.load
			d.load = func(h string) (Settings, error) { s, err := load(h); s.TemplatePath = ""; return s, err }
		}, doctorOptions{harness: "codex"}, "templates", "invalid", false},
		{"optional harnesses", func(d *doctorDeps) {
			d.lookup = func(name string) (string, error) {
				if name != "codex" {
					return "", errors.New("secret-value")
				}
				return name, nil
			}
		}, doctorOptions{harness: "all"}, "executable", "unavailable", true},
		{"required harness", func(d *doctorDeps) { d.lookup = func(string) (string, error) { return "", errors.New("secret-value") } }, doctorOptions{harness: "codex"}, "executable", "unavailable", false},
		{"unsupported effort", func(d *doctorDeps) {}, doctorOptions{harness: "codex", reasoning: "ultra"}, "reasoning", "unsupported", false},
		{"unknown model", func(d *doctorDeps) {}, doctorOptions{harness: "codex", model: "secret-value"}, "model", "unknown", false},
		{"bad version", func(d *doctorDeps) {
			d.run = func(context.Context, string, ...string) ([]byte, error) {
				return []byte("secret-value"), errors.New("secret-value")
			}
		}, doctorOptions{harness: "codex"}, "version", "unknown", true},
		{"yesod failure", func(d *doctorDeps) {
			old := d.run
			d.run = func(ctx context.Context, n string, a ...string) ([]byte, error) {
				if n == "yesod" {
					return []byte("secret-value"), errors.New("secret-value")
				}
				return old(ctx, n, a...)
			}
		}, doctorOptions{harness: "codex", yesod: true}, "yesod", "unavailable", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, d, _ := doctorFixture(t, 200)
			tc.setup(&d)
			tc.opts.timeout = time.Second
			r := diagnose(context.Background(), tc.opts, d)
			if r.OK != tc.ok || !hasDoctorState(r, tc.wantName, tc.wantState) {
				t.Fatalf("%+v", r)
			}
			data, _ := json.Marshal(r)
			if strings.Contains(string(data), "secret-value") {
				t.Fatal("leaked secret")
			}
		})
	}
}

func TestDoctorCacheAndDeadline(t *testing.T) {
	s, d, _ := doctorFixture(t, 200)
	snap := snapshot()
	snap.RefreshedAt = time.Now().Add(-time.Hour)
	if err := catalog.Write(s.CachePath, snap); err != nil {
		t.Fatal(err)
	}
	r := diagnose(context.Background(), doctorOptions{harness: "codex", timeout: time.Second}, d)
	if !r.OK || !hasDoctorState(r, "cache", "stale") {
		t.Fatalf("%+v", r)
	}
	d.run = func(ctx context.Context, _ string, _ ...string) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() }
	start := time.Now()
	r = diagnose(context.Background(), doctorOptions{harness: "codex", timeout: 10 * time.Millisecond}, d)
	if r.OK || !hasDoctorState(r, "deadline", "timeout") || time.Since(start) > time.Second {
		t.Fatalf("deadline failed: %+v", r)
	}
}

func TestDoctorCLIUsageAndCodes(t *testing.T) {
	for _, args := range [][]string{{"--wat"}, {"--model", "x"}, {"--timeout", "0"}, {"--timeout", "61"}, {"--harness", "wrong"}, {"--harness"}, {"--reasoning", "high"}} {
		_, err := parseDoctor(args)
		if ExitCode(err) != 2 {
			t.Fatal("wrong usage status", args, err)
		}
	}
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"doctor", "--help"}, &out, &out); err != nil || !strings.Contains(out.String(), "no inference") {
		t.Fatal(err, out.String())
	}
	if ExitCode(nil) != 0 || ExitCode(errors.New("x")) != 1 || ExitCode(&exitError{1, "failed"}) != 1 {
		t.Fatal("bad codes")
	}
}

func TestDoctorInstalledVersionFormats(t *testing.T) {
	for _, version := range []string{"codex-cli 0.162.1", "2.1.296 (Claude Code)", "opencode v2.0.26", "v1.2.3"} {
		t.Run(version, func(t *testing.T) {
			_, d, _ := doctorFixture(t, 200)
			d.run = func(context.Context, string, ...string) ([]byte, error) {
				return []byte(version + "\nsecret-value"), nil
			}
			r := diagnose(context.Background(), doctorOptions{harness: "codex", timeout: time.Second}, d)
			if !hasDoctorState(r, "version", "known") {
				t.Fatal(r)
			}
			data, _ := json.Marshal(r)
			if strings.Contains(string(data), "secret-value") {
				t.Fatal("version output leak")
			}
		})
	}
}
