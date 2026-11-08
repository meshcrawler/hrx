package backend

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
)

//go:embed assets/agent_config.py
var agentConfigHelper string

type AgentStart struct {
	Schema     int    `json:"schema"`
	Deployment string `json:"deployment"`
	Identity   string `json:"identity"`
	Harness    string `json:"harness"`
	Session    string `json:"session"`
	Status     string `json:"status"`
	Ready      bool   `json:"ready"`
}

// StartAgent consumes a private configuration stream containing only the selected
// agent's scoped credentials. Successful startup is explicitly not readiness.
func (p Proxmox) StartAgent(ctx context.Context, id string, spec ContainerSpec, configuration io.Reader) (AgentStart, error) {
	var started AgentStart
	payload, err := io.ReadAll(io.LimitReader(configuration, 1024*1024+1))
	if err != nil || len(payload) > 1024*1024 {
		return started, errors.New("private agent configuration unavailable or oversized")
	}
	var expected struct {
		Deployment string `json:"deployment"`
		Identity   string `json:"identity"`
		Harness    string `json:"harness"`
	}
	if json.Unmarshal(payload, &expected) != nil || expected.Deployment != id {
		return started, errors.New("agent configuration deployment mismatch")
	}
	result, err := p.Guest(ctx, id, spec, []string{"python3", "-c", agentConfigHelper}, bytes.NewReader(payload))
	if err != nil {
		return started, err
	}
	if json.Unmarshal([]byte(result.GuestOutput), &started) != nil || started.Schema != 1 || started.Deployment != id || started.Ready || started.Identity != expected.Identity || started.Harness != expected.Harness || (started.Status != "starting" && started.Status != "starting_or_running") || started.Session != "hrx-"+id {
		return AgentStart{}, errors.New("invalid native startup observation")
	}
	return started, nil
}

// ResumeAgent resumes the exact saved native binding without repeating initial
// prompts or assigned work. Readiness remains false until separately verified.
func (p Proxmox) ResumeAgent(ctx context.Context, id string, spec ContainerSpec) (AgentStart, error) {
	payload, _ := json.Marshal(map[string]string{"operation": "resume", "deployment": id})
	result, err := p.Guest(ctx, id, spec, []string{"python3", "-c", agentConfigHelper}, bytes.NewReader(payload))
	if err != nil {
		return AgentStart{}, err
	}
	var started AgentStart
	if json.Unmarshal([]byte(result.GuestOutput), &started) != nil || started.Schema != 1 || started.Deployment != id || started.Session != "hrx-"+id || (started.Harness != "opencode" && started.Harness != "claude" && started.Harness != "codex") || started.Ready || (started.Status != "starting" && started.Status != "starting_or_running") {
		return AgentStart{}, errors.New("invalid native resume observation")
	}
	return started, nil
}

// InstallLease updates only an inactive native worker's scoped credential file.
func (p Proxmox) InstallLease(ctx context.Context, id string, spec ContainerSpec, lease CredentialLease) error {
	private, err := lease.PrivateJSON()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"operation": "replace-lease", "deployment": id, "lease": json.RawMessage(private)})
	if err != nil {
		return err
	}
	result, err := p.Guest(ctx, id, spec, []string{"python3", "-c", agentConfigHelper}, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	var receipt struct {
		Deployment string `json:"deployment"`
		Installed  bool   `json:"credential_installed"`
		Ready      bool   `json:"ready"`
	}
	if json.Unmarshal([]byte(result.GuestOutput), &receipt) != nil || receipt.Deployment != id || !receipt.Installed || receipt.Ready {
		return errors.New("invalid credential installation receipt")
	}
	return nil
}
