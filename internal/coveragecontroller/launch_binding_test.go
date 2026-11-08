package coveragecontroller

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/meshcrawler/hrx/internal/codexlaunch"
)

// Filesystem binding cases run on GitLab with the full suite.
func TestLaunchRecordWorkingDirectoryBinding(t *testing.T) {
	root := t.TempDir()
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "project")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	h := &gateway{expectedWorkingDirectory: canonical, b: &bootstrap{native: codexlaunch.Native{Cwd: alias}}}
	if err := h.validateLaunchBinding(); err != nil {
		t.Fatal("canonical project rejected", err)
	}
	h.b.native.Cwd = t.TempDir()
	if h.validateLaunchBinding() == nil {
		t.Fatal("different project accepted")
	}
	h.b.native.Cwd = filepath.Join(root, "missing")
	if h.validateLaunchBinding() == nil {
		t.Fatal("unresolved project accepted")
	}
}
