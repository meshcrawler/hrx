package backend

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestReservationConcurrentAndRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deployments")
	fingerprint := strings.Repeat("a", 64)
	var wg sync.WaitGroup
	ids := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := (StateStore{Directory: dir}).Reserve("librarian", "pve1", fingerprint)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- r.DeploymentID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("duplicate allocations: %v", seen)
	}
	r, err := (StateStore{Directory: dir}).Reserve("librarian", "pve1", fingerprint)
	if err != nil || !seen[r.DeploymentID] {
		t.Fatalf("restart changed identity: %v", err)
	}
	if _, err = (StateStore{Directory: dir}).Reserve("librarian", "pve2", fingerprint); err == nil {
		t.Fatal("changed host accepted")
	}
	if _, err = (StateStore{Directory: dir}).Reserve("librarian", "pve1", strings.Repeat("b", 64)); err == nil {
		t.Fatal("changed specification accepted")
	}
}

func TestReservationCorruptionIsNotReplaced(t *testing.T) {
	store := StateStore{Directory: filepath.Join(t.TempDir(), "state")}
	if _, err := store.Reserve("worker", "host", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(store.Directory, "*.json"))
	if err := os.WriteFile(files[0], []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reserve("worker", "host", strings.Repeat("a", 64)); err == nil {
		t.Fatal("corrupt reservation replaced")
	}
}

func TestReservationRejectsSymlinkDirectory(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := (StateStore{Directory: link}).Reserve("worker", "host", strings.Repeat("a", 64)); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestConfiguredReservationLookup(t *testing.T) {
	store := StateStore{Directory: filepath.Join(t.TempDir(), "state")}
	digest := strings.Repeat("a", 64)
	spec := ContainerSpec{Template: "local:vztmpl/debian.tar.zst", Storage: "local-lvm", Bridge: "vmbr1", Cores: 2, MemoryMiB: 2048, DiskGiB: 8, Tags: []string{"experiment"}, LaunchDigest: digest}
	original, err := store.ReserveConfigured("worker", "pve1", digest, spec)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Lookup("worker")
	if err != nil {
		t.Fatal(err)
	}
	if saved.DeploymentID != original.DeploymentID || saved.Container.Bridge != "vmbr1" {
		t.Fatal("lost ownership binding")
	}
	spec.Bridge = "vmbr2"
	if _, err = store.ReserveConfigured("worker", "pve1", digest, spec); err == nil {
		t.Fatal("accepted mutated specification")
	}
	if _, err = store.Lookup("missing"); !os.IsNotExist(err) {
		t.Fatalf("missing lookup: %v", err)
	}
	files, _ := filepath.Glob(filepath.Join(store.Directory, "*.json"))
	if len(files) != 1 {
		t.Fatal("lookup allocated state")
	}
	if err = os.Chmod(files[0], 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Lookup("worker"); err == nil {
		t.Fatal("accepted public state")
	}
}

func TestLookupRejectsLegacyReservation(t *testing.T) {
	store := StateStore{Directory: filepath.Join(t.TempDir(), "state")}
	if _, err := store.Reserve("worker", "pve1", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Lookup("worker"); err == nil {
		t.Fatal("invented missing specification")
	}
}
