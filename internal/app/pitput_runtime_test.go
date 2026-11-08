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
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/session"
)

func TestPitputRuntimeOptions(t *testing.T) {
	o, err := ParseLaunch([]string{"--pitput", "--agent-identity", "host/mayor/agent", "--pitput-board", "/b/dev", "--pitput-board=ops"})
	if err != nil || !slices.Equal(o.PitputBoards, []string{"dev", "ops"}) {
		t.Fatal(o, err)
	}
	for _, args := range [][]string{{"--pitput-board=dev"}, {"--pitput", "--pitput-board=../bad"}, {"--pitput", "--pitput-board"}} {
		if _, err := ParseLaunch(args); err == nil {
			t.Fatal("invalid options accepted", args)
		}
	}
	for _, extra := range [][]string{{"--no-daemon"}, {"exec"}} {
		if err := preparePitputRuntime(context.Background(), LaunchOptions{Pitput: true, AgentIdentity: "host/test/agent", Extra: extra}, &Launch{Executable: "codex"}); err == nil {
			t.Fatal("incompatible runtime accepted")
		}
	}
}

func TestPitputQueueAdapterPreservesMessage(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "codex fixture")
	output := filepath.Join(dir, "args")
	t.Setenv("HRX_ADAPTER_OUTPUT", output)
	os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$HRX_ADAPTER_OUTPUT\"\n"), 0700)
	adapter := filepath.Join(dir, "adapter")
	os.WriteFile(adapter, []byte(pitputQueueAdapter(binary, "unix:///tmp/private socket")), 0700)
	message := "literal $(false) `false` 'quoted'"
	if err := exec.Command(adapter, "queue", "--remote", "unix://", "--thread", "id", "--message", message).Run(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(output)
	if string(data) != "queue\n--remote\nunix:///tmp/private socket\n--thread\nid\n--message\n"+message+"\n" {
		t.Fatal("adapter changed arguments")
	}
	if exec.Command(adapter, "exec", "--remote", "unix://").Run() == nil {
		t.Fatal("non-queue invocation accepted")
	}
}

func TestNativePitputPrivateServerQueue(t *testing.T) {
	nativePitputPrivateServerQueue(t, false)
}

func TestNativePitputInterruptedTurnResume(t *testing.T) {
	nativePitputPrivateServerQueue(t, true)
}

