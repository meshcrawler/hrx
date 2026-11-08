package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/meshcrawler/hrx/internal/catalog"
)

type harnessConfig struct {
	Model    string   `toml:"model"`
	Provider Provider `toml:"provider"`
}

func loadHarnessSettings(harness string, profileCredential ...string) (Settings, error) {
	if harness == "codex" {
		return LoadSettings(profileCredential...)
	}
	s := Settings{Harness: harness, ProviderName: "litellm"}
	if harness != "claude" && harness != "opencode" {
		return s, errors.New("unknown harness")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return s, err
	}
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		configDir = filepath.Join(home, ".config")
	}
	s.ConfigPath = filepath.Join(configDir, "hrx", "config.toml")
	if path := os.Getenv("HRX_CONFIG"); path != "" {
		s.ConfigPath = path
	}
	var config map[string]any
	data, err := os.ReadFile(s.ConfigPath)
	if err == nil {
		if toml.Unmarshal(data, &config) != nil {
			return s, errors.New("invalid hrx TOML configuration")
		}
	} else if !os.IsNotExist(err) || os.Getenv("HRX_CONFIG") != "" {
		return s, errors.New("cannot read hrx configuration")
	}
	if raw, ok := config[harness]; ok {
		// Decode only this harness table. Profile names such as "model" or
		// "provider" must not be interpreted as fields of a harnessConfig.
		encoded, err := toml.Marshal(raw)
		var entry harnessConfig
		if err != nil || toml.Unmarshal(encoded, &entry) != nil {
			return s, errors.New("invalid harness configuration table")
		}
		s.Provider, s.DefaultModel = entry.Provider, entry.Model
	} else if harness == "claude" {
		// Claude Code has no gateway provider of its own to reuse.
		return s, errors.New("configure [claude.provider] in the HRX configuration: base_url and exactly one credential handle (env_key, credential_file or auth command)")
	} else {
		path := filepath.Join(configDir, "opencode", "opencode.json")
		baseURL, _, err := readOpenCodeProvider(path)
		if err != nil {
			return s, err
		}
		s.Provider = Provider{BaseURL: baseURL, CredentialFile: path}
	}
	u, err := url.Parse(s.Provider.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimRight(u.Path, "/") != "/v1" {
		return s, errors.New("harness provider needs an HTTPS /v1 base_url without credentials, query or fragment")
	}
	s.Provider.BaseURL = strings.TrimRight(s.Provider.BaseURL, "/")
	s.CatalogURL = u.Scheme + "://" + u.Host + "/dashboard/models.json"
	if len(profileCredential) > 0 && profileCredential[0] != "" {
		s.Provider.Auth = Auth{}
		s.Provider.CredentialFile = ""
		s.Provider.EnvKey = "HRX_PROFILE_GATEWAY_KEY"
		s.Provider.ProfileCredential = profileCredential[0]
	}
	count := 0
	for _, handle := range []string{s.Provider.EnvKey, s.Provider.Auth.Command, s.Provider.CredentialFile} {
		if handle != "" {
			count++
		}
	}
	if count != 1 {
		return s, errors.New("configure exactly one credential handle: env_key, auth command, or credential_file")
	}
	if s.Provider.CredentialFile != "" && !filepath.IsAbs(s.Provider.CredentialFile) {
		return s, errors.New("credential_file must be an absolute path")
	}
	cacheDir := os.Getenv("XDG_CACHE_HOME")
	if cacheDir == "" {
		cacheDir = filepath.Join(home, ".cache")
	}
	binding, _ := json.Marshal([]any{harness, s.ConfigPath, s.ProviderName, s.Provider, s.CatalogURL})
	hash := sha256.Sum256(binding)
	s.Binding = hex.EncodeToString(hash[:])
	s.CachePath = filepath.Join(cacheDir, "hrx", s.Binding+".json")
	return s, nil
}

func readOpenCodeProvider(path string) (string, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", errors.New("cannot read existing OpenCode configuration")
	}
	var config struct {
		Provider map[string]struct {
			Options struct {
				BaseURL string `json:"baseURL"`
				APIKey  string `json:"apiKey"`
			} `json:"options"`
		} `json:"provider"`
	}
	if json.Unmarshal(data, &config) != nil {
		return "", "", errors.New("existing OpenCode configuration must be valid JSON")
	}
	p, ok := config.Provider["litellm"]
	if !ok || p.Options.BaseURL == "" || p.Options.APIKey == "" {
		return "", "", errors.New("configure an existing OpenCode litellm provider or an hrx credential handle first")
	}
	return p.Options.BaseURL, p.Options.APIKey, nil
}

