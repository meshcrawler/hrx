package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/meshcrawler/hrx/internal/backend"
)

func TestRemoteLifecycleResolvesConfiguredStateDirectory(t *testing.T) {
	for _, path := range []string{"state", "~/state"} {
		t.Run(path, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "observations"))
			configPath := filepath.Join(dir, "config.toml")
			t.Setenv("HRX_CONFIG", configPath)
			if err := os.WriteFile(configPath, []byte(fmt.Sprintf("[proxmox]\nstate_directory=%q\n", path)), 0600); err != nil {
				t.Fatal(err)
			}
			digest := strings.Repeat("a", 64)
			store := backend.StateStore{Directory: filepath.Join(dir, "state")}
			spec := backend.ContainerSpec{Template: "local:vztmpl/debian.tar.zst", Storage: "local-lvm", Bridge: "vmbr1", Cores: 2, MemoryMiB: 2048, DiskGiB: 8, LaunchDigest: digest}
			if _, err := store.ReserveConfigured("worker", "pve1", digest, spec); err != nil {
				t.Fatal(err)
			}
			// This validation happens after lookup and before any external action.
			err := runRemoteLifecycle(context.Background(), []string{"replace-lease", "--name=worker"}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "replace-lease requires --generation") {
				t.Fatalf("lifecycle did not find the saved deployment: %v", err)
			}
		})
	}
}