func nativePitputPrivateServerQueue(t *testing.T, interruptPending bool) {
	if os.Getenv("HRX_NATIVE_SESSION_TESTS") != "1" {
		t.Skip("opt-in native Codex test; local fixture inference only")
	}
	realPitput := os.Getenv("HRX_PITPUT_INTEGRATION") == "1"
	if os.Getenv("HRX_HUB_NATIVE_TEST") == "1" {
		trustHubFixtureCA(t)
	}
	home := t.TempDir()
	if realPitput {
		home = os.Getenv("HOME")
	}
	home, _ = filepath.EvalSymlinks(home)
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("HRX_TEST_CREDENTIAL", "private-launch-sentinel")
	t.Setenv("HRX_FIXTURE_KEY", "fixture-only")
	os.MkdirAll(os.Getenv("CODEX_HOME"), 0700)
	const profileMarker = "HRX production selected profile proof"
	if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "launch-profile.config.toml"), []byte("developer_instructions = \""+profileMarker+"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	requests := make(chan string, 10)
	var firstRequest atomic.Bool
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(req.Body, 4*1024*1024))
		if strings.Contains(req.URL.Path, "responses") {
			var payload struct {
				Input []struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"input"`
			}
			if json.Unmarshal(body, &payload) != nil {
				t.Error("invalid fixture request")
			}
			occurrences := 0
			for _, item := range payload.Input {
				if item.Role == "developer" {
					occurrences += strings.Count(string(item.Content), profileMarker)
				}
			}
			if occurrences != 1 {
				t.Errorf("production profile developer instruction count=%d, want 1", occurrences)
			}
			if req.Header.Get("Authorization") != "Bearer private-launch-sentinel" {
				t.Error("native provider did not inherit launch credential")
			}
			select {
			case requests <- string(body):
			default:
			}
		}
		if strings.Contains(req.URL.Path, "responses") && interruptPending && !firstRequest.Swap(true) {
			// Leave the first turn pending until launcher shutdown cancels the
			// native request. No timing guess can turn this into an idle resume.
			<-req.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"fixture-response\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer gateway.Close()
	cli, err := exec.LookPath("pitput")
	if !realPitput {
		cli = filepath.Join(home, "pitput-fixture")
		err = os.WriteFile(cli, []byte("#!/bin/sh\ncase \"$1 $2\" in\n  \"bridge prepare\") exit 0 ;;\n  \"bridge run\") exec sleep 120 ;;\n  *) exit 64 ;;\nesac\n"), 0700)
	}
	if err != nil {
		t.Fatal(err)
	}
	r := &pitputRuntime{identity: "test/codex/agent", boards: []string{"exchange"}, cli: cli, record: session.Record{Harness: "codex", Identity: "fixture", Route: "fixture", Project: home, Pitput: true, AgentIdentity: "test/codex/agent", PitputBoards: []string{"exchange"}}}
	if identity := os.Getenv("HRX_PITPUT_TEST_IDENTITY"); realPitput && identity != "" {
		r.identity, r.record.AgentIdentity = identity, identity
	}
	if realPitput {
		l := Launch{Executable: "codex"}
		if err := preparePitputRuntime(context.Background(), LaunchOptions{Pitput: true, AgentIdentity: r.identity, PitputBoards: r.boards}, &l); err != nil {
			t.Fatal(err)
		}
	}
	l := Launch{Executable: "codex", pitputRuntime: r, Env: os.Environ(), Args: []string{"--no-daemon", "--profile", "launch-profile", "-c", `model_provider="fixture"`, "-c", fmt.Sprintf(`model_providers.fixture={name="fixture",base_url=%q,wire_api="responses",env_key="HRX_TEST_CREDENTIAL",request_max_retries=0,stream_max_retries=0}`, gateway.URL+"/v1")}}
	receipt := filepath.Join(home, "receipt")
	resumeLaunch := l
	resumeLaunch.Args = append([]string(nil), l.Args...)
	acquireHub := func(launch *Launch) func() {
		if os.Getenv("HRX_HUB_NATIVE_TEST") != "1" {
			return func() {}
		}
		client, err := configuredHubClient(false)
		if err != nil {
			t.Fatal(err)
		}
		launch.Identity, launch.agentIdentity, launch.hubClient = "fixture", r.identity, client
		if r.record.NativeID != "" {
			record := r.record
			launch.pendingSession = &record
		}
		trackLaunch(launch)
		cleanup, err := startHubOwnership(launch, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		return func() { finishLaunch(*launch); cleanup() }
	}
	stopHub := acquireHub(&l)
	defer stopHub()
	stop, err := startPitputRuntime(&l, receipt, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if slices.Contains(l.Args, "--no-daemon") || l.Args[0] != "--remote" {
		t.Fatal("TUI not bound to private server")
	}
	second := l
	if cleanup, err := startPitputRuntime(&second, "", io.Discard); err == nil {
		cleanup()
		t.Fatal("duplicate identity accepted")
	}
	socket := strings.TrimPrefix(l.Args[1], "unix://")
	var client *pitputRPC
	if os.Getenv("HRX_PITPUT_TUI") != "1" {
		client, err = connectPitputServer(context.Background(), socket)
		if err != nil {
			t.Fatal(err)
		}
		defer client.conn.Close()
	}
	var started struct{ Thread struct{ ID string } }
	if os.Getenv("HRX_PITPUT_TUI") == "1" {
		config := fmt.Sprintf("[projects.%q]\ntrust_level = \"trusted\"\n", home)
		if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		python, err := exec.LookPath("python3")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		script := `import os,pty,select,signal,sys,time
pid,master=pty.fork()
if pid==0:
 os.environ['TERM']='xterm-256color'
 os.execvp('codex',['codex']+sys.argv[1:])
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
		args := append([]string{"-c", script}, l.Args...)
		args = append(args, "--model", "fixture", "--yolo", "--no-alt-screen")
		p, err := startPitputProcess(ctx, python, args, os.Environ(), home)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			cancel()
			select {
			case <-p.done:
			case <-time.After(5 * time.Second):
				t.Error("TUI fixture failed to stop")
			}
		}()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(receipt)
			if len(data) > 0 {
				started.Thread.ID = string(data)
				break
			}
			select {
			case err := <-l.sidecarErrors:
				t.Fatal(err)
			default:
			}
			time.Sleep(100 * time.Millisecond)
		}
		if started.Thread.ID == "" {
			t.Fatal("TUI failed to load a discoverable thread")
		}
	} else {
		if err := client.call("thread/start", map[string]any{"model": "fixture", "modelProvider": "fixture", "cwd": home, "approvalPolicy": "never", "sandbox": "danger-full-access", "historyMode": "paginated"}, &started); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(15 * time.Second)
	var configs []string
	for time.Now().Before(deadline) {
		configs, _ = filepath.Glob(filepath.Join(home, "state/hrx/pitput/*", started.Thread.ID, "exchange/bridge.toml"))
		if len(configs) == 1 {
			break
		}
		select {
		case err := <-l.sidecarErrors:
			t.Fatal(err)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(configs) != 1 {
		t.Fatal("bridge mapping not created")
	}
	if !realPitput {
		adapter := filepath.Join(filepath.Dir(configs[0]), "codex-queue")
		cmd := exec.Command(adapter, "queue", "--remote", "unix://", "--thread", started.Thread.ID, "--message", "HRX fixture notification")
		cmd.Env = os.Environ()
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("native queue failed: %v %s", err, output)
		}
	}
	select {
	case body := <-requests:
		marker := "HRX fixture notification"
		if realPitput {
			marker = "Pitput attention notification"
		}
		if !strings.Contains(body, marker) {
			t.Fatal("queued notification did not reach the model")
		}
	case err := <-l.sidecarErrors:
		t.Fatal(err)
	case <-time.After(25 * time.Second):
		t.Fatal("notification was not delivered")
	}
	data, _ := os.ReadFile(receipt)
	if string(data) != started.Thread.ID {
		t.Fatal("resume receipt missing")
	}
	store, _ := session.Default()
	saved, err := store.Resolve("codex", home, started.Thread.ID)
	if err != nil || saved.AgentIdentity != r.identity || !slices.Equal(saved.PitputBoards, r.boards) {
		t.Fatal("resume metadata lost", err)
	}
	if realPitput {
		status := exec.Command(cli, "bridge", "status", "--config", configs[0])
		data, err := status.Output()
		var snapshot struct {
			Agents []struct {
				Pending *struct {
					EventKey string `json:"event_key"`
				}
			}
		}
		if err != nil || json.Unmarshal(data, &snapshot) != nil || len(snapshot.Agents) != 1 || snapshot.Agents[0].Pending == nil {
			t.Fatal("missing durable pending notification")
		}
		pendingKey := snapshot.Agents[0].Pending.EventKey
		stop()
		stopHub()
		data, err = exec.Command(cli, "bridge", "status", "--config", configs[0]).Output()
		if err != nil || json.Unmarshal(data, &snapshot) != nil || len(snapshot.Agents) != 1 || snapshot.Agents[0].Pending == nil || snapshot.Agents[0].Pending.EventKey != pendingKey {
			t.Fatal("shutdown acknowledged or discarded unhandled notification")
		}
		ack := exec.Command(cli, "bridge", "ack", "--config", configs[0], "--agent-id", r.identity, "--event-key", snapshot.Agents[0].Pending.EventKey)
		if err := ack.Run(); err != nil {
			t.Fatal("handling ACK failed", err)
		}
	}
	if client != nil {
		client.conn.Close()
	}
	stop()
	stopHub()
	if realPitput {
		data, err := exec.Command(cli, "bridge", "status", "--config", configs[0]).Output()
		var status struct{ Running bool }
		if err != nil || json.Unmarshal(data, &status) != nil || status.Running {
			t.Fatal("bridge remained registered as running after cleanup", err)
		}
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatal("private socket survived cleanup")
	}
	r.record.NativeID = started.Thread.ID
	stopResumedHub := acquireHub(&resumeLaunch)
	defer stopResumedHub()
	stopResumed, err := startPitputRuntime(&resumeLaunch, receipt, io.Discard)
	if err != nil {
		t.Fatal("resume server failed", err)
	}
	defer stopResumed()
	resumed, err := connectPitputServer(context.Background(), strings.TrimPrefix(resumeLaunch.Args[1], "unix://"))
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.conn.Close()
	if err := resumed.call("thread/resume", map[string]any{"threadId": started.Thread.ID, "model": "fixture", "modelProvider": "fixture", "cwd": home, "approvalPolicy": "never", "sandbox": "danger-full-access"}, nil); err != nil {
		t.Fatal("native resume failed", err)
	}
	if !realPitput {
		const resumedMarker = "HRX resumed profile notification proof"
		if err := resumed.call("thread/queue/add", map[string]any{
			"threadId":            started.Thread.ID,
			"input":               []map[string]any{{"type": "text", "text": resumedMarker}},
			"clientUserMessageId": "hrx-resumed-profile-proof",
		}, nil); err != nil {
			t.Fatal("resumed queue failed", err)
		}
		select {
		case body := <-requests:
			if !strings.Contains(body, resumedMarker) {
				t.Fatal("resumed notification missing from provider input")
			}
		case err := <-resumeLaunch.sidecarErrors:
			t.Fatal(err)
		case <-time.After(25 * time.Second):
			t.Fatal("resumed profile notification not delivered")
		}
	}
	if realPitput {
		select {
		case body := <-requests:
			if !strings.Contains(body, "Pitput attention notification") {
				t.Fatal("resume did not deliver the next board notification")
			}
		case err := <-resumeLaunch.sidecarErrors:
			t.Fatal(err)
		case <-time.After(25 * time.Second):
			t.Fatal("bridge did not rearm after ACK and resume")
		}
	}
}

func TestPitputServerArgumentsPreserveConfigAndFeatureSyntax(t *testing.T) {
	args := []string{"--config=service_tier=\"flex\"", "-c", "model_provider=\"fixture\"", "--enable=fast_mode", "--disable", "multi_agent", "--image", "--config=literal", "--", "--enable=literal"}
	got := pitputServerArgs(args)
	want := []string{"app-server", "-c", "service_tier=\"flex\"", "-c", "model_provider=\"fixture\"", "--enable", "fast_mode", "--disable", "multi_agent"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("server arguments %q, want %q", got, want)
	}
}
