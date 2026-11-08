package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Only the native worker may read/post/ack. The controller creates a challenge
// and reads receipts; it never mutates worker acknowledgment state.
func verifyLiveReadiness(t *testing.T, ctx context.Context, p Proxmox, id string, spec ContainerSpec, identity, board string) {
	verifyLiveReadinessMode(t, ctx, p, id, spec, identity, board, true)
}

func verifyLiveReadinessMode(t *testing.T, ctx context.Context, p Proxmox, id string, spec ContainerSpec, identity, board string, issue bool) {
	t.Helper()
	python := os.Getenv("HRX_AGENT_LIVE_READINESS_PYTHON")
	if python == "" {
		t.Log("readiness exchange not requested; startup proof only")
		return
	}
	controller := os.Getenv("HRX_AGENT_LIVE_CONTROLLER")
	if controller == "" {
		t.Fatal("explicit readiness controller identity required")
	}
	helper, err := os.ReadFile("assets/readiness_controller.py")
	if err != nil {
		t.Fatal(err)
	}
	observe, err := os.ReadFile("assets/readiness_observe.py")
	if err != nil {
		t.Fatal(err)
	}
	call := func(action, monitor, event string) []byte {
		request, _ := json.Marshal(map[string]string{"action": action, "identity": identity, "deployment": id, "board": board, "controller": controller, "monitor": monitor, "event_key": event})
		command := exec.CommandContext(ctx, python, "-c", string(helper))
		command.Stdin = bytes.NewReader(request)
		output, e := command.Output()
		if e != nil {
			t.Fatal("readiness controller operation failed; private diagnostics suppressed")
		}
		return output
	}
	if issue {
		call("challenge", "", "")
		t.Logf("issued readiness challenge /b/%s/hrx-readiness-%s", board, id)
	}
	deadline := time.Now().Add(5 * time.Minute)
	previousDiagnostic := ""
	for time.Now().Before(deadline) {
		result, e := p.Guest(ctx, id, spec, []string{"python3", "-c", string(observe), identity, board, id}, nil)
		if e != nil {
			t.Fatal(e)
		}
		var observation struct {
			Retries []struct {
				Attempt int `json:"attempt"`
				Status  int `json:"status"`
			} `json:"provider_retries"`
			Bound         bool     `json:"bound"`
			NativeBinding bool     `json:"native_binding"`
			Terminated    bool     `json:"terminated"`
			Pending       bool     `json:"pending"`
			Monitor       string   `json:"monitor"`
			Actions       []string `json:"native_actions"`
			Events        []struct {
				Key  string `json:"event_key"`
				Path string `json:"path"`
			} `json:"handled_events"`
		}
		if json.Unmarshal([]byte(result.GuestOutput), &observation) != nil {
			t.Fatal("invalid native readiness observation")
		}
		diagnostic, _ := json.Marshal(map[string]any{"bound": observation.Bound, "native_binding": observation.NativeBinding, "pending": observation.Pending, "actions": observation.Actions, "provider_retries": observation.Retries})
		if string(diagnostic) != previousDiagnostic {
			t.Logf("readiness observation: %s", diagnostic)
			previousDiagnostic = string(diagnostic)
		}
		if observation.Terminated {
			t.Fatal("native worker exited during readiness")
		}
		for _, event := range observation.Events {
			topic := "/b/" + board + "/hrx-readiness-" + id
			if event.Path != topic && !strings.HasPrefix(event.Path, topic+"/") {
				continue
			}
			var evidence struct {
				Replies      int      `json:"reply_count"`
				Dispositions []string `json:"dispositions"`
			}
			if json.Unmarshal(call("observe", observation.Monitor, event.Key), &evidence) != nil {
				t.Fatal("invalid readiness evidence")
			}
			has := func(values []string, want string) bool {
				for _, v := range values {
					if v == want {
						return true
					}
				}
				return false
			}
			if evidence.Replies > 1 {
				t.Fatal("duplicate readiness replies")
			}
			if observation.Bound && observation.NativeBinding && !observation.Pending && evidence.Replies == 1 && has(evidence.Dispositions, "read") && has(evidence.Dispositions, "handled") && has(observation.Actions, "read") && has(observation.Actions, "post") && has(observation.Actions, "ack") {
				t.Logf("verified native session/project binding and real read/post/ACK event=%s", event.Key)
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
	t.Fatal(fmt.Sprintf("readiness observation deadline for %s; guest retained", id))
}

// Re-check an existing fresh-run exchange without posting or starting anything.
// Useful when an observer defect is corrected after the worker has finished.
func TestProxmoxRetainedReadinessEvidence(t *testing.T) {
	id := os.Getenv("HRX_READINESS_LIVE_DEPLOYMENT")
	if id == "" {
		t.Skip("requires explicit retained deployment")
	}
	host, template := os.Getenv("HRX_PROXMOX_LIVE_HOST"), os.Getenv("HRX_PROXMOX_LIVE_TEMPLATE")
	identity, board := os.Getenv("HRX_AGENT_LIVE_IDENTITY"), os.Getenv("HRX_AGENT_LIVE_BOARD")
	if host == "" || template == "" || identity == "" || board == "" || os.Getenv("HRX_AGENT_LIVE_READINESS_PYTHON") == "" {
		t.Fatal("explicit retained readiness configuration required")
	}
	spec := ContainerSpec{Template: template, Storage: "local-lvm", Bridge: "vmbr1", Cores: 2, MemoryMiB: 2048, DiskGiB: 8, Tags: []string{"hrx-native-acceptance"}, LaunchDigest: fmt.Sprintf("%064x", 2)}
	if relay := os.Getenv("HRX_PROXMOX_LIVE_RELAY"); relay != "" {
		spec.Relay = &RelaySpec{}
		if err := json.Unmarshal([]byte(relay), spec.Relay); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	verifyLiveReadinessMode(t, ctx, Proxmox{Host: host, Transport: SSH{}}, id, spec, identity, board, false)
}