func openCodeCredential(path string) (string, error) {
	_, key, err := readOpenCodeProvider(path)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(key, "{env:") && strings.HasSuffix(key, "}") {
		return os.Getenv(key[5 : len(key)-1]), nil
	}
	if strings.HasPrefix(key, "{file:") && strings.HasSuffix(key, "}") {
		keyPath := key[6 : len(key)-1]
		if !filepath.IsAbs(keyPath) {
			return "", errors.New("OpenCode credential file reference must be absolute")
		}
		data, err := os.ReadFile(keyPath)
		if err != nil {
			return "", errors.New("cannot read OpenCode credential reference")
		}
		return string(data), nil
	}
	if strings.ContainsAny(key, "{}") {
		return "", errors.New("unsupported OpenCode credential interpolation")
	}
	return key, nil
}

func childEnvironment(base []string, overrides map[string]string, remove []string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if _, ok := overrides[name]; ok || slices.Contains(remove, name) {
			continue
		}
		result = append(result, entry)
	}
	for name, value := range overrides {
		result = append(result, name+"="+value)
	}
	return result
}

func checkClaudeRoutingSettings(managed string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configDir := managed
	if configDir == "" {
		configDir = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	if configDir == "" {
		configDir = filepath.Join(home, ".claude")
	}
	paths := []string{filepath.Join(configDir, "settings.json")}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	for {
		paths = append(paths, filepath.Join(dir, ".claude", "settings.json"), filepath.Join(dir, ".claude", "settings.local.json"))
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return errors.New("cannot check Claude settings for authentication overrides")
		}
		var config struct {
			Env          map[string]string `json:"env"`
			APIKeyHelper string            `json:"apiKeyHelper"`
		}
		if json.Unmarshal(data, &config) != nil {
			return errors.New("cannot validate Claude settings JSON")
		}
		if config.APIKeyHelper != "" {
			return errors.New("Claude apiKeyHelper conflicts with subscription authentication; use a separate CLAUDE_CONFIG_DIR")
		}
		for name := range config.Env {
			if strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "CLAUDE_CODE_USE_") || name == "CLAUDE_CODE_OAUTH_TOKEN" || name == "CLAUDE_CODE_SUBAGENT_MODEL" {
				return errors.New("Claude settings contain routing/auth environment overrides; remove them or use a separate CLAUDE_CONFIG_DIR")
			}
		}
	}
	return nil
}

