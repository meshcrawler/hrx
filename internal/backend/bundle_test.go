package backend

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func testBundle(t *testing.T) Bundle {
	t.Helper()
	root := t.TempDir()
	b := Bundle{Schema: 1, Harness: "opencode"}
	for _, row := range [][2]string{{"hrx", "hrx"}, {"opencode", "harness"}, {"pitput-0.1-py3-none-any.whl", "wheel"}} {
		data := []byte(row[0])
		path := filepath.Join(root, row[0])
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		b.Artifacts = append(b.Artifacts, Artifact{Name: row[0], Kind: row[1], Version: "0.1", SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Size: int64(len(data)), Path: path})
	}
	return b
}

func TestBundleWriteAndChangedFile(t *testing.T) {
	b := testBundle(t)
	var buf bytes.Buffer
	if err := b.Write(&buf); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(&buf)
	count := 0
	for {
		h, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if count == 0 && h.Name != "manifest.json" {
			t.Fatal("missing manifest")
		}
		count++
	}
	if count != 4 {
		t.Fatalf("member count %d", count)
	}
	if err := os.WriteFile(b.Artifacts[0].Path, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.Write(io.Discard); err == nil {
		t.Fatal("accepted modified artifact")
	}
}

func TestBundleRejectsUnsafeNamesAndIncompletePins(t *testing.T) {
	for _, name := range []string{"../escape", "/etc/passwd", "manifest.json", "bad/name"} {
		b := testBundle(t)
		b.Artifacts[0].Name = name
		if b.Validate() == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	b := testBundle(t)
	b.Artifacts[0].SHA256 = ""
	if b.Validate() == nil {
		t.Fatal("accepted unpinned runtime")
	}
}

func TestCodexBundleRequiresCompletePinnedCompanions(t *testing.T) {
	b := testBundle(t)
	b.Harness = "codex"
	if b.Validate() == nil {
		t.Fatal("accepted Codex without companions")
	}
	for _, name := range []string{"codex-code-mode-host", "bwrap", "rg"} {
		a := b.Artifacts[0]
		a.Name = name
		a.Kind = "codex-resource"
		b.Artifacts = append(b.Artifacts, a)
	}
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	b.Harness = "opencode"
	if b.Validate() == nil {
		t.Fatal("accepted Codex companions for OpenCode")
	}
	b.Harness = "codex"
	b.Artifacts[len(b.Artifacts)-1].Name = "unknown"
	if b.Validate() == nil {
		t.Fatal("accepted arbitrary companion target")
	}
}
