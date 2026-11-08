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

func TestYesodPrelaunchFailuresNeverInjectOrKeepLease(t *testing.T) {
	for _, harness := range []string{"codex", "claude", "opencode"} {
		for _, failure := range []string{"missing-harness", "registry-unavailable", "active-owner", "claim-unavailable"} {
			t.Run(harness+"/"+failure, func(t *testing.T) {
				dir := t.TempDir()
				t.Setenv("PATH", dir)
				t.Setenv("HRX_FAILURE_DIR", dir)
				t.Setenv("YESOD_MAIL_IDENTITY", "parent/agent/untouched")
				selector := "#!/bin/sh\nprintf 'select\\n' >> \"$HRX_FAILURE_DIR/events\"\nprintf '%s' '{\"agent\":\"fixture/hrx/failure\"}'\n"
				if failure == "registry-unavailable" {
					selector = "#!/bin/sh\nprintf 'select\\n' >> \"$HRX_FAILURE_DIR/events\"\necho private-sentinel >&2\nexit 1\n"
				}
				claimCode := "1"
				if failure == "active-owner" {
					claimCode = "3"
				}
				lease := "#!/bin/sh\nread -r token\n[ ${#token} = 64 ] || exit 9\nprintf '%s\\n' \"$2\" >> \"$HRX_FAILURE_DIR/events\"\nif [ \"$2\" = claim ]; then echo private-sentinel >&2; exit " + claimCode + "; fi\n"
				for name, script := range map[string]string{"python3": selector, "yesod": lease} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
						t.Fatal(err)
					}
				}
				if failure != "missing-harness" {
					if err := os.WriteFile(filepath.Join(dir, harness), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				launch := Launch{Executable: harness, Model: "fixture-route", Args: []string{"original-argument"}, Env: []string{"KEEP=original"}}
				var output bytes.Buffer
				err := prepareYesod(context.Background(), testSettings(t), LaunchOptions{YesodMail: true, YesodBBS: true, YesodAgent: "fixture/hrx/failure"}, &launch, &output)
				if err == nil || strings.Contains(err.Error(), "private-sentinel") || output.Len() != 0 || launch.lease != nil {
					t.Fatal("prelaunch failure leaked diagnostics or retained lease", err)
				}
				if errors.Is(err, errYesodLeaseLost) != (failure == "active-owner") {
					t.Fatal("ownership conflict misclassified", err)
				}
				if strings.Join(launch.Args, " ") != "original-argument" || strings.Join(launch.Env, " ") != "KEEP=original" || os.Getenv("YESOD_MAIL_IDENTITY") != "parent/agent/untouched" {
					t.Fatal("failed claim changed child or parent configuration")
				}
				events, _ := os.ReadFile(filepath.Join(dir, "events"))
				want := "select\nclaim\nrelease\n"
				if failure == "missing-harness" {
					want = ""
				}
				if failure == "registry-unavailable" {
					want = "select\n"
				}
				if string(events) != want {
					t.Fatalf("incorrect failure lifecycle: %q", events)
				}
			})
		}
	}
}

func TestRefreshFailureFixturesPreserveCache(t *testing.T) {
	for _, stage := range []string{"catalog", "authorization"} {
		for _, mode := range []string{"401", "403", "429", "500", "503", "malformed", "timeout", "stale"} {
			if stage == "authorization" && mode == "stale" {
				continue
			}
			t.Run(stage+"/"+mode, func(t *testing.T) {
				t.Setenv("HRX_TEST_KEY", "fixture-key")
				s := testSettings(t)
				if err := catalog.Write(s.CachePath, snapshot()); err != nil {
					t.Fatal(err)
				}
				before, _ := os.ReadFile(s.CachePath)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					isCatalog := r.URL.Path == "/catalog"
					if r.Method != "GET" {
						t.Error("unexpected mutation")
					}
					if isCatalog && r.Header.Get("Authorization") != "" {
						t.Error("credential leaked to catalog")
					}
					if !isCatalog && r.Header.Get("Authorization") != "Bearer fixture-key" {
						t.Error("authorization did not use fixture key")
					}
					if isCatalog == (stage == "catalog") {
						switch mode {
						case "timeout":
							<-r.Context().Done()
							return
						case "malformed":
							fmt.Fprint(w, "private-sentinel-invalid-json")
							return
						case "stale":
						default:
							var code int
							fmt.Sscan(mode, &code)
							http.Error(w, "private-sentinel", code)
							return
						}
					}
					if isCatalog {
						generated := time.Now()
						if mode == "stale" {
							generated = generated.Add(-time.Hour)
						}
						json.NewEncoder(w).Encode(map[string]any{"schema_version": 1, "generated_at": generated, "coverage": map[string]any{"providers": []any{map[string]any{"source": "gateway", "ok": true}}}, "models": []any{}})
					} else {
						fmt.Fprint(w, `{"data":[]}`)
					}
				}))
				defer server.Close()
				s.CatalogURL = server.URL + "/catalog"
				s.Provider.BaseURL = server.URL + "/v1"
				duration := time.Second
				if mode == "timeout" {
					duration = 50 * time.Millisecond
				}
				ctx, cancel := context.WithTimeout(context.Background(), duration)
				defer cancel()
				_, err := Refresh(ctx, s)
				if err == nil || strings.Contains(err.Error(), "private-sentinel") || strings.Contains(err.Error(), "fixture-key") {
					t.Fatal("unsafe or accepted refresh failure", err)
				}
				after, _ := os.ReadFile(s.CachePath)
				if !bytes.Equal(before, after) {
					t.Fatal("failure replaced last good cache")
				}
			})
		}
	}
}

func TestCredentialFailureFixtures(t *testing.T) {
	for _, value := range []string{"", "   ", "private-sentinel\nsecond-line", "private-sentinel with-spaces"} {
		t.Setenv("HRX_FAILURE_CREDENTIAL", value)
		got, err := credential(context.Background(), Provider{EnvKey: "HRX_FAILURE_CREDENTIAL"})
		if err == nil || got != "" || strings.Contains(err.Error(), "private-sentinel") {
			t.Fatal("invalid environment credential accepted or exposed")
		}
	}
	for _, command := range []string{"missing", "failure", "timeout", "oversized"} {
		t.Run(command, func(t *testing.T) {
			p := Provider{Auth: Auth{Command: "/bin/sh", Args: []string{"-c", "echo private-sentinel >&2; exit 1"}}}
			duration := 3 * time.Second
			switch command {
			case "missing":
				p.Auth.Command = filepath.Join(t.TempDir(), "missing")
			case "timeout":
				p.Auth.Args = []string{"-c", "exec /bin/sleep 30"}
				duration = 50 * time.Millisecond
			case "oversized":
				p.Auth.Args = []string{"-c", "i=0; while [ $i -lt 5000 ]; do printf private-sentinel-value; i=$((i+1)); done"}
			}
			ctx, cancel := context.WithTimeout(context.Background(), duration)
			defer cancel()
			got, err := credential(ctx, p)
			if err == nil || got != "" || strings.Contains(err.Error(), "private-sentinel") {
				t.Fatal("helper failure accepted or exposed")
			}
		})
	}
}
