package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/coveragecontroller"
)

// This finite #30 driver reuses production launch/start and the Go sole receive
// owner. Python owns only source writes, the scripted provider and observations.
func TestNativePitputCombinedAcceptanceCodex(t *testing.T) {
	if os.Getenv("HRX_PITPUT_COMBINED_ACCEPTANCE") != "1" {
		t.Skip("disposable GitLab acceptance only")
	}
	root, provider, helper := os.Getenv("HRX_PITPUT_COMBINED_CONTROL"), os.Getenv("HRX_PITPUT_COMBINED_PROVIDER"), os.Getenv("HRX_PITPUT_ADMISSION_BINARY")
	if !filepath.IsAbs(root) || !strings.HasPrefix(provider, "http://127.0.0.1:") || !filepath.IsAbs(helper) {
		t.Fatal("BLOCKED: combined acceptance inputs")
	}
	f := &targetedFixture{t: t, harness: "codex", identity: "test/combined/agent", receipt: map[string]any{}, bound: map[string]bool{}}
	f.setup()
	f.gateway.forward = provider
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	_, launch, err := f.launch(ctx, []string{"--pitput", "--pitput-board", targetedBoard, "--pitput-combined-preview", "--agent-identity", f.identity, "--model", "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	launch.pitputRuntime.admissionBinary = helper
	launch.pitputRuntime.attentionTestHook = combinedAcceptanceHook(root)
	stop, rpc, id := f.start(&launch, "")
	running := true
	defer func() {
		if running {
			rpc.conn.Close()
			stop()
		}
	}()
	var config string
	for deadline := time.Now().Add(40 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		paths, _ := filepath.Glob(filepath.Join(os.Getenv("XDG_STATE_HOME"), "hrx/pitput/*", id, "@combined/bridge.toml"))
		if len(paths) == 1 {
			if s, e := f.tryStatus(paths[0]); e == nil && s.Running {
				config = paths[0]
				break
			}
		}
		f.sidecar(&launch)
		if time.Now().After(deadline) {
			t.Fatal("combined acceptance consumer deadline")
		}
	}
	write := func(name string, value any) {
		raw, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		tmp := filepath.Join(root, name+".tmp")
		if os.WriteFile(tmp, raw, 0600) != nil || os.Rename(tmp, filepath.Join(root, name)) != nil {
			t.Fatal("acceptance control write")
		}
	}
	write("acceptance.ready", map[string]string{"config": config, "thread": id})
	for step := 0; step < 1500; step++ {
		name := fmt.Sprintf("command-%04d.json", step)
		var command struct {
			Action string `json:"action"`
		}
		for deadline := time.Now().Add(120 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			raw, e := os.ReadFile(filepath.Join(root, name))
			if e == nil {
				if json.Unmarshal(raw, &command) != nil {
					t.Fatal("invalid acceptance command")
				}
				break
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				t.Fatal("acceptance phase deadline")
			}
		}
		result := map[string]any{"action": command.Action, "thread": id, "running": running}
		switch command.Action {
		case "failure-observe":
			// Failure diagnostics use the same owner and never release input or
			// retry inference. A missing/refused read is a bounded diagnostic,
			// not a replacement for the Python fixture's primary assertion.
			result["failure_observation"] = map[string]any{"status": "owner_stopped"}
			if running {
				bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
				var raw json.RawMessage
				err := rpc.callContext(bounded, "thread/read", map[string]any{"threadId": id, "includeTurns": true}, &raw)
				cancel()
				status := "rpc_unavailable"
				if errors.Is(err, errPitputObservationDeferred) {
					status = "publication_deferred"
				} else if err == nil {
					if projection, parseErr := combinedFailureProjection(raw, id); parseErr == nil {
						result["failure_observation"] = projection
						break
					}
					status = "projection_unavailable"
				}
				result["failure_observation"] = map[string]any{"status": status}
			}
		case "observe", "continuation-observe":
			if !running {
				t.Fatal("observation requires running production owner")
			}
			f.sidecar(&launch)
			var native struct {
				Thread struct {
					ID    string `json:"id"`
					Turns []struct {
						ID        string `json:"id"`
						Status    string `json:"status"`
						ItemsView string `json:"itemsView"`
						Items     []struct {
							Type     string `json:"type"`
							ClientID string `json:"clientId"`
						} `json:"items"`
					} `json:"turns"`
					Status struct {
						Type string `json:"type"`
					} `json:"status"`
				} `json:"thread"`
			}
			observationErr := rpc.call("thread/read", map[string]any{"threadId": id, "includeTurns": command.Action == "continuation-observe"}, &native)
			deferred := errors.Is(observationErr, errPitputObservationDeferred)
			if !deferred && (observationErr != nil || native.Thread.ID != id) {
				// Preserve a concurrently terminated owner's classified cause
				// before test cleanup cancels its reporter, as during startup.
				select {
				case cause := <-launch.sidecarErrors:
					t.Log("native observation owner failed:", cause)
				case <-time.After(time.Second):
				}
				if observationErr != nil {
					t.Fatal("native transport observation failed:", observationErr)
				}
				t.Fatal("native observation thread binding mismatch")
			}
			status, e := f.tryStatusBy(config, time.Now().Add(5*time.Second))
			if e != nil || !status.Running || len(status.Agents) != 1 {
				t.Fatal("controller/bridge unhealthy")
			}
			// A live publication fence can refuse a read during an unfinished
			// lifecycle. Its correlated reply proves transport, never idleness.
			result["observation_deferred"] = deferred
			result["transport_healthy"], result["controller_healthy"], result["native_idle"] = true, status.Agents[0].LastError == nil, !deferred && native.Thread.Status.Type == "idle"
			result["pending"] = status.Agents[0].Pending
			if command.Action == "continuation-observe" && !deferred {
				turns := []map[string]any{}
				for _, turn := range native.Thread.Turns {
					if turn.ItemsView != "full" {
						t.Fatal("continuation observation requires full native history")
					}
					clients := []string{}
					for _, item := range turn.Items {
						if item.Type == "userMessage" {
							clients = append(clients, item.ClientID)
						}
					}
					turns = append(turns, map[string]any{"id": turn.ID, "status": turn.Status, "client_ids": clients})
				}
				result["native_turns"] = turns // IDs/status only; never native message bodies.
			}
		case "stop":
			if !running {
				t.Fatal("duplicate acceptance stop")
			}
			rpc.conn.Close()
			stop()
			running = false
			result["owner_stopped"] = true
		case "resume":
			if running {
				t.Fatal("duplicate acceptance resume")
			}
			_, resumed, e := f.launch(ctx, []string{"--resume", id, "--agent-identity", f.identity})
			if e != nil || !resumed.pitputRuntime.combined {
				t.Fatal("combined resume failed", e)
			}
			resumed.pitputRuntime.admissionBinary = helper
			resumed.pitputRuntime.attentionTestHook = combinedAcceptanceHook(root)
			var resumedID string
			stop, rpc, resumedID = f.start(&resumed, id)
			if resumedID != id {
				t.Fatal("native resume changed session")
			}
			launch = resumed
			running = true
			// The session receipt precedes bridge configuration and startup.
			// As on fresh launch, finish startup before reporting resume ready;
			// subsequent health observations must still fail immediately.
			for deadline := time.Now().Add(40 * time.Second); ; time.Sleep(100 * time.Millisecond) {
				f.sidecar(&launch)
				status, e := f.tryStatusBy(config, deadline)
				if e == nil && status.Running && len(status.Agents) == 1 {
					break
				}
				if time.Now().After(deadline) || ctx.Err() != nil {
					t.Fatal("combined acceptance resumed consumer deadline")
				}
			}
		case "busy":
			if !running {
				t.Fatal("busy requires native owner")
			}
			if rpc.call("turn/start", map[string]any{"threadId": id, "input": []map[string]string{{"type": "text", "text": "fixture-authorized-busy-turn"}}}, nil) != nil {
				t.Fatal("foreground busy turn failed")
			}
		case "done":
			if !running {
				t.Fatal("acceptance ended stopped")
			}
			var native json.RawMessage
			if rpc.call("thread/read", map[string]any{"threadId": id, "includeTurns": true}, &native) != nil {
				t.Fatal("native completion unavailable")
			}
			for _, source := range []string{"board", "dm", "ask"} {
				if !strings.Contains(string(native), "fixture-combined-complete:"+source) {
					t.Fatal("native post-tool completion missing:", source)
				}
			}
			if s, e := f.tryStatus(config); e != nil || !s.Running || s.Agents[0].Pending != nil {
				t.Fatal("acceptance pending work remains")
			}
			result["native_post_tool_completion"] = true
			write(fmt.Sprintf("result-%04d.json", step), result)
			return
		default:
			t.Fatal("unsupported acceptance action")
		}
		result["running"] = running
		write(fmt.Sprintf("result-%04d.json", step), result)
	}
	t.Fatal("acceptance command budget exceeded")
}

// Only this test supplies the callback. The controller's durable stage version,
// not this marker, establishes whether a native write was impossible.
func combinedAcceptanceHook(root string) func(context.Context, coveragecontroller.AttentionTestPoint) error {
	return func(ctx context.Context, point coveragecontroller.AttentionTestPoint) error {
		path := filepath.Join(root, "owner-fault.json")
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		var armed struct {
			Phase string `json:"phase"`
		}
		if json.Unmarshal(raw, &armed) != nil {
			return fmt.Errorf("invalid owned fault phase")
		}
		if armed.Phase != point.Phase {
			return nil
		}
		if point.Phase != coveragecontroller.AttentionPrepared && point.Phase != coveragecontroller.AttentionQueueResponse {
			return fmt.Errorf("unsupported owned fault phase")
		}
		if err = os.Rename(path, path+".claimed"); err != nil {
			return err
		}
		observed := map[string]any{"phase": point.Phase, "thread_id": point.ThreadID, "delivery_id": point.DeliveryID, "payload_sha256": point.PayloadSHA256, "attempt": point.Attempt, "response_ordinal": point.ResponseOrdinal}
		raw, err = json.Marshal(observed)
		if err != nil {
			return err
		}
		temporary := filepath.Join(root, "owner-fault-entered.json.tmp")
		if err = os.WriteFile(temporary, raw, 0600); err != nil {
			return err
		}
		if err = os.Rename(temporary, filepath.Join(root, "owner-fault-entered.json")); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
}

func TestCombinedAcceptanceHookPublishesOnlyExactArmedBoundary(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "owner-fault.json"), []byte(`{"phase":"prepared"}`), 0600); err != nil {
		t.Fatal(err)
	}
	hook := combinedAcceptanceHook(root)
	if err := hook(context.Background(), coveragecontroller.AttentionTestPoint{Phase: coveragecontroller.AttentionQueueResponse}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "owner-fault-entered.json")); !os.IsNotExist(err) {
		t.Fatal("unarmed phase produced witness")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	point := coveragecontroller.AttentionTestPoint{Phase: coveragecontroller.AttentionPrepared, ThreadID: "thread", DeliveryID: "delivery", PayloadSHA256: "digest", Attempt: 1}
	if err := hook(ctx, point); err != context.Canceled {
		t.Fatal("hook did not stop on owned cancellation", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "owner-fault-entered.json"))
	if err != nil {
		t.Fatal(err)
	}
	var facts map[string]any
	if json.Unmarshal(raw, &facts) != nil || facts["phase"] != "prepared" || facts["thread_id"] != "thread" || facts["response_ordinal"] != float64(0) {
		t.Fatal("hook facts changed")
	}
}
