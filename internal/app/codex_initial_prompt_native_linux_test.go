package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/coveragecontroller"
)

// Exercise the real TUI's initial turn through the production admission owner.
// The loopback provider returns scripted completions; no paid model or personal config is used.
func TestNativeCodexInitialPromptAdmission(t *testing.T) {
	if os.Getenv("HRX_NATIVE_SESSION_TESTS") != "1" {
		t.Skip("isolated Linux native fixture")
	}
	for _, hook := range []bool{false, true} {
		t.Run(fmt.Sprintf("production-capture-%t", hook), func(t *testing.T) { testNativeCodexInitialPromptAdmission(t, hook) })
	}
}
func testNativeCodexInitialPromptAdmission(t *testing.T, hook bool) {
	home, err := os.MkdirTemp("/tmp", "hrx-init-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	defer func() {
		if t.Failed() {
			script := `import sqlite3,sys,json
c=sqlite3.connect('file:'+sys.argv[1]+'?mode=ro',uri=True)
rows=c.execute("select feedback_log_body from logs where level='ERROR' order by id desc limit 10").fetchall()
fields=['collaborationMode','model','cwd','approvalsReviewer','approvalPolicy','sandboxPolicy','runtimeWorkspaceRoots','effort','serviceTier','serviceTierForTurn','turnTrigger','summary','personality']
print(json.dumps({'scope_failures':[field for field in fields if any('baseline setting mismatch ('+field+')' in (r[0] or '') for r in rows)],'error_count':len(rows)}))`
			output, err := exec.Command("python3", "-c", script, filepath.Join(home, "codex/logs_2.sqlite")).Output()
			if err == nil {
				t.Log(string(output))
			}
		}
	}()

	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, "codex"), 0700); err != nil {
		t.Fatal(err)
	}
	model := "fixture"
	catalogPrefix := ""
	if path := os.Getenv("HRX_NATIVE_CODEX_CATALOG"); path != "" {
		model = os.Getenv("HRX_NATIVE_CODEX_MODEL")
		raw, err := readRemoteCodexCatalog(path, model)
		if err != nil {
			t.Fatal(err)
		}
		catalogPath := filepath.Join(home, "catalog.json")
		if err := os.WriteFile(catalogPath, raw, 0600); err != nil {
			t.Fatal(err)
		}
		catalogPrefix = fmt.Sprintf("model_catalog_json=%q\n", catalogPath)
	}
	requests := make(chan bool, 1)
	var providerCalls atomic.Int32
	var toolReturned atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v1/responses" {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
			select {
			case requests <- strings.Contains(string(body), "HRX_INITIAL_PROMPT_FIXTURE"):
			default:
			}
			call := providerCalls.Add(1)
			if hook && call > 1 {
				var submitted struct {
					Input []struct {
						Type   string `json:"type"`
						CallID string `json:"call_id"`
						Output string `json:"output"`
					} `json:"input"`
				}
				if json.Unmarshal(body, &submitted) == nil {
					for _, item := range submitted.Input {
						if item.Type == "function_call_output" && item.CallID == "call_fixture" && strings.Contains(item.Output, "HRX_NATIVE_TOOL_OUTPUT") && strings.Contains(item.Output, "Process exited with code 0") {
							toolReturned.Store(true)
						}
					}
				}
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"initial-fixture\",\"status\":\"in_progress\",\"output\":[]}}\n\n")
			if hook && call == 1 {
				item := map[string]any{"id": "fc_fixture", "type": "function_call", "call_id": "call_fixture", "name": "exec_command", "arguments": `{"cmd":"printf HRX_NATIVE_TOOL_OUTPUT","max_output_tokens":100}`}
				event, _ := json.Marshal(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
				fmt.Fprintf(w, "data: %s\n\n", event)
			}
			io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"initial-fixture\",\"status\":\"completed\",\"output\":[]}}\n\n")
			return
		}
		http.Error(w, "unexpected fixture request", http.StatusNotFound)
	}))
	defer provider.Close()
	config := fmt.Sprintf("service_tier=\"default\"\ncheck_for_update_on_startup=false\nmodel=\"fixture\"\nmodel_provider=\"fixture\"\napproval_policy=\"never\"\nsandbox_mode=\"read-only\"\n[model_providers.fixture]\nname=\"fixture\"\nbase_url=%q\nwire_api=\"responses\"\nenv_key=\"HRX_FIXTURE_KEY\"\nrequest_max_retries=0\nstream_max_retries=0\n[projects.%q]\ntrust_level=\"trusted\"\n", provider.URL+"/v1", home)
	config = catalogPrefix + strings.Replace(config, "model=\"fixture\"", fmt.Sprintf("model=%q", model), 1)
	if hook {
		config = "model_reasoning_effort=\"low\"\n" + strings.Replace(config, "sandbox_mode=\"read-only\"", "sandbox_mode=\"danger-full-access\"", 1)
	}
	if err := os.WriteFile(filepath.Join(home, "codex/config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	socket := filepath.Join(home, "native.sock")
	launch := Launch{Executable: "codex", Args: []string{"--no-alt-screen", "--cd", home, "HRX_INITIAL_PROMPT_FIXTURE"}}
	if hook {
		launch.Identity, launch.Model = model, model
		launch.pitputRuntime = &pitputRuntime{identity: "test/initial/agent", boards: []string{"fixture"}}
		if err := prepareSessionCapture(ctx, LaunchOptions{project: home, Pitput: true, AgentIdentity: "test/initial/agent"}, &launch, io.Discard); err != nil {
			t.Fatal(err)
		}
		if launch.pitputRuntime.record.AgentIdentity != "test/initial/agent" {
			t.Fatal("capture lost identity binding")
		}
	}
	rawStopServer := creatorOwnedProcess(t, ctx, binary, append(pitputServerArgs(launch.Args), "--listen", "unix://"+socket), home, "initial-server")
	var serverOnce sync.Once
	stopServer := func() { serverOnce.Do(rawStopServer) }
	defer stopServer()
	if err := os.Mkdir(filepath.Join(home, "journal"), 0700); err != nil {
		t.Fatal(err)
	}
	runtime, err := coveragecontroller.NewRuntime(ctx, creatorFixtureConnection(t, ctx, socket), coveragecontroller.RuntimeOptions{SocketRoot: home, JournalRoot: filepath.Join(home, "journal"), Name: "initial-fixture", ExpectedModel: model, ExpectedWorkingDirectory: home})
	if err != nil {
		t.Fatal(err)
	}
	var runtimeOnce sync.Once
	closeRuntime := func() {
		runtimeOnce.Do(func() {
			shutdown, done := context.WithTimeout(context.Background(), 6*time.Second)
			defer done()
			if err := runtime.Close(shutdown); err != nil {
				t.Error(err)
			}
		})
	}
	defer closeRuntime()
	launch.Args = slices.DeleteFunc(launch.Args, func(arg string) bool { return arg == "--no-daemon" })
	helper, err := filepath.Abs("../../tests/compatibility/creator_tui_pty.py")
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := filepath.Join(home, "tui-errors.json")
	rawStopTUI := creatorOwnedProcess(t, ctx, "/usr/bin/env", append([]string{"HRX_TUI_DIAGNOSTIC=" + diagnostic, "python3", "-I", helper, binary, "--remote", runtime.Terminal}, launch.Args...), home, "initial-tui")
	var tuiOnce sync.Once
	stopTUI := func() { tuiOnce.Do(rawStopTUI) }
	defer func() {
		stopTUI()
		if t.Failed() {
			if raw, err := os.ReadFile(diagnostic); err == nil {
				t.Log(string(raw))
			}
		}
	}()
	if _, err := runtime.Ready(ctx); err != nil {
		t.Fatal("native admission failed", err)
	}
	identity, err := runtime.Ready(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(codexObserveRequest{Endpoint: runtime.Board, Session: identity.ThreadID, Project: home})
	var observation bytes.Buffer
	if err := runRemoteCodexObserve(ctx, bytes.NewReader(request), &observation); err != nil {
		t.Fatal(err)
	}
	var evidence codexObserveResult
	if json.Unmarshal(observation.Bytes(), &evidence) != nil || !evidence.Binding {
		t.Fatal("native read did not establish exact binding")
	}
	select {
	case matched := <-requests:
		if !matched {
			t.Fatal("provider request omitted initial prompt")
		}
	case <-ctx.Done():
		t.Fatal("native initial prompt never reached loopback provider")
	}
	waitCompleted := func(endpoint string) {
		t.Helper()
		for {
			request, _ := json.Marshal(codexObserveRequest{Endpoint: endpoint, Session: identity.ThreadID, Project: home})
			var output bytes.Buffer
			if err := runRemoteCodexObserve(ctx, bytes.NewReader(request), &output); err != nil {
				t.Fatal(err)
			}
			var result codexObserveResult
			if json.Unmarshal(output.Bytes(), &result) != nil || !result.Binding {
				t.Fatal("lost native binding")
			}
			if result.CompletedTurns > 0 {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("native history never recorded a completed turn")
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	waitCompleted(runtime.Board)
	stopTUI()
	closeRuntime()
	stopServer()
	resumeSocket := filepath.Join(home, "resume.sock")
	stopResumeServer := creatorOwnedProcess(t, ctx, binary, []string{"app-server", "--listen", "unix://" + resumeSocket}, home, "resume-server")
	defer stopResumeServer()
	resumed, err := coveragecontroller.NewRuntime(ctx, creatorFixtureConnection(t, ctx, resumeSocket), coveragecontroller.RuntimeOptions{SocketRoot: home, JournalRoot: filepath.Join(home, "journal"), Name: "initial-fixture", ExpectedModel: model, ExpectedWorkingDirectory: home, ExpectedThreadID: identity.ThreadID})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		shutdown, done := context.WithTimeout(context.Background(), 6*time.Second)
		defer done()
		if err := resumed.Close(shutdown); err != nil {
			t.Error(err)
		}
	}()
	resumeArgs := []string{"-I", helper, binary, "--remote", resumed.Terminal}
	if hook {
		resumeArgs = append(resumeArgs, "-c", `model_provider="fixture"`, "--model", model, "-c", `model_reasoning_effort="low"`, "--yolo")
	}
	resumeArgs = append(resumeArgs, "resume", identity.ThreadID, "--no-alt-screen", "--cd", home)
	resumeArgs = append(resumeArgs[:5], pitputTerminalArgs(resumeArgs[5:])...)
	resumeDiagnostic := filepath.Join(home, "resume-errors.json")
	stopResumeTUI := creatorOwnedProcess(t, ctx, "/usr/bin/env", append([]string{"HRX_TUI_DIAGNOSTIC=" + resumeDiagnostic, "python3"}, resumeArgs...), home, "resume-tui")
	defer func() {
		stopResumeTUI()
		if t.Failed() {
			if raw, err := os.ReadFile(resumeDiagnostic); err == nil {
				t.Log(string(raw))
			}
		}
	}()
	resumedIdentity, err := resumed.Ready(ctx)
	if err != nil || resumedIdentity.ThreadID != identity.ThreadID {
		t.Fatal("native resume did not retain exact session", err)
	}
	waitCompleted(resumed.Board)

}
