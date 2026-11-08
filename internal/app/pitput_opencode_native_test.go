package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/meshcrawler/hrx/internal/session"
)

const nativeOpenCodePTYScript = `import os,pty,select,signal,sys
pid,master=pty.fork()
if pid==0:
 os.environ['TERM']='xterm-256color'
 os.execvp('opencode',['opencode']+sys.argv[1:])
def stop(*_):
 try: os.killpg(pid,signal.SIGKILL)
 except ProcessLookupError: pass
 try: os.waitpid(pid,0)
 except ChildProcessError: pass
 sys.exit(0)
signal.signal(signal.SIGTERM,stop)
try:
 while True:
  if select.select([master],[],[],.1)[0]:
   data=os.read(master,65536)
   if not data: break
   if b'\x1b[6n' in data: os.write(master,b'\x1b[1;1R')
finally: stop()
`

// This fixture proves real TUI authentication, delivery, persisted handling ACK,
// native resume and process cleanup. ACK is issued by the test, not a model tool.
func TestNativeOpenCodePitputBridge(t *testing.T) {
	if os.Getenv("HRX_NATIVE_SESSION_TESTS") != "1" || os.Getenv("HRX_PITPUT_INTEGRATION") != "1" {
		t.Skip("requires disposable native OpenCode and Pitput integration")
	}
	home, err := filepath.EvalSymlinks(os.Getenv("HOME"))
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(home, "project")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"DATA", "CACHE", "STATE"} {
		t.Setenv("XDG_"+kind+"_HOME", filepath.Join(home, strings.ToLower(kind)))
	}
	requests := make(chan string, 100)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		var request struct {
			Stream bool
			Tools  []json.RawMessage
		}
		_ = json.Unmarshal(body, &request)
		if len(request.Tools) > 0 {
			select {
			case requests <- string(body):
			default:
			}
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"fixture handled\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"fixture","object":"chat.completion","created":1,"model":"fixture","choices":[{"index":0,"message":{"role":"assistant","content":"fixture handled"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		}
	}))
	defer gateway.Close()
	config, _ := json.Marshal(map[string]any{"model": "hrx/fixture", "small_model": "hrx/fixture", "enabled_providers": []string{"hrx"}, "provider": map[string]any{"hrx": map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "fixture", "options": map[string]string{"baseURL": gateway.URL + "/v1", "apiKey": "fixture"}, "models": map[string]any{"fixture": map[string]string{"name": "fixture"}, "fixture-alt": map[string]string{"name": "fixture-alt"}}}}})
	t.Setenv("OPENCODE_CONFIG_CONTENT", string(config))
	// An existing shared-server password must not override the private runtime.
	t.Setenv("OPENCODE_PASSWORD", "unrelated-shared-server-password")
	var id, configPath string
	var saved session.Record
	for _, mode := range []string{"fresh", "resume", "resume-no-yolo"} {
		// Each mode gets its own budget. One context shared by all three let fresh and
		// resume (about 80 s on the native runner) exhaust it, so the third mode started
		// with an expired context and failed at the TUI start, not on its own behaviour.
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
		defer cancel()
		options := LaunchOptions{Pitput: true, AgentIdentity: "test/opencode/agent", PitputBoards: []string{"exchange"}, project: project, Resume: id}
		args := []string{"--prompt", "HRX TUI " + mode + " marker"}
		model := "fixture"
		if mode == "resume-no-yolo" {
			model = "fixture-alt"
		} else {
			args = append(args, "--auto")
		}
		if id != "" {
			args = append(args, "--session", id)
		}
		l := Launch{Executable: "opencode", Identity: "fixture", Model: model, Env: os.Environ(), Args: args}
		ownershipErrors := make(chan error, 4)
		l.sidecarErrors = ownershipErrors
		if err := preparePitputRuntime(ctx, options, &l); err != nil {
			t.Fatal(err)
		}
		if err := prepareSessionCapture(ctx, options, &l, io.Discard); err != nil {
			t.Fatal(err)
		}
		if id != "" && l.pendingSession.NativeID != id {
			t.Fatal("resume replaced native session")
		}
		id = l.pendingSession.NativeID
		receipt := filepath.Join(home, "receipt")
		stop, err := startPitputRuntime(&l, receipt, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
		if l.sidecarErrors != ownershipErrors {
			t.Fatal("OpenCode bridge replaced the hub ownership failure channel")
		}
		duplicate := l
		if cleanup, err := startPitputRuntime(&duplicate, "", io.Discard); err == nil {
			cleanup()
			t.Fatal("duplicate identity accepted")
		}
		found, _ := filepath.Glob(filepath.Join(home, "state/hrx/pitput/*", id, "exchange/bridge.toml"))
		if len(found) != 1 {
			t.Fatal("bridge config absent")
		}
		if configPath != "" && configPath != found[0] {
			t.Fatal("resume moved pending state")
		}
		configPath = found[0]
		encoded, _ := os.ReadFile(configPath)
		var bridge map[string]any
		if toml.Unmarshal(encoded, &bridge) != nil {
			t.Fatal("invalid bridge config")
		}
		passwordFile := bridge["opencode_password_file"].(string)
		password, _ := os.ReadFile(passwordFile)
		endpoint := bridge["opencode_url"].(string)
		if err := openCodeRequest(ctx, endpoint, "wrong", "GET", "/api/session/"+id, nil, nil); err == nil {
			t.Fatal("private API accepted wrong credential")
		}
		python, err := exec.LookPath("python3")
		if err != nil {
			t.Fatal(err)
		}
		tuiCtx, killTUI := context.WithCancel(ctx)

		var tui *pitputProcess
		err = l.startWithSession(func() error {
			var e error
			tui, e = startPitputProcess(tuiCtx, python, append([]string{"-c", nativeOpenCodePTYScript}, l.Args...), l.Env, project)
			return e
		})
		if err != nil {
			killTUI()
			t.Fatal(err)
		}
		defer killTUI()
		seenTUI, seenForum := false, false
		deadline := time.After(30 * time.Second)
		for !seenTUI || !seenForum {
			select {
			case body := <-requests:
				seenTUI = seenTUI || (strings.Contains(body, "HRX TUI "+mode+" marker") && strings.Contains(body, `"model":"`+model+`"`))
				seenForum = seenForum || strings.Contains(body, "Pitput attention notification")
			case err := <-l.sidecarErrors:
				status, statusErr := exec.Command(l.pitputRuntime.cli, "bridge", "status", "--config", configPath).CombinedOutput()
				t.Fatalf("%v; bridge status (%v): %s", err, statusErr, status)
			case <-deadline:
				t.Fatalf("%s did not receive both real TUI prompt and forum notification (TUI=%t forum=%t)", mode, seenTUI, seenForum)
			}
		}
		store, _ := session.Default()
		saved, err = store.Resolve("opencode", project, id)
		if err != nil || saved.AgentIdentity != options.AgentIdentity {
			t.Fatal("session capture lost identity", err)
		}
		if mode == "fresh" {
			data, err := exec.Command(l.pitputRuntime.cli, "bridge", "status", "--config", configPath).Output()
			var status struct {
				Agents []struct {
					Pending *struct {
						EventKey string `json:"event_key"`
					}
				}
			}
			if err != nil || json.Unmarshal(data, &status) != nil || len(status.Agents) != 1 || status.Agents[0].Pending == nil {
				t.Fatal("missing pending notification")
			}
			if err := exec.Command(l.pitputRuntime.cli, "bridge", "ack", "--config", configPath, "--agent-id", options.AgentIdentity, "--event-key", status.Agents[0].Pending.EventKey).Run(); err != nil {
				t.Fatal("explicit fixture handling ACK failed", err)
			}
		}
		killTUI()
		select {
		case <-tui.done:
		case <-time.After(5 * time.Second):
			t.Fatal("TUI did not stop")
		}
		stop()
		if _, err := os.Stat(passwordFile); !os.IsNotExist(err) {
			t.Fatal("private credential survived exit")
		}
		if err := openCodeRequest(ctx, endpoint, string(password), "GET", "/api/session/"+id, nil, nil); err == nil {
			t.Fatal("private server survived exit")
		}
		fmt.Fprintf(os.Stderr, "verified OpenCode %s: session=%s, real TUI, forum delivery, credential cleanup\n", mode, id)
	}
}
