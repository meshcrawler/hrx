package backend

import (
	"strings"
	"testing"
)

func TestAttachUsesOwnedGuestAndExplicitTerminal(t *testing.T) {
	spec := ContainerSpec{Template: "local:vztmpl/debian.tar.zst", Storage: "local-lvm", Bridge: "vmbr1", Cores: 2, MemoryMiB: 2048, DiskGiB: 8, LaunchDigest: strings.Repeat("b", 64)}
	p := Proxmox{Host: "pve1"}
	args, err := p.AttachCommand(strings.Repeat("a", 32), spec)
	if err != nil {
		t.Fatal(err)
	}
	if args[0] != "-tt" || args[len(args)-2] != "pve1" {
		t.Fatal("missing explicit terminal/host")
	}
	command := args[len(args)-1]
	for _, required := range []string{"guest=attach", "module.Host", "attach-session", "runuser", "hrx-" + strings.Repeat("a", 32)} {
		if !strings.Contains(command, required) {
			t.Fatalf("missing %s", required)
		}
	}
	p.Host = "host; echo injected"
	if _, err = p.AttachCommand(strings.Repeat("a", 32), spec); err == nil {
		t.Fatal("accepted injected host")
	}
}
