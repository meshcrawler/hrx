package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type Auth struct {
	Command   string   `toml:"command" json:"command"`
	Args      []string `toml:"args" json:"args"`
	TimeoutMS int      `toml:"timeout_ms" json:"timeout_ms"`
}

type Provider struct {
	ProfileCredential string `toml:"-" json:"profile_credential,omitempty"`
	CredentialFile    string `toml:"credential_file" json:"credential_file,omitempty"`
	BaseURL           string `toml:"base_url" json:"base_url"`
	WireAPI           string `toml:"wire_api" json:"wire_api"`
	EnvKey            string `toml:"env_key" json:"env_key"`
	Auth              Auth   `toml:"auth" json:"auth"`
}

type Settings struct {
	Harness                                                                string
	TemplatePath, GeneratedCatalogPath                                     string
	ConfigPath, ProviderName, DefaultModel, CatalogURL, CachePath, Binding string
	Provider                                                               Provider
}

func LoadSettings(profileCredential ...string) (Settings, error) {
	var s Settings
	home, err := os.UserHomeDir()
	if err != nil {
		return s, err
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	s.ConfigPath = filepath.Join(codexHome, "config.toml")
	data, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		return s, errors.New("cannot read Codex config.toml; set CODEX_HOME if needed")
	}
	var config struct {
		ModelCatalog string              `toml:"model_catalog_json"`
		Model        string              `toml:"model"`
		Provider     string              `toml:"model_provider"`
		Providers    map[string]Provider `toml:"model_providers"`
	}
	if err = toml.Unmarshal(data, &config); err != nil {
		return s, errors.New("invalid Codex TOML configuration")
	}
	s.ProviderName = config.Provider
	local, _, err := loadUserConfig()
	if err != nil {
		return s, err
	}
	if local.Codex.ProviderName != "" {
		s.ProviderName = local.Codex.ProviderName
	}
	if provider := os.Getenv("HRX_CODEX_PROVIDER"); provider != "" {
		s.ProviderName = provider
	}
	s.TemplatePath = config.ModelCatalog
	if s.TemplatePath == "" {
		s.TemplatePath = filepath.Join(codexHome, "models_cache.json")
	}
	if !filepath.IsAbs(s.TemplatePath) {
		s.TemplatePath = filepath.Join(codexHome, s.TemplatePath)
	}
	s.DefaultModel = config.Model
	if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(s.ProviderName) {
		return s, errors.New("configure a named LiteLLM provider in Codex first")
	}
	var ok bool
	s.Provider, ok = config.Providers[s.ProviderName]
	if !ok || s.Provider.WireAPI != "responses" {
		return s, errors.New("Codex provider must use the Responses API; set HRX_CODEX_PROVIDER to a configured LiteLLM provider")
	}
	u, err := url.Parse(s.Provider.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return s, errors.New("Codex provider needs an HTTPS base_url without credentials, query or fragment")
	}
	s.Provider.BaseURL = strings.TrimRight(s.Provider.BaseURL, "/")
	s.CatalogURL = u.Scheme + "://" + u.Host + "/dashboard/models.json"
	if len(profileCredential) > 0 && profileCredential[0] != "" {
		s.Provider.Auth = Auth{}
		s.Provider.CredentialFile = ""
		s.Provider.EnvKey = "HRX_PROFILE_GATEWAY_KEY"
		s.Provider.ProfileCredential = profileCredential[0]
	}
	if s.Provider.EnvKey == "" && s.Provider.Auth.Command == "" {
		return s, errors.New("provider needs env_key or an auth command; hrx does not manage credentials")
	}
	if s.Provider.EnvKey != "" && s.Provider.Auth.Command != "" {
		return s, errors.New("ambiguous provider credentials: both env_key and auth command are set")
	}
	cacheDir := os.Getenv("XDG_CACHE_HOME")
	if cacheDir == "" {
		cacheDir = filepath.Join(home, ".cache")
	}
	binding, _ := json.Marshal([]any{s.ConfigPath, s.ProviderName, s.Provider, s.CatalogURL})
	hash := sha256.Sum256(binding)
	s.Binding = hex.EncodeToString(hash[:])
	s.CachePath = filepath.Join(cacheDir, "hrx", s.Binding+".json")
	return s, nil
}
