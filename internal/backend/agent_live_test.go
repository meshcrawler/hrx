package backend

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Explicitly opted-in native startup proof. A failed observation retains the
// guest, but always attempts lease revocation. Startup is not readiness.
func TestProxmoxNativeAgentStartup(t *testing.T) {
	configPath := os.Getenv("HRX_AGENT_LIVE_CONFIG")
	if configPath == "" {
		t.Skip("requires explicit private agent configuration")
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	host, template, directory := os.Getenv("HRX_PROXMOX_LIVE_HOST"), os.Getenv("HRX_PROXMOX_LIVE_TEMPLATE"), os.Getenv("HRX_PROXMOX_LIVE_BUNDLE")
	if host == "" || template == "" || directory == "" {
		t.Fatal("explicit host, template and verified bundle required")
	}
	if os.Getenv("HRX_AGENT_LIVE_READINESS_PYTHON") != "" && os.Getenv("HRX_AGENT_LIVE_CONTROLLER") == "" {
		t.Fatal("HRX_AGENT_LIVE_CONTROLLER is required before allocating a readiness deployment")
	}
	private, err := os.ReadFile(configPath)
	must(err)
	var configuration map[string]any
	must(json.Unmarshal(private, &configuration))
	identity, ok := configuration["identity"].(string)
	if !ok || identity == "" {
		t.Fatal("identity required")
	}
	boards, ok := configuration["boards"].([]any)
	if !ok || len(boards) != 1 {
		t.Fatal("exactly one readiness board required")
	}
	board, ok := boards[0].(string)
	if !ok {
		t.Fatal("invalid board")
	}
	model, ok := configuration["model"].(string)
	if !ok {
		t.Fatal("model required")
	}
	auth, err := os.ReadFile(os.Getenv("HRX_LEASE_LIVE_AUTH_COMMAND"))
	must(err)
	var argv []string
	must(json.Unmarshal(auth, &argv))
	if len(argv) == 0 {
		t.Fatal("auth command required")
	}
	broker := LeaseBroker{URL: os.Getenv("HRX_LEASE_LIVE_URL"), Authenticate: func(ctx context.Context) (string, error) {
		output, e := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
		if e != nil {
			return "", fmt.Errorf("controller authentication failed")
		}
		return strings.TrimSpace(string(output)), nil
	}}
	raw := make([]byte, 16)
	_, err = rand.Read(raw)
	must(err)
	id := fmt.Sprintf("%x", raw)
	if saved := os.Getenv("HRX_PROXMOX_LIVE_DEPLOYMENT"); saved != "" {
		id = saved
	}
	configuration["deployment"] = id
	configuration["readiness_only"] = true
	configuration["initial_prompt"] = "If this is Claude, first start the exact HRX-owned background monitor tasks provided in your launch instructions, then wait for their notifications. You are " + identity + " in a fresh-container readiness test. Your only task is the current HRX/Pitput readiness notification. Use its exact attention handle and all continuation fragments, post the requested READY value with its stable request ID, verify the real receipt, then run the exact Handle ACK command supplied with that notification. Do not guess message paths, browse other threads, poll the forum, or hide command failures with echo or shell pipelines. If no notification is present, end your turn and wait for delivery."

	spec := ContainerSpec{Template: template, Storage: "local-lvm", Bridge: "vmbr1", Cores: 2, MemoryMiB: 2048, DiskGiB: 8, Tags: []string{"hrx-native-acceptance"}, LaunchDigest: fmt.Sprintf("%064x", 2)}
	if rawRelay := os.Getenv("HRX_PROXMOX_LIVE_RELAY"); rawRelay != "" {
		spec.Relay = &RelaySpec{}
		must(json.Unmarshal([]byte(rawRelay), spec.Relay))
	}
	p := Proxmox{Host: host, Transport: SSH{}}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	if python := os.Getenv("HRX_AGENT_LIVE_READINESS_PYTHON"); python != "" {
		profile, e := EnrollAgent(ctx, python, identity, []string{board})
		must(e)
		selected, e := profile.PrivateJSON()
		must(e)
		configuration["pitput"] = json.RawMessage(selected)
		t.Log("verified automatic scoped Pitput enrollment")
	}
	t.Logf("deployment=%s host=%s; retain guest on failure", id, host)
	deployment, err := p.Reconcile(ctx, "ensure", id, spec)
	must(err)
	t.Logf("allocated vmid=%d", deployment.VMID)
	started, err := p.Reconcile(ctx, "start", id, spec)
	must(err)
	if spec.Relay != nil {
		gateway, e := url.Parse(configuration["gateway_url"].(string))
		must(e)
		hostname := gateway.Hostname()
		gateway.Host = net.JoinHostPort(hostname, strconv.Itoa(started.RelayPort))
		configuration["gateway_url"] = gateway.String()
		configuration["hosts"] = map[string]string{hostname: spec.Relay.ListenAddress}
		t.Logf("managed gateway relay port=%d", started.RelayPort)
	}
	data, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	must(err)
	var bundle Bundle
	must(json.Unmarshal(data, &bundle))
	for i := range bundle.Artifacts {
		a := &bundle.Artifacts[i]
		a.Path = filepath.Join(directory, a.Name)
		if a.Kind == "wheel" {
			a.Path = filepath.Join(directory, "wheels", a.Name)
		}
	}
	_, err = p.Bootstrap(ctx, id, spec, bundle)
	must(err)
	t.Log("verified runtime bootstrap")
	if spec.Relay != nil {
		gateway, e := url.Parse(configuration["gateway_url"].(string))
		must(e)
		_, e = p.Guest(ctx, id, spec, []string{"curl", "--silent", "--show-error", "--connect-timeout", "10", "--max-time", "15", "--output", "/dev/null", "--head", "--resolve", gateway.Hostname() + ":" + gateway.Port() + ":" + spec.Relay.ListenAddress, gateway.String() + "/models"}, nil)
		must(e)
		t.Log("verified guest gateway TLS before requesting credentials")
	}
	credentialProfile := ""
	if configuration["harness"] == "claude" {
		credentialProfile = "claude-code"
	}
	lease, err := broker.Mint(ctx, LeaseRequest{RequestID: id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:], CredentialProfile: credentialProfile, ClientID: "hrx-native-acceptance", Purpose: "fresh-container-readiness", Models: []string{model}, TTLSeconds: 900, MaxBudgetUSD: 1, RPMLimit: 30, TPMLimit: 1000000, MaxParallelRequests: 2})
	if lease.ID != "" {
		defer func() {
			cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
			defer done()
			if e := broker.Revoke(cleanup, lease.ID); e != nil {
				t.Errorf("lease %s revocation failed: %v", lease.ID, e)
			} else {
				t.Logf("revoked lease %s", lease.ID)
			}
		}()
	}
	must(err)
	secret, err := lease.PrivateJSON()
	must(err)
	configuration["lease"] = json.RawMessage(secret)
	private, err = json.Marshal(configuration)
	must(err)
	first, err := p.StartAgent(ctx, id, spec, bytes.NewReader(private))
	must(err)
	t.Logf("native startup status=%s", first.Status)
	observer, err := os.ReadFile("assets/readiness_observe.py")
	must(err)
	deadline := time.Now().Add(2 * time.Minute)
	bound := false
	for time.Now().Before(deadline) {
		result, e := p.Guest(ctx, id, spec, []string{"python3", "-c", string(observer), identity, board, id}, nil)
		must(e)
		var observation struct {
			Terminated bool   `json:"terminated"`
			Bound      bool   `json:"bound"`
			Session    string `json:"session"`
		}
		must(json.Unmarshal([]byte(result.GuestOutput), &observation))
		if observation.Terminated {
			t.Fatal("native process exited; retained pane contains startup diagnostics")
		}
		if observation.Bound {
			bound = true
			t.Logf("native bridge bound session=%s", observation.Session)
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
	if !bound {
		t.Fatal("native binding observation deadline; guest retained, not classified as terminated")
	}
	again, err := p.StartAgent(ctx, id, spec, bytes.NewReader(private))
	must(err)
	if again.Session != first.Session {
		t.Fatal("repeat launch changed session")
	}
	verifyLiveReadiness(t, ctx, p, id, spec, identity, board)
	for _, op := range []string{"stop", "destroy"} {
		_, err = p.Reconcile(ctx, op, id, spec)
		must(err)
	}
	t.Log("startup, configured acceptance gates, and teardown finished")
}