func buildOtherLaunch(s Settings, snapshot catalog.Snapshot, o LaunchOptions, key string) (Launch, error) {
	l := Launch{Executable: s.Harness, Provider: s.ProviderName, BaseURL: s.Provider.BaseURL, Passthrough: o.Extra}
	extra, err := sessionArguments(s.Harness, o)
	if err != nil {
		return l, err
	}
	l.Yolo = o.Yolo
	l.Resume, l.Continue = o.Resume, o.Continue
	if o.Model == "" {
		o.Model = s.DefaultModel
	}
	if o.Model == "" {
		return l, errors.New("select --model from hrx models --harness " + s.Harness)
	}
	m, err := snapshot.Resolve(o.Model, s.Harness)
	if err != nil {
		return l, err
	}
	if err := validateRestoredRoute(o, m.Route); err != nil {
		return l, err
	}
	cap := m.Harnesses[s.Harness]
	if o.Reasoning == "" {
		o.Reasoning = cap.DefaultReasoning
	}
	if o.Reasoning != "" && !slices.Contains(cap.Reasoning, o.Reasoning) {
		return l, errors.New("reasoning level is not declared for this model and harness")
	}
	l.Identity, l.Model, l.Reasoning = m.ID, m.Route, o.Reasoning
	if s.Harness == "claude" {
		l.Args = []string{"--model", m.Route}
		if o.Reasoning != "" {
			l.Args = append(l.Args, "--effort", o.Reasoning)
		}
		l.Args = append(l.Args, extra...)
		env := map[string]string{
			"ANTHROPIC_BASE_URL":                     strings.TrimSuffix(s.Provider.BaseURL, "/v1"),
			"ANTHROPIC_MODEL":                        m.Route,
			"ANTHROPIC_DEFAULT_SONNET_MODEL":         m.Route,
			"ANTHROPIC_DEFAULT_OPUS_MODEL":           m.Route,
			"ANTHROPIC_DEFAULT_HAIKU_MODEL":          m.Route,
			"CLAUDE_CODE_SUBAGENT_MODEL":             m.Route,
			"ANTHROPIC_CUSTOM_HEADERS":               "x-litellm-api-key: Bearer " + key,
			"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS": "1",
		}
		if o.ClaudeConfigDir != "" {
			env["CLAUDE_CONFIG_DIR"] = o.ClaudeConfigDir
		}
		// Custom headers alone do not satisfy Claude's native login check.
		// Keep explicitly configured dual OAuth auth; clean homes use the
		// selected gateway credential instead of requiring a personal login.
		if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == "" {
			env["ANTHROPIC_AUTH_TOKEN"] = key
		}
		l.Env = childEnvironment(os.Environ(), env, []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"})
		return l, nil
	}
	models := map[string]any{}
	for _, model := range snapshot.Models {
		cap, ok := model.Harnesses["opencode"]
		if !ok {
			continue
		}
		entry := map[string]any{"name": model.ID}
		if model.Context > 0 && model.Output > 0 {
			entry["limit"] = map[string]int{"context": model.Context, "output": model.Output}
		}
		variants := map[string]any{}
		for _, effort := range cap.Reasoning {
			variants[effort] = map[string]string{"reasoningEffort": effort}
		}
		entry["variants"] = variants
		if model.Route == m.Route && o.Reasoning != "" {
			entry["options"] = map[string]string{"reasoningEffort": o.Reasoning}
		}
		models[model.Route] = entry
	}
	selected := "hrx/" + m.Route
	config := map[string]any{
		"model": selected, "small_model": selected, "enabled_providers": []string{"hrx"},
		"provider": map[string]any{"hrx": map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "hrx LiteLLM", "options": map[string]string{"baseURL": s.Provider.BaseURL, "apiKey": "{env:HRX_GATEWAY_KEY}"}, "models": models}},
	}
	agents := map[string]any{}
	for _, name := range []string{"build", "plan", "general", "explore", "compaction", "title", "summary"} {
		agents[name] = map[string]string{"model": selected}
	}
	config["agent"] = agents
	data, err := json.Marshal(config)
	if err != nil {
		return l, errors.New("cannot build OpenCode runtime configuration")
	}
	l.Env = childEnvironment(os.Environ(), map[string]string{"OPENCODE_CONFIG_CONTENT": string(data), "HRX_GATEWAY_KEY": key}, nil)
	// The installed CLI accepts --model on run, not on the interactive root.
	// Interactive sessions use the model already pinned in runtime config.
	l.Args = extra
	if openCodeRunPosition(extra) >= 0 {
		l.Args = insertOpenCodeFlags(extra, "--model", selected)
	}
	return l, nil
}

