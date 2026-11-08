package app

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/pelletier/go-toml/v2"
	"github.com/meshcrawler/hrx/internal/codexlaunch"
)

var errCodexProfileInstructions = errors.New("cannot read selected Codex profile developer instructions")

// Read only the selected profile's explicit instruction value. Native
// config/read, at thread creation, decides whether this layer actually wins.
func codexProfileInstructions(args, env []string, dir string) (codexlaunch.Optional[string], error) {
	profile := ""
	for _, token := range nativeArguments("codex", args) {
		if !token.option {
			continue
		}
		name, value, equal := strings.Cut(args[token.index], "=")
		// clap accepts an attached value for the short -p option.
		if strings.HasPrefix(name, "-p") && name != "-p" && !strings.HasPrefix(name, "--") {
			name, value, equal = "-p", strings.TrimPrefix(args[token.index], "-p"), true
		}
		if name != "--profile" && name != "-p" {
			continue
		}
		if !equal {
			if token.index+1 >= len(args) {
				return codexlaunch.Optional[string]{}, errCodexProfileInstructions
			}
			value = args[token.index+1]
		}
		if profile != "" || value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\\x00") {
			return codexlaunch.Optional[string]{}, errCodexProfileInstructions
		}
		profile = value
	}
	if profile == "" {
		return codexlaunch.Optional[string]{}, nil
	}
	home, userHome := "", ""
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "CODEX_HOME="); ok {
			home = value
		}
		if value, ok := strings.CutPrefix(entry, "HOME="); ok {
			userHome = value
		}
	}
	if home == "" {
		if userHome == "" {
			return codexlaunch.Optional[string]{}, errCodexProfileInstructions
		}
		home = filepath.Join(userHome, ".codex")
	}
	if !filepath.IsAbs(home) {
		home = filepath.Join(dir, home)
	}
	f, err := os.OpenFile(filepath.Join(home, profile+".config.toml"), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return codexlaunch.Optional[string]{}, nil
	}
	if err != nil {
		return codexlaunch.Optional[string]{}, errCodexProfileInstructions
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return codexlaunch.Optional[string]{}, errCodexProfileInstructions
	}
	raw, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 {
		return codexlaunch.Optional[string]{}, errCodexProfileInstructions
	}
	return parseCodexProfileInstructions(raw)
}

func parseCodexProfileInstructions(raw []byte) (codexlaunch.Optional[string], error) {
	var fields map[string]any
	if len(raw) > 4<<20 || toml.Unmarshal(raw, &fields) != nil {
		return codexlaunch.Optional[string]{}, errCodexProfileInstructions
	}
	value, present := fields["developer_instructions"]
	if !present {
		return codexlaunch.Optional[string]{}, nil
	}
	text, ok := value.(string)
	if !ok {
		return codexlaunch.Optional[string]{}, errCodexProfileInstructions
	}
	return codexlaunch.Optional[string]{Present: true, Value: text}, nil
}
