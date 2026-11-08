package backend

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

//go:embed assets/bootstrap.py
var bootstrapHelper string

// Artifact pins an operator-selected local file. Path is never sent to the guest.
type Artifact struct {
	Name    string `json:"name" toml:"name"`
	Kind    string `json:"kind" toml:"kind"`
	Version string `json:"version" toml:"version"`
	SHA256  string `json:"sha256" toml:"sha256"`
	Size    int64  `json:"size" toml:"size"`
	Path    string `json:"-" toml:"path"`
}
type Bundle struct {
	Schema    int        `json:"schema" toml:"schema"`
	Harness   string     `json:"harness" toml:"harness"`
	Artifacts []Artifact `json:"artifacts" toml:"artifacts"`
}

var artifactName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,199}$`)

func (b Bundle) Validate() error {
	if b.Schema != 1 || (b.Harness != "codex" && b.Harness != "claude" && b.Harness != "opencode") {
		return errors.New("invalid runtime bundle schema or harness")
	}
	if len(b.Artifacts) < 3 || len(b.Artifacts) > 128 {
		return errors.New("runtime bundle requires HRX, harness and locked Pitput wheels")
	}
	seen := map[string]bool{}
	counts := map[string]int{}
	var total int64
	hasPitput := false
	for _, a := range b.Artifacts {
		decoded, e := hex.DecodeString(a.SHA256)
		if !artifactName.MatchString(a.Name) || a.Name == "manifest.json" || seen[a.Name] || a.Version == "" || len(a.Version) > 128 || strings.ToLower(a.SHA256) != a.SHA256 || e != nil || len(decoded) != 32 || a.Size <= 0 || a.Size > 512<<20 {
			return errors.New("invalid or duplicate pinned artifact")
		}
		if a.Kind != "hrx" && a.Kind != "harness" && a.Kind != "wheel" && a.Kind != "codex-resource" {
			return errors.New("unsupported artifact kind")
		}
		if a.Kind == "codex-resource" && (b.Harness != "codex" || (a.Name != "codex-code-mode-host" && a.Name != "bwrap" && a.Name != "rg")) {
			return errors.New("unsupported Codex companion artifact")
		}
		if a.Kind == "wheel" && !strings.HasSuffix(a.Name, ".whl") {
			return errors.New("wheel artifact requires wheel filename")
		}
		if a.Kind == "wheel" && strings.HasPrefix(a.Name, "pitput-") {
			hasPitput = true
		}
		counts[a.Kind]++
		seen[a.Name] = true
		total += a.Size
	}
	if b.Harness == "codex" && counts["codex-resource"] != 3 {
		return errors.New("Codex bundle requires pinned codex-code-mode-host, bwrap and rg companions")
	}
	if !hasPitput {
		return errors.New("runtime bundle requires a Pitput wheel")
	}

	if counts["hrx"] != 1 || counts["harness"] != 1 || counts["wheel"] < 1 || total > 1<<30 {
		return errors.New("invalid runtime bundle composition or size")
	}
	return nil
}

// Write verifies file contents before copying each artifact into the stream. The
// receiver independently verifies hashes before making the runtime available.
func (b Bundle) Write(output io.Writer) error {
	if err := b.Validate(); err != nil {
		return err
	}
	manifest, err := json.Marshal(b)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(output)
	if err = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(manifest))}); err != nil {
		return err
	}
	if _, err = tw.Write(manifest); err != nil {
		return err
	}
	for _, a := range b.Artifacts {
		err = func() error {
			f, err := os.Open(a.Path)
			if err != nil {
				return fmt.Errorf("artifact %s unavailable", a.Name)
			}
			defer f.Close()
			info, err := f.Stat()
			if err != nil || !info.Mode().IsRegular() || info.Size() != a.Size {
				return fmt.Errorf("artifact %s size/type mismatch", a.Name)
			}
			hash := sha256.New()
			if _, err = io.Copy(hash, io.LimitReader(f, a.Size+1)); err != nil {
				return err
			}
			if hex.EncodeToString(hash.Sum(nil)) != a.SHA256 {
				return fmt.Errorf("artifact %s digest mismatch", a.Name)
			}
			if _, err = f.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if err = tw.WriteHeader(&tar.Header{Name: a.Name, Mode: 0600, Size: a.Size}); err != nil {
				return err
			}
			_, err = io.CopyN(tw, f, a.Size)
			return err
		}()
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

func (p Proxmox) Bootstrap(ctx context.Context, id string, spec ContainerSpec, b Bundle) (Deployment, error) {
	if err := b.Validate(); err != nil {
		return Deployment{}, err
	}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() { err := b.Write(writer); writer.CloseWithError(err); done <- err }()
	result, err := p.Guest(ctx, id, spec, []string{"python3", "-c", bootstrapHelper}, reader)
	reader.Close() // Unblock the producer if SSH failed before consuming the bundle.
	writeErr := <-done
	if err != nil {
		return result, err
	}
	if writeErr != nil {
		return result, writeErr
	}
	return result, nil
}