// Existing Codex commands retain their defaults and cache binding.
func runOtherHarness(ctx context.Context, args []string, out, errOut io.Writer, latest bool) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	harness := ""
	if args[0] == "claude" || args[0] == "opencode" {
		harness = args[0]
	}
	if args[0] == "_complete" && len(args) >= 3 && (args[2] == "claude" || args[2] == "opencode" || args[2] == "codex") {
		harness = args[2]
	}
	if args[0] == "models" || args[0] == "refresh" {
		clean := []string{args[0]}
		for i := 1; i < len(args); i++ {
			if args[i] == "--harness" {
				i++
				if i == len(args) || harness != "" {
					return true, errors.New("provide exactly one --harness")
				}
				harness = args[i]
			} else {
				clean = append(clean, args[i])
			}
		}
		if harness == "codex" {
			if latest {
				clean = append(clean, "--latest")
			}
			return true, Run(ctx, clean, out, errOut)
		}
		args = clean
	}
	if harness == "" {
		return false, nil
	}
	if len(args) == 2 && args[1] == "--help" {
		_, err := fmt.Fprint(out, Help)
		return true, err
	}
	var opts LaunchOptions
	var s Settings
	var err error
	if args[0] == harness {
		opts, err = prepareLaunchOptions(harness, args[1:])
		if err != nil {
			return true, err
		}
		s, err = loadHarnessSettings(harness, opts.CredentialFile)
	} else {
		s, err = loadHarnessSettings(harness)
	}
	if err != nil {
		if args[0] == "_complete" {
			return true, nil
		}
		return true, err
	}
	if args[0] == "_complete" {
		snapshot, err := catalog.Read(s.CachePath)
		if err != nil || snapshot.Validate(s.Binding, time.Now(), false) != nil {
			return true, nil
		}
		if latest {
			snapshot = snapshot.Latest(harness)
		}
		if args[1] == "models" && len(args) == 3 {
			for _, name := range snapshot.Names(harness) {
				fmt.Fprintln(out, name)
			}
		}
		if args[1] == "model-descriptions" && len(args) == 3 {
			completeModelDescriptions(out, snapshot, harness)
		}
		if (args[1] == "reasoning" || args[1] == "reasoning-descriptions") && len(args) == 4 {
			name := args[3]
			if name == "" {
				name = s.DefaultModel
			}
			if args[1] == "reasoning-descriptions" {
				completeReasoningDescriptions(out, snapshot, harness, name)
				return true, nil
			}
			if m, err := snapshot.Resolve(name, harness); err == nil {
				for _, level := range m.Harnesses[harness].Reasoning {
					fmt.Fprintln(out, level)
				}
			}
		}
		return true, nil
	}
	if args[0] == harness {
		if harness == "claude" {
			if err := ensureClaudeConfig(opts, time.Now()); err != nil {
				return true, err
			}
			if err := checkClaudeRoutingSettings(opts.ClaudeConfigDir); err != nil {
				return true, err
			}
		}
		snapshot, key, err := refreshWithCredential(ctx, s)
		if err != nil {
			return true, fmt.Errorf("launch validation failed: %w", err)
		}
		if err := validateProfileRoute(s, snapshot, opts); err != nil {
			return true, err
		}
		launch, err := buildOtherLaunch(s, snapshot, opts, key)
		if err != nil {
			return true, err
		}
		attachProfile(opts, &launch)
		if launch.Profile != nil {
			fmt.Fprintf(errOut, "hrx: profile=%s route=%s\n", launch.Profile.Name, launch.Model)
		}
		if err := prepareYesod(ctx, s, opts, &launch, errOut); err != nil {
			return true, err
		}
		if err := prepareSessionCapture(ctx, opts, &launch, errOut); err != nil {
			return true, err
		}
		if opts.DryRun {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return true, enc.Encode(launch)
		}
		fmt.Fprintf(errOut, "hrx: %s -> %s; harness=%s; reasoning=%s\n", launch.Identity, launch.Model, harness, launch.Reasoning)
		return true, execHarness(launch)
	}
	refresh, asJSON, stale := args[0] == "refresh", false, false
	for _, arg := range args[1:] {
		switch arg {
		case "--refresh":
			refresh = true
		case "--json":
			asJSON = true
		case "--stale":
			stale = true
		default:
			return true, errors.New("unknown catalog option")
		}
	}
	var snapshot catalog.Snapshot
	if refresh {
		snapshot, err = Refresh(ctx, s)
	} else {
		snapshot, err = catalog.Read(s.CachePath)
	}
	if err != nil {
		return true, err
	}
	if err = snapshot.Validate(s.Binding, time.Now(), stale); err != nil {
		return true, err
	}
	if latest {
		snapshot = snapshot.Latest(harness)
	}
	if asJSON {
		return true, json.NewEncoder(out).Encode(snapshot)
	}
	if stale {
		fmt.Fprintln(errOut, "hrx: stale authorization allowed for listing only; launches always refresh")
	}
	fmt.Fprintf(out, "Authorization checked %s; harness=%s (not a live inference test)\n", snapshot.RefreshedAt.Format(time.RFC3339), harness)
	for _, name := range snapshot.Names(harness) {
		m, _ := snapshot.Resolve(name, harness)
		fmt.Fprintf(out, "%s\t%s\t%s\n", name, m.Route, strings.Join(m.Harnesses[harness].Reasoning, ", "))
	}
	if len(snapshot.Names(harness)) == 0 {
		fmt.Fprintln(errOut, "hrx: no authorized routes with declared "+harness+" capabilities")
	}
	return true, nil
}
