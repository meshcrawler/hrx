package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type userConfig struct {
	Proxmox  proxmoxConfiguration `toml:"proxmox"`
	Profiles map[string]any       `toml:"profiles"`
	Wave     struct {
		Enabled    *bool             `toml:"enabled"`
		RoleLabels map[string]string `toml:"role_labels"`
	} `toml:"wave"`
	Hub struct {
		Enabled         bool   `toml:"enabled"`
		URL             string `toml:"url"`
		TokenFile       string `toml:"token_file"`
		ViewerTokenFile string `toml:"viewer_token_file"`
	} `toml:"hub"`
	Auto struct {
		ProviderHarness string `toml:"provider_harness"`
		Model           string `toml:"model"`
	} `toml:"auto"`
	Permissions struct {
		Yolo bool `toml:"yolo"`
	} `toml:"permissions"`
	Codex struct {
		ProviderName string `toml:"provider_name"`
	} `toml:"codex"`
	Catalog struct {
		// Short compact-catalog labels keyed by "provider/account".
		Labels map[string]string `toml:"labels"`
	} `toml:"catalog"`
	Agents struct {
		// Opt-in agent workspace convention; no guidance is injected when empty.
		WorkspaceRoot string `toml:"workspace_root"`
	} `toml:"agents"`
	Plugins struct {
		Broker struct {
			URL                 string `toml:"url"`
			ClientTokensDir     string `toml:"client_tokens_dir"`
			EnrollmentTokenFile string `toml:"enrollment_token_file"`
		} `toml:"gitlab-service-account-broker"`
	} `toml:"plugins"`
}

func applyLaunchDefaults(o LaunchOptions) (LaunchOptions, error) {
	config, _, err := loadUserConfig()
	if err != nil {
		return o, err
	}
	if !o.yoloSet {
		o.Yolo = config.Permissions.Yolo
	}
	return o, nil
}

func loadUserConfig() (userConfig, string, error) {
	var config userConfig
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return config, "", err
		}
		base = filepath.Join(home, ".config")
	}
	path := os.Getenv("HRX_CONFIG")
	if path == "" {
		path = filepath.Join(base, "hrx", "config.toml")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return config, "", errors.New("invalid HRX configuration path")
	}
	dir := filepath.Dir(path)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) && os.Getenv("HRX_CONFIG") == "" {
		return config, dir, nil
	}
	if err != nil {
		return config, dir, errors.New("cannot read HRX configuration")
	}
	if len(b) > 1024*1024 || toml.Unmarshal(b, &config) != nil {
		return config, dir, errors.New("invalid HRX TOML configuration")
	}
	return config, dir, nil
}

func brokerConfiguration(identity string) (string, string, error) {
	config, dir, err := loadUserConfig()
	if err != nil {
		return "", "", err
	}
	origin := config.Plugins.Broker.URL
	if override := os.Getenv("HRX_GITLAB_BROKER_URL"); override != "" {
		origin = override
	}
	if path := os.Getenv("HRX_GITLAB_BROKER_TOKEN_FILE"); path != "" {
		return origin, path, nil
	}
	// Use the full identity, not just its role, to avoid collisions across hosts.
	if !brokerIdentity.MatchString(identity) || strings.HasPrefix(identity, "/") {
		return "", "", errors.New("invalid broker identity for credential lookup")
	}
	for _, part := range strings.Split(identity, "/") {
		if part == "" || part == "." || part == ".." {
			return "", "", errors.New("invalid broker identity for credential lookup")
		}
	}
	root := config.Plugins.Broker.ClientTokensDir
	if root == "" {
		root = "client-tokens"
	}
	if strings.HasPrefix(root, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		root = filepath.Join(home, strings.TrimPrefix(root, "~/"))
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(dir, root)
	}
	path := filepath.Join(root, filepath.FromSlash(identity), "broker.token")
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if !os.IsNotExist(err) && (err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0) {
			return "", "", errors.New("agent broker credential directory is missing or not private (0700)")
		}
		if parent == filepath.Clean(root) {
			break
		}
	}
	return origin, path, nil
}
