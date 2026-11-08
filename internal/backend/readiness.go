package backend

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"time"
)

//go:embed assets/readiness_observe.py
var readinessObserver string

//go:embed assets/readiness_controller.py
var readinessController string

type ReadyReceipt struct {
	Session   string `json:"session"`
	Monitor   string `json:"monitor"`
	EventKey  string `json:"event_key"`
	ReplyPath string `json:"reply_path"`
}

type readinessObservation struct {
	Bound         bool     `json:"bound"`
	NativeBinding bool     `json:"native_binding"`
	Terminated    bool     `json:"terminated"`
	Pending       bool     `json:"pending"`
	Session       string   `json:"session"`
	Monitor       string   `json:"monitor"`
	Actions       []string `json:"native_actions"`
	Events        []struct {
		Key  string `json:"event_key"`
		Path string `json:"path"`
	} `json:"handled_events"`
}

// WaitReady creates the challenge only after native binding. It observes real
// worker actions; it never reads source content or acknowledges as the worker.
// A deadline leaves the deployment intact and does not imply process termination.
func (p Proxmox) WaitReady(ctx context.Context, id string, spec ContainerSpec, python, controller, identity, board string) (ReadyReceipt, error) {
	call := func(action, monitor, event string) ([]byte, error) {
		payload, _ := json.Marshal(map[string]string{"action": action, "identity": identity, "deployment": id, "board": board, "controller": controller, "monitor": monitor, "event_key": event})
		command := exec.CommandContext(ctx, python, "-c", readinessController)
		command.Stdin = bytes.NewReader(payload)
		output := &boundedOutput{limit: 1024 * 1024}
		command.Stdout = output
		if command.Run() != nil || output.overflow {
			return nil, errors.New("Pitput readiness controller operation failed")
		}
		return output.buffer.Bytes(), nil
	}
	issued := false
	session := ""
	for {
		result, err := p.Guest(ctx, id, spec, []string{"python3", "-c", readinessObserver, identity, board, id}, nil)
		if err != nil {
			return ReadyReceipt{}, err
		}
		var observation readinessObservation
		if json.Unmarshal([]byte(result.GuestOutput), &observation) != nil {
			return ReadyReceipt{}, errors.New("invalid native readiness observation")
		}
		if observation.Terminated {
			return ReadyReceipt{}, errors.New("native process exited; inspect retained startup diagnostics")
		}
		if observation.Bound && observation.NativeBinding {
			if session != "" && session != observation.Session {
				return ReadyReceipt{}, errors.New("native session changed during readiness")
			}
			session = observation.Session
			if !issued {
				if _, err = call("challenge", "", ""); err != nil {
					return ReadyReceipt{}, err
				}
				issued = true
			}
			for _, event := range observation.Events {
				topic := "/b/" + board + "/hrx-readiness-" + id
				if event.Path != topic && !strings.HasPrefix(event.Path, topic+"/") {
					continue
				}
				raw, e := call("observe", observation.Monitor, event.Key)
				if e != nil {
					return ReadyReceipt{}, e
				}
				var evidence struct {
					Count        int      `json:"reply_count"`
					Reply        string   `json:"reply_path"`
					Dispositions []string `json:"dispositions"`
				}
				if json.Unmarshal(raw, &evidence) != nil {
					return ReadyReceipt{}, errors.New("invalid server readiness evidence")
				}
				if evidence.Count > 1 {
					return ReadyReceipt{}, errors.New("duplicate readiness replies")
				}
				if !observation.Pending && evidence.Count == 1 && slices.Contains(evidence.Dispositions, "read") && slices.Contains(evidence.Dispositions, "handled") && slices.Contains(observation.Actions, "read") && slices.Contains(observation.Actions, "post") && slices.Contains(observation.Actions, "ack") {
					return ReadyReceipt{Session: session, Monitor: observation.Monitor, EventKey: event.Key, ReplyPath: evidence.Reply}, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ReadyReceipt{}, errors.New("readiness observation deadline; deployment retained")
		case <-time.After(2 * time.Second):
		}
	}
}
