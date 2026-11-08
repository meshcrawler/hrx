package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexProviderOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HRX_CONFIG", "")
	t.Setenv("CODEX_HOME", dir)
	t.Setenv("HRX_CODEX_PROVIDER", "")
	config := []byte("model_provider = \"openai\"\n[model_providers.gateway]\nwire_api = \"responses\"\nbase_url = \"https://gateway.example/v1\"\nenv_key = \"FIXTURE_KEY\"\n")
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, config, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(); err == nil {
		t.Fatal("unconfigured provider accepted")
	}
	t.Setenv("HRX_CODEX_PROVIDER", "gateway")
	s, err := LoadSettings()
	if err != nil || s.ProviderName != "gateway" {
		t.Fatal("provider override failed", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(config) {
		t.Fatal("Codex configuration modified")
	}
	t.Setenv("HRX_CODEX_PROVIDER", "missing")
	if _, err := LoadSettings(); err == nil {
		t.Fatal("unknown provider accepted")
	}
}
