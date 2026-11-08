package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/meshcrawler/hrx/internal/backend"
	"github.com/meshcrawler/hrx/internal/monitor"
)

func remotePlanFixture(t *testing.T) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "monitor-state"))
	bundleDir := filepath.Join(dir, "bundle")
	if err := os.MkdirAll(filepath.Join(bundleDir, "wheels"), 0700); err != nil {
		t.Fatal(err)
	}
	b := backend.Bundle{Schema: 1, Harness: "opencode"}
	for i, name := range []string{"hrx", "opencode", "pitput-0.1-py3-none-any.whl"} {
		data := []byte("fixture " + name)
		path := filepath.Join(bundleDir, name)
		kind := []string{"hrx", "harness", "wheel"}[i]
		if kind == "wheel" {
			path = filepath.Join(bundleDir, "wheels", name)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		b.Artifacts = append(b.Artifacts, backend.Artifact{Name: name, Kind: kind, Version: "0.1", SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Size: int64(len(data))})
	}
	raw, _ := json.Marshal(b)
	if err := os.WriteFile(filepath.Join(bundleDir, "manifest.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(dir, "task.txt")
	if err := os.WriteFile(prompt, []byte("PRIVATE assigned task"), 0600); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`[profiles.worker]
harness="opencode"
model="payer/model"
[proxmox]
python="/controller/python"
gateway_url="https://gateway.example/v1"
controller_identity="host/controller/agent"
state_directory=%q
broker_url="https://broker.example"
broker_auth_command=["/controller/auth"]
lease_seconds=900
max_budget_usd=1
rpm_limit=30
tpm_limit=200000
max_parallel_requests=2
[proxmox.bundles]
opencode=%q
[proxmox.hosts.testhost]
template="local:vztmpl/debian.tar.zst"
storage="local-lvm"
bridge="vmbr1"
cores=2
memory_mib=2048
disk_gib=8
`, filepath.Join(dir, "state"), bundleDir)
	configPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRX_CONFIG", configPath)
	return dir, []string{"run", "--backend=proxmox", "--plan", "--host=testhost", "--profile=worker", "--agent-identity=host/worker/agent", "--pitput-board=test", "--prompt-file=" + prompt}
}

func TestRemotePlanHasNoEffectsAndBindsPrompt(t *testing.T) {
	dir, args := remotePlanFixture(t)
	var out bytes.Buffer
	if err := Run(context.Background(), args, &out, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "PRIVATE") {
		t.Fatal("plan exposed prompt")
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
		t.Fatal("plan created deployment state")
	}
	var first map[string]any
	if err := json.Unmarshal(out.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first["ready"] != false || first["backend"] != "proxmox" {
		t.Fatal("invalid plan")
	}
	if err := os.WriteFile(filepath.Join(dir, "task.txt"), []byte("changed task"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := Run(context.Background(), args, &out, &out); err != nil {
		t.Fatal(err)
	}
	var second map[string]any
	json.Unmarshal(out.Bytes(), &second)
	if first["fingerprint"] == second["fingerprint"] {
		t.Fatal("assigned prompt not bound")
	}
}

func TestRemotePlanRejectsModifiedArtifactsBeforeEffects(t *testing.T) {
	dir, args := remotePlanFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "bundle", "hrx"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(context.Background(), args, &out, &out); err == nil {
		t.Fatal("unverified artifact accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
		t.Fatal("invalid plan had effects")
	}
}

func TestExplicitLocalRunKeepsVersionBehavior(t *testing.T) {
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"run", "--backend=local", "--version"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "hrx") {
		t.Fatal("local behavior lost")
	}
	if err := Run(context.Background(), []string{"run", "--backend=other"}, &out, &out); err == nil {
		t.Fatal("unknown backend accepted")
	}
}

func TestRemotePlanRejectsGatewayRootBeforeAllocation(t *testing.T) {
	dir, args := remotePlanFixture(t)
	path := os.Getenv("HRX_CONFIG")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.ReplaceAll(string(data), "/v1\"", "\"")
	if changed == string(data) {
		t.Fatal("fixture has no provider path")
	}
	if err = os.WriteFile(path, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = Run(context.Background(), args, &out, &out); err == nil || !strings.Contains(err.Error(), "/v1") {
		t.Fatalf("wrong gateway validation: %v", err)
	}
	if _, err = os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
		t.Fatal("invalid gateway allocated state")
	}
}

func TestRemoteRoleResourcesAndFiltersAreBound(t *testing.T) {
	_, args := remotePlanFixture(t)
	read := func(argv []string) map[string]any {
		t.Helper()
		var out bytes.Buffer
		if err := Run(context.Background(), argv, &out, &out); err != nil {
			t.Fatal(err)
		}
		var plan map[string]any
		if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
			t.Fatal(err)
		}
		return plan
	}
	initial := read(args)
	changed := read(append(append([]string{}, args...), "--cores=4", "--memory-mib=4096", "--pitput-follow-thread=/b/test/topic"))
	if initial["fingerprint"] == changed["fingerprint"] {
		t.Fatal("role settings not bound")
	}
	container := changed["container"].(map[string]any)
	if container["cores"] != float64(4) || container["memory_mib"] != float64(4096) {
		t.Fatal("resource overrides lost")
	}
	var out bytes.Buffer
	if err := Run(context.Background(), append(args, "--pitput-follow-thread=/b/unjoined/topic"), &out, &out); err == nil {
		t.Fatal("followed unjoined board")
	}
}

func TestRemoteCodexCatalogRequiresExactUniqueRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{`{"models":[{"slug":"route"}]}`, true},
		{`{"models":[{"slug":"other"}]}`, false},
		{`{"models":[{"slug":"route"},{"slug":"route"}]}`, false},
		{`{"models":[]}`, false},
	} {
		if err := os.WriteFile(path, []byte(tc.raw), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := readRemoteCodexCatalog(path, "route")
		if (err == nil) != tc.valid {
			t.Fatalf("catalog validation: %v", err)
		}
	}
}

func TestRemoteEnrollmentFailureIsVisibleWithoutPromptContent(t *testing.T) {
	dir, args := remotePlanFixture(t)
	configPath := filepath.Join(dir, "config.toml")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte("/controller/python"), []byte(filepath.Join(dir, "missing-python")))
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	args = slices.DeleteFunc(args, func(arg string) bool { return arg == "--plan" })
	var out bytes.Buffer
	if err := Run(context.Background(), args, &out, &out); err == nil {
		t.Fatal("missing enrollment executable accepted")
	}
	observations, err := monitor.Default()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := observations.Snapshot(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	if rows[0].Remote == nil || rows[0].Remote.Stage != "enrolling" || !rows[0].Remote.Failed || rows[0].Status != "unknown" {
		t.Fatal(rows)
	}
	encoded, _ := json.Marshal(rows)
	if bytes.Contains(encoded, []byte("PRIVATE")) {
		t.Fatal("prompt leaked into dashboard")
	}
}
