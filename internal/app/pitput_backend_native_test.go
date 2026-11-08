package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Reuse the production-path fixture, not a second OpenCode launcher. The Python
// owner authors the fixed transcript and observes real native shell results.
func TestNativePitputCombinedAcceptanceOpenCode(t *testing.T) {
	if os.Getenv("CI") != "true" || os.Getenv("HRX_PITPUT_BACKEND_ACCEPTANCE") != "opencode" {
		t.Skip("disposable GitLab backend acceptance only")
	}
	root := os.Getenv("HRX_PITPUT_COMBINED_CONTROL")
	if root == "" || os.Getenv("HRX_PITPUT_COMBINED_PROVIDER") == "" {
		t.Fatal("BLOCKED: backend fixture inputs missing")
	}
	readyPublished := false
	startupStage, startupCode := "setup", "native_fixture_failure"
	defer func() {
		if !readyPublished {
			data, _ := json.Marshal(map[string]string{"stage": startupStage, "code": startupCode})
			_ = os.WriteFile(filepath.Join(root, "backend-failure.json"), data, 0600)
		}
	}()
	f := &targetedFixture{t: t, harness: "opencode", identity: "test/combined/agent", receipt: map[string]any{}, bound: map[string]bool{}}
	f.setup()
	f.gateway.forward = os.Getenv("HRX_PITPUT_COMBINED_PROVIDER")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	_, launch, err := f.launch(ctx, backendOpenCodeFixtureArgs(f.identity, ""))
	if err != nil {
		t.Fatal(err)
	}
	stop, _, id := f.start(&launch, "")
	var stopClient func() error
	var clientAlive func() bool
	clientCloseClean := true
	closeClient := func() {
		if stopClient != nil {
			if err := stopClient(); err != nil {
				clientCloseClean = false
				t.Error(err)
			}
			stopClient = nil
			clientAlive = nil
		}
	}
	defer func() { closeClient(); stop() }()
	write := func(path string, value any) {
		data, err := json.Marshal(value)
		if err != nil || os.WriteFile(path+".tmp", data, 0600) != nil || os.Rename(path+".tmp", path) != nil {
			t.Fatal("backend control publication failed")
		}
	}
	config := ""
	for deadline := time.Now().Add(30 * time.Second); config == ""; time.Sleep(100 * time.Millisecond) {
		paths, _ := filepath.Glob(filepath.Join(os.Getenv("XDG_STATE_HOME"), "hrx/pitput/*", id, "@combined/bridge.toml"))
		if len(paths) == 1 {
			config = paths[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("combined OpenCode config missing")
		}
	}
	transportSafe := func() (string, string, string, error) {
		data, err := os.ReadFile(config)
		var value struct {
			Endpoint string `toml:"opencode_url"`
			Password string `toml:"opencode_password_file"`
			State    string `toml:"state_directory"`
		}
		if err != nil || toml.Unmarshal(data, &value) != nil {
			return "", "", "", fmt.Errorf("backend config unavailable")
		}
		secret, err := os.ReadFile(value.Password)
		if err != nil {
			return "", "", "", fmt.Errorf("backend credential unavailable")
		}
		return value.Endpoint, string(secret), value.State, nil
	}
	transport := func() (string, string, string) {
		endpoint, password, state, err := transportSafe()
		if err != nil {
			t.Fatal(err)
		}
		return endpoint, password, state
	}
	observe := func(stage string) map[string]any {
		startupStage = stage
		endpoint, password, state := transport()
		path := filepath.Join(state, fmt.Sprintf("%x", sha256.Sum256([]byte(f.identity))), "state.json")
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			data = []byte(`{"pending":null}`)
		} else if err != nil {
			t.Fatal("backend spool observation unavailable")
		}
		messageID, err := combinedOpenCodeMessageID(data)
		if err != nil {
			startupCode = backendOpenCodeFailureCode(err)
			t.Fatal(err)
		}
		value, err := backendOpenCodeObservation(id, f.project, messageID, func(path string, result any) error {
			return openCodeRequest(ctx, endpoint, password, "GET", path, nil, result)
		})
		if err != nil {
			startupCode = backendOpenCodeFailureCode(err)
			t.Fatal(err)
		}
		value["stage"] = stage
		if stage == "after_server_restart_before_client" {
			write(filepath.Join(root, "native-resume-proof.json"), value)
		}
		value["native_client_alive"] = clientAlive != nil && clientAlive()
		if (stage == "fresh_client_started" || stage == "after_production_client_started") && value["native_client_alive"] != true {
			startupCode = "attachment_early_exit"
			t.Fatal("native attachment exited before observation")
		}
		return value
	}
	activate := func() error {
		startupStage = "client_start"
		if !launch.Yolo || !nativeHasOption("opencode", launch.Args, "--auto") {
			return fmt.Errorf("explicit fixture permissions missing")
		}
		endpoint, password, _ := transport()
		var session struct{ Data openCodeSession }
		if openCodeRequest(ctx, endpoint, password, "GET", "/api/session/"+id, nil, &session) != nil || validateOpenCodeBinding(session.Data, id, f.project) != nil {
			startupCode = "observation_binding"
			return fmt.Errorf("native observation session binding mismatch")
		}
		instruction, err := ownedBackendOpenCodeInstruction(&launch)
		if err != nil {
			startupCode = backendOpenCodeFailureCode(err)
			return err
		}
		write(filepath.Join(root, "owned-primer.json"), map[string]any{
			"session": id, "text": instruction.Text, "sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(instruction.Text))),
			"instruction_path": instruction.Path, "instruction_root": instruction.Root, "exact_instruction_binding": true,
			"exact_session_binding": true,
		})
		clientAlive, stopClient, err = startBackendOpenCodeClient(ctx, &launch, id, endpoint)
		if err != nil {
			startupCode = backendOpenCodeFailureCode(err)
		}
		return err
	}
	if err := activate(); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(root, "backend.ready"), map[string]any{"thread": id, "config": config,
		"fixture_permission_mode": "explicit_yolo",
		"native_lifecycle":        []map[string]any{observe("fresh_client_started")}})
	readyPublished = true
	running := true
	controllerHealthy := true
	for step := 0; ; step++ {
		path := filepath.Join(root, "command-"+formatBackendStep(step)+".json")
		var command struct {
			Action   string `json:"action"`
			CallHash string `json:"call_sha256"`
			Deadline int64  `json:"deadline_unix_ms"`
		}
		for {
			data, err := os.ReadFile(path)
			if err == nil && json.Unmarshal(data, &command) == nil {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("backend command deadline")
			case <-time.After(100 * time.Millisecond):
			}
		}
		var lifecycle []map[string]any
		if command.Action == "failure-observe" {
			value := map[string]any{"thread": id, "failure_observation": map[string]any{"status": "unavailable"}}
			if running {
				deadline, err := backendFailureDeadline(command.Deadline, time.Now())
				if err == nil {
					observationCtx, done := context.WithDeadline(ctx, deadline)
					endpoint, password, state, transportErr := transportSafe()
					var data []byte
					readErr := transportErr
					if transportErr == nil {
						data, readErr = os.ReadFile(filepath.Join(state, fmt.Sprintf("%x", sha256.Sum256([]byte(f.identity))), "state.json"))
					}
					messageID, identityErr := combinedOpenCodeMessageID(data)
					if transportErr == nil && readErr == nil && identityErr == nil {
						observed, observationErr := backendOpenCodeFailureObservation(id, f.project, messageID, command.CallHash, func(path string, result any) error {
							return openCodeRequest(observationCtx, endpoint, password, "GET", path, nil, result)
						})
						if observationErr == nil {
							observed["status"] = "observed"
							observed["native_client_alive"] = clientAlive != nil && clientAlive()
							value["failure_observation"] = observed
						} else if observationCtx.Err() != nil {
							value["failure_observation"] = map[string]any{"status": "deadline"}
						} else {
							value["failure_observation"] = map[string]any{"status": "unavailable", "code": backendOpenCodeFailureCode(observationErr)}
						}
					}
					done()
				}
			}
			// Failure diagnostics never terminate the owner or hide its original
			// assertion/cleanup result if their own file publication fails.
			path := filepath.Join(root, "result-"+formatBackendStep(step)+".json")
			data, _ := json.Marshal(value)
			if os.WriteFile(path+".tmp", data, 0600) == nil {
				_ = os.Rename(path+".tmp", path)
			}
			continue
		}
		switch command.Action {
		case "stop":
			lifecycle = append(lifecycle, observe("before_stop"))
			closeClient()
			stop()
			running = false
		case "resume":
			lifecycle, err = backendOpenCodeResumeLifecycle(func() error {
				_, resumed, err := f.launch(ctx, backendOpenCodeFixtureArgs(f.identity, id))
				if err != nil || !resumed.pitputRuntime.combined {
					t.Fatal("OpenCode combined resume failed", err)
				}
				launch = resumed
				stop, _, _ = f.start(&launch, id)
				return nil
			}, observe, activate)
			if err != nil {
				t.Fatal(err)
			}
			running, controllerHealthy = true, true
		case "busy", "observe", "done":
		default:
			t.Fatal("unknown backend control")
		}
		result := map[string]any{"thread": id, "config": config, "transport_healthy": false, "controller_healthy": false, "native_idle": false, "native_stop_verified": !running && clientCloseClean && stopClient == nil && clientAlive == nil}
		if len(lifecycle) != 0 {
			result["native_lifecycle"] = lifecycle
		}
		if running {
			data, err := os.ReadFile(config)
			var value struct {
				Endpoint string `toml:"opencode_url"`
				Password string `toml:"opencode_password_file"`
			}
			if err != nil || toml.Unmarshal(data, &value) != nil {
				t.Fatal("backend config unavailable")
			}
			secret, err := os.ReadFile(value.Password)
			if err != nil {
				t.Fatal("backend credential unavailable")
			}
			if command.Action == "busy" {
				err = openCodeRequest(ctx, value.Endpoint, string(secret), "POST", "/api/session/"+id+"/prompt", map[string]any{
					"id": "msg_combined_busy_" + strconv.Itoa(step), "text": "fixture-authorized-busy-turn",
					"delivery": "queue", "resume": true,
				}, nil)
				if err != nil {
					t.Fatal("backend busy admission failed")
				}
			}
			var inbox struct{ Data []json.RawMessage }
			err = openCodeRequest(ctx, value.Endpoint, string(secret), "GET", "/api/session/"+id+"/inbox", nil, &inbox)
			select {
			case <-launch.sidecarErrors:
				controllerHealthy = false
			default:
			}
			result["native_client_alive"] = clientAlive != nil && clientAlive()
			result["transport_healthy"] = err == nil && result["native_client_alive"] == true
			result["controller_healthy"] = controllerHealthy
			result["native_idle"] = err == nil && len(inbox.Data) == 0
		}
		write(filepath.Join(root, "result-"+formatBackendStep(step)+".json"), result)
		if command.Action == "done" {
			return
		}
	}
}

func formatBackendStep(step int) string {
	value := strconv.Itoa(step)
	for len(value) < 4 {
		value = "0" + value
	}
	return value
}
