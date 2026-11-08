package backend

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in real infrastructure acceptance. Routine CI never provisions a guest.
// Failures retain the guest and print its deployment ID for explicit recovery.
func TestProxmoxFreshContainerLifecycle(t *testing.T) {
	host := os.Getenv("HRX_PROXMOX_LIVE_HOST")
	if host == "" {
		t.Skip("requires explicit Proxmox live host and template")
	}
	template := os.Getenv("HRX_PROXMOX_LIVE_TEMPLATE")
	if template == "" {
		t.Fatal("HRX_PROXMOX_LIVE_TEMPLATE required")
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("%x", raw)
	if saved := os.Getenv("HRX_PROXMOX_LIVE_DEPLOYMENT"); saved != "" {
		id = saved
	}
	t.Logf("deployment=%s host=%s; preserve on failure", id, host)
	spec := ContainerSpec{Template: template, Storage: "local-lvm", Bridge: "vmbr1", Cores: 1, MemoryMiB: 512, DiskGiB: 4, Tags: []string{"hrx-bootstrap-acceptance"}, LaunchDigest: fmt.Sprintf("%064x", 1)}
	p := Proxmox{Host: host, Transport: SSH{}}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	first, err := p.Reconcile(ctx, "ensure", id, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("allocated vmid=%d node=%s", first.VMID, first.Node)
	if directory := os.Getenv("HRX_PROXMOX_LIVE_BUNDLE"); directory != "" {
		data, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		var bundle Bundle
		if err = json.Unmarshal(data, &bundle); err != nil {
			t.Fatal(err)
		}
		for i := range bundle.Artifacts {
			a := &bundle.Artifacts[i]
			a.Path = filepath.Join(directory, a.Name)
			if a.Kind == "wheel" {
				a.Path = filepath.Join(directory, "wheels", a.Name)
			}
		}
		if _, err = p.Reconcile(ctx, "start", id, spec); err != nil {
			t.Fatal(err)
		}
		for attempt := 0; attempt < 2; attempt++ {
			if _, err = p.Bootstrap(ctx, id, spec, bundle); err != nil {
				t.Fatalf("bootstrap attempt %d: %v; deployment retained", attempt, err)
			}
			t.Logf("verified runtime installation attempt=%d harness=%s", attempt, bundle.Harness)
		}
	}
	for _, op := range []string{"ensure", "inspect", "start", "start", "stop", "stop", "destroy", "destroy", "inspect"} {
		d, err := p.Reconcile(ctx, op, id, spec)
		if err != nil {
			t.Fatalf("%s: %v (deployment %s retained)", op, err, id)
		}
		if d.VMID != first.VMID {
			t.Fatalf("%s changed allocation", op)
		}
		t.Logf("%s: phase=%s status=%s", op, d.Phase, d.ContainerStatus)
	}
}
