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

	"github.com/meshcrawler/hrx/internal/session"
)

// Driven by tests/integration/pitput_dm_bridge.py against a disposable Pitput server:
// a real Codex app server, real Pitput bridges and a local model fixture (no paid calls).
func TestNativePitputDirectMessageQueue(t *testing.T) {
	if os.Getenv("HRX_NATIVE_SESSION_TESTS") != "1" || os.Getenv("HRX_PITPUT_DM_INTEGRATION") != "1" {
		t.Skip("opt-in native direct-message test; run tests/integration/pitput_dm_bridge.py")
	}
	home, _ := filepath.EvalSymlinks(os.Getenv("HOME"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("HRX_FIXTURE_KEY", "fixture-only")
	os.MkdirAll(os.Getenv("CODEX_HOME"), 0700)
	sentinel := os.Getenv("HRX_PITPUT_DM_SENTINEL")
	requests := make(chan string, 16)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(req.Body, 4*1024*1024))
		if strings.Contains(req.URL.Path, "responses") {
			select {
			case requests <- string(body):
			default:
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"fixture-response\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer gateway.Close()
	identity := "test/codex/agent"
	l := Launch{Executable: "codex"}
	if err := preparePitputRuntime(context.Background(), LaunchOptions{Pitput: true, AgentIdentity: identity, PitputBoards: []string{"exchange"}}, &l); err != nil {
		t.Fatal(err)
	}
	r := l.pitputRuntime
	if !r.dm {
		t.Fatal("direct messages were not enabled by default against a dm.v1 server: " + r.dmNotice)
	}
	r.record = session.Record{Harness: "codex", Identity: "fixture", Route: "fixture", Project: home, Pitput: true, AgentIdentity: identity, PitputBoards: r.boards}
	l.Env = os.Environ()
	l.Args = []string{"--no-daemon", "-c", `model_provider="fixture"`, "-c", fmt.Sprintf(`model_providers.fixture={name="fixture",base_url=%q,wire_api="responses",env_key="HRX_FIXTURE_KEY",request_max_retries=0,stream_max_retries=0}`, gateway.URL+"/v1")}
	stop, err := startPitputRuntime(&l, filepath.Join(home, "receipt"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	client, err := connectPitputServer(context.Background(), strings.TrimPrefix(l.Args[1], "unix://"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.conn.Close()
	var started struct{ Thread struct{ ID string } }
	if err := client.call("thread/start", map[string]any{"model": "fixture", "modelProvider": "fixture", "cwd": home, "approvalPolicy": "never", "sandbox": "danger-full-access", "historyMode": "paginated"}, &started); err != nil {
		t.Fatal(err)
	}
	var config string
	for deadline := time.Now().Add(20 * time.Second); config == "" && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		matches, _ := filepath.Glob(filepath.Join(home, "state/hrx/pitput/*", started.Thread.ID, pitputDMKey, "bridge.toml"))
		if len(matches) == 1 {
			config = matches[0]
		}
		select {
		case err := <-l.sidecarErrors:
			t.Fatal(err)
		default:
		}
	}
	if config == "" {
		t.Fatal("direct-message bridge was not configured")
	}
	// The driver sends the DM once the consumer cursor exists.
	os.WriteFile(filepath.Join(home, "dm-bridge-ready"), []byte(config), 0600)
	select {
	case body := <-requests:
		if !strings.Contains(body, "Pitput private direct message") || !strings.Contains(body, "dm read dm-") {
			t.Fatal("direct-message notification did not reach the model")
		}
		if sentinel == "" || strings.Contains(body, sentinel) {
			t.Fatal("notification leaked the direct-message body")
		}
	case err := <-l.sidecarErrors:
		t.Fatal(err)
	case <-time.After(40 * time.Second):
		t.Fatal("direct message was not delivered")
	}
	var snapshot struct {
		Running bool
		Agents  []struct {
			Source  string
			Pending *struct {
				EventKey string `json:"event_key"`
			}
		}
	}
	data, err := exec.Command(r.cli, "bridge", "status", "--config", config).Output()
	if err != nil || json.Unmarshal(data, &snapshot) != nil || len(snapshot.Agents) != 1 || snapshot.Agents[0].Source != "dm" || snapshot.Agents[0].Pending == nil {
		t.Fatal("missing durable pending direct message", err)
	}
	key := snapshot.Agents[0].Pending.EventKey
	client.conn.Close()
	stop()
	// Shutdown never acknowledges: only an explicit handling ACK does.
	data, err = exec.Command(r.cli, "bridge", "status", "--config", config).Output()
	if err != nil || json.Unmarshal(data, &snapshot) != nil || snapshot.Running || snapshot.Agents[0].Pending == nil || snapshot.Agents[0].Pending.EventKey != key {
		t.Fatal("shutdown acknowledged or discarded the direct message")
	}
	if output, err := exec.Command(r.cli, "bridge", "ack", "--config", config, "--agent-id", identity, "--event-key", key).CombinedOutput(); err != nil {
		t.Fatalf("handling ACK failed: %v %s", err, output)
	}
	data, err = exec.Command(r.cli, "bridge", "status", "--config", config).Output()
	if err != nil || json.Unmarshal(data, &snapshot) != nil || snapshot.Agents[0].Pending != nil {
		t.Fatal("direct message still pending after ACK")
	}
}
