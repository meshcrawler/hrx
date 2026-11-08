package app

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the real command boundary and fake native executables. All gateway
// requests terminate in this fixture; the native scripts cannot call a model.
func TestProfilesCLIProcesses(t *testing.T) {
	buildEnv := os.Environ()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("profile fixture requires python3")
	}
	dir := profileConfig(t, "")
	binary := filepath.Join(dir, "hrx")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../tests/integration/hrxfixture")
	build.Env = buildEnv
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("unexpected write/inference")
			w.WriteHeader(500)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			if r.Header.Get("Authorization") != "Bearer profile-test-key" {
				w.WriteHeader(401)
				return
			}
			fmt.Fprint(w, `{"data":[{"id":"payer/example"}]}`)
		case "/dashboard/models.json":
			caps := map[string]any{}
			for h, wire := range map[string]string{"codex": "responses", "claude": "messages", "opencode": "chat_completions"} {
				caps[h] = map[string]any{"wire_api": wire, "reasoning_levels": []string{"low"}, "default_reasoning": "low"}
			}
			json.NewEncoder(w).Encode(map[string]any{"schema_version": 1, "generated_at": time.Now(), "coverage": map[string]any{"providers": []any{map[string]any{"source": "gateway", "ok": true}}}, "models": []any{map[string]any{"canonical_id": "example", "available": true, "sources": []any{map[string]any{"gateway_route": "payer/example", "available": true, "details": map[string]any{"harnesses": caps, "max_input_tokens": 100000, "max_output_tokens": 10000}}}}}})
		default:
			t.Error("unexpected endpoint", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	cert := filepath.Join(dir, "cert.pem")
	os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)
	var config strings.Builder
	for _, h := range []string{"codex", "claude", "opencode"} {
		fmt.Fprintf(&config, "[profiles.%s]\nharness=%q\nmodel=\"payer/example\"\ncredential_file=\"key.json\"\n[%s.provider]\nbase_url=%q\n", h, h, h, server.URL+"/v1")
	}
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config.String()), 0600)
	codex := filepath.Join(dir, "codex-home")
	os.Mkdir(codex, 0700)
	os.WriteFile(filepath.Join(codex, "config.toml"), []byte(fmt.Sprintf("model_provider=\"fixture\"\n[model_providers.fixture]\nwire_api=\"responses\"\nbase_url=%q\n", server.URL+"/v1")), 0600)
	os.WriteFile(filepath.Join(codex, "models_cache.json"), []byte(`{"models":[{"slug":"example"}]}`), 0600)
	fakes := filepath.Join(dir, "bin")
	os.Mkdir(fakes, 0700)
	script := `import json, os, sys, subprocess
if len(sys.argv)>1 and sys.argv[1]=='api':
    print(json.dumps({'data':{'id':'native-profile-fixture','location':{'directory':os.getcwd()}}}))
else:
    metadata=os.environ.get('HRX_SESSION_METADATA')
    if metadata:
        record=json.loads(metadata)
        event={'hook_event_name':'SessionStart','source':'resume' if record.get('native_id') else 'startup',
               'session_id':'native-profile-fixture','cwd':os.getcwd(),'model':record['model_route']}
        subprocess.run([os.environ['PROFILE_HRX'],'_session-start'],input=json.dumps(event),text=True,check=True)
    keys=['HRX_PROFILE_GATEWAY_KEY','HRX_GATEWAY_KEY','ANTHROPIC_CUSTOM_HEADERS','HRX_SESSION_METADATA']
    with open(os.environ['PROFILE_CAPTURE'],'w') as out:
        json.dump({'args':sys.argv[1:],'env':{k:os.environ.get(k,'') for k in keys}},out)
`
	for _, h := range []string{"codex", "claude", "opencode"} {
		os.WriteFile(filepath.Join(fakes, h), []byte("#!"+python+"\n"+script), 0700)
	}
	base := childEnvironment(os.Environ(), map[string]string{"HOME": dir, "HRX_CONFIG": filepath.Join(dir, "config.toml"), "CODEX_HOME": codex, "HRX_INTEGRATION_CA": cert, "PATH": fakes + string(os.PathListSeparator) + os.Getenv("PATH"), "HRX_MONITOR": "off", "HRX_CODEX_PROVIDER": "", "WAVETERM_BLOCKID": "", "HRX_WAVE": "off"}, nil)
	base = childEnvironment(base, map[string]string{"PROFILE_HRX": binary}, nil)
	call := func(args ...string) ([]byte, error) {
		c := exec.CommandContext(ctx, binary, args...)
		c.Dir = dir
		c.Env = base
		return c.CombinedOutput()
	}
	for _, args := range [][]string{{"profiles", "list", "--json"}, {"profiles", "show", "codex", "--json"}, {"profiles", "validate", "--all", "--json"}, {"profiles", "validate", "--all", "--offline", "--json"}, {"--profile", "claude", "--dry-run"}} {
		output, err := call(args...)
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, output)
		}
		if strings.Contains(string(output), "profile-test-key") {
			t.Fatal("CLI exposed key")
		}
	}
	if _, err := call("profiles", "validate", "missing", "--json"); err == nil {
		t.Fatal("CLI accepted missing profile")
	}
	if _, err := call("opencode", "--profile", "codex", "--dry-run"); err == nil {
		t.Fatal("CLI accepted conflicting harness")
	}
	for _, h := range []string{"codex", "claude", "opencode"} {
		capture := filepath.Join(dir, h+"-capture.json")
		base = childEnvironment(base, map[string]string{"PROFILE_CAPTURE": capture}, nil)
		args := []string{"--profile", h, "--", "fixture prompt"}
		if h == "opencode" {
			args = []string{"--profile", h, "--", "run", "fixture prompt"}
		}
		output, err := call(args...)
		if err != nil {
			t.Fatalf("%s launch: %v %s", h, err, output)
		}
		if strings.Contains(string(output), "profile-test-key") {
			t.Fatal("launch exposed key")
		}
		var report struct {
			Args []string          `json:"args"`
			Env  map[string]string `json:"env"`
		}
		data, err := os.ReadFile(capture)
		if err != nil || json.Unmarshal(data, &report) != nil {
			t.Fatal("missing fake harness receipt", err)
		}
		if strings.Contains(strings.Join(report.Args, " "), "profile-test-key") {
			t.Fatal("key in argv")
		}
		if h == "codex" && report.Env["HRX_PROFILE_GATEWAY_KEY"] != "profile-test-key" || h == "opencode" && report.Env["HRX_GATEWAY_KEY"] != "profile-test-key" || h == "claude" && !strings.HasSuffix(report.Env["ANTHROPIC_CUSTOM_HEADERS"], "profile-test-key") {
			t.Fatal("wrong child credential")
		}
		if strings.Contains(report.Env["HRX_SESSION_METADATA"], "profile-test-key") {
			t.Fatal("key in session metadata")
		}
	}
	// Resume all three without their named definitions. This must use the saved
	// credential reference, not the provider's absent credential or edited defaults.
	var providers strings.Builder
	for _, h := range []string{"codex", "claude", "opencode"} {
		fmt.Fprintf(&providers, "[%s.provider]\nbase_url=%q\n", h, server.URL+"/v1")
	}
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte(providers.String()), 0600)
	for _, h := range []string{"codex", "claude", "opencode"} {
		args := []string{h, "--resume", "native-profile-fixture", "--", "fixture resumed"}
		if h == "opencode" {
			args = []string{h, "--resume", "native-profile-fixture", "--", "run", "fixture resumed"}
		}
		output, err := call(args...)
		if err != nil {
			t.Fatalf("%s profile resume: %v %s", h, err, output)
		}
		if strings.Contains(string(output), "profile-test-key") {
			t.Fatal("resume leaked key")
		}
	}
}
