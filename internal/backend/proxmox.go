package backend

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

//go:embed assets/proxmox.py
var proxmoxHelper string

// ContainerSpec contains no credentials. LaunchDigest binds the complete desired
// launch configuration so reusing an ID cannot silently repurpose an agent.
type RelaySpec struct {
	ListenAddress   string `json:"listen_address" toml:"listen_address"`
	UpstreamAddress string `json:"upstream_address" toml:"upstream_address"`
	UpstreamPort    int    `json:"upstream_port" toml:"upstream_port"`
}

type ContainerSpec struct {
	Relay        *RelaySpec `json:"relay,omitempty" toml:"relay"`
	Template     string     `json:"template" toml:"template"`
	Storage      string     `json:"storage" toml:"storage"`
	Bridge       string     `json:"bridge" toml:"bridge"`
	Cores        int        `json:"cores" toml:"cores"`
	MemoryMiB    int        `json:"memory_mib" toml:"memory_mib"`
	DiskGiB      int        `json:"disk_gib" toml:"disk_gib"`
	Tags         []string   `json:"tags" toml:"tags"`
	LaunchDigest string     `json:"launch_digest" toml:"-"`
}

type Deployment struct {
	RelayPort int `json:"relay_port,omitempty"`

	GuestOutput     string `json:"guest_output,omitempty"`
	Schema          int    `json:"schema"`
	ID              string `json:"deployment"`
	Digest          string `json:"digest"`
	VMID            int    `json:"vmid"`
	Node            string `json:"node"`
	Phase           string `json:"phase"`
	Marker          string `json:"marker"`
	ContainerStatus string `json:"container_status,omitempty"`
}

// Proxmox keeps lifecycle ownership on the host, shared by independent controllers.
// This establishes container state only: created/running never means agent-ready.
type Proxmox struct {
	Host      string
	Transport Transport
}

func (p Proxmox) Reconcile(ctx context.Context, operation, id string, spec ContainerSpec) (Deployment, error) {
	return p.request(ctx, operation, id, spec, nil, nil)
}

// Guest executes only inside a running, ownership-verified container while holding
// the lifecycle lock. Input is streamed; no credentials enter host command arguments.
func (p Proxmox) Guest(ctx context.Context, id string, spec ContainerSpec, command []string, input io.Reader) (Deployment, error) {
	return p.request(ctx, "guest", id, spec, command, input)
}

func (p Proxmox) request(ctx context.Context, operation, id string, spec ContainerSpec, command []string, stream io.Reader) (Deployment, error) {
	var zero Deployment
	if p.Transport == nil {
		return zero, errors.New("Proxmox transport is required")
	}
	switch operation {
	case "ensure", "inspect", "start", "stop", "destroy", "guest":
	default:
		return zero, errors.New("unsupported Proxmox operation")
	}
	if spec.Tags == nil {
		spec.Tags = []string{}
	}
	serializedSpec, err := json.Marshal(spec)
	if err != nil {
		return zero, err
	}
	var canonical map[string]any
	if err = json.Unmarshal(serializedSpec, &canonical); err != nil {
		return zero, err
	}
	canonicalJSON, err := json.Marshal(canonical)
	if err != nil {
		return zero, err
	}
	expectedDigest := fmt.Sprintf("%x", sha256.Sum256(canonicalJSON))
	request := struct {
		Operation  string        `json:"operation"`
		Deployment string        `json:"deployment"`
		Spec       ContainerSpec `json:"spec"`
		Command    []string      `json:"command,omitempty"`
	}{operation, id, spec, command}
	input, err := json.Marshal(request)
	if err != nil {
		return zero, err
	}
	output, err := p.Transport.Run(ctx, p.Host, []string{"python3", "-c", proxmoxHelper}, io.MultiReader(bytes.NewReader(append(input, '\n')), readerOrEmpty(stream)))
	if err != nil {
		return zero, err
	}
	var response struct {
		OK         bool       `json:"ok"`
		Error      string     `json:"error"`
		Deployment Deployment `json:"deployment"`
	}
	if json.Unmarshal(output, &response) != nil {
		return zero, errors.New("invalid Proxmox lifecycle response")
	}
	if !response.OK {
		return zero, fmt.Errorf("Proxmox lifecycle refused: %s", response.Error)
	}
	d := response.Deployment
	if d.Schema != 1 || d.ID != id || d.VMID < 100 || d.Node == "" || d.Digest != expectedDigest || d.Marker != "hrx:v1:"+id+":"+d.Digest {
		return zero, errors.New("Proxmox response does not match deployment identity")
	}
	return d, nil
}

func readerOrEmpty(input io.Reader) io.Reader {
	if input == nil {
		return bytes.NewReader(nil)
	}
	return input
}
