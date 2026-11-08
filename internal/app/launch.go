package app

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/meshcrawler/hrx/internal/catalog"
	"github.com/meshcrawler/hrx/internal/hub"
	"github.com/meshcrawler/hrx/internal/session"
)

type LaunchOptions struct {
	ProfileName, CredentialFile string
	profile                     *session.LaunchProfile
	provenance                  map[string]string
	Plugin, AgentIdentity       string
	resumeInsertion             int
	project                     string
	restoredRoute               string
	Model, Reasoning            string
	DryRun                      bool
	Pitput                      bool
	PitputBoards                []string
	PitputTargeted              bool
	PitputCombinedPreview       bool
	PitputAllActivity           bool
	pitputSelectionSet          bool
	PitputFollowThreads         []string
	// PitputDM is "" (direct messages on, the default), "off", or "only" (no boards).
	PitputDM                            string
	Extra                               []string
	YesodMail, YesodBBS, Yolo, Continue bool
	YesodAgent, Resume                  string
	yoloSet                             bool
	// ClaudeConfigDir is an HRX-managed Claude configuration directory; ClaudeInit
	// names the first-run answers HRX may write there (theme, trust, bypass).
	ClaudeConfigDir string
	ClaudeInit      []string
	// claudeConfigRestored marks a directory restored from a session record: it must
	// already exist, because the native session lives in it.
	claudeConfigRestored bool
}

func ParseLaunch(args []string) (LaunchOptions, error) {
	return parseLaunchFor("codex", args)
}

func parseLaunchFor(harness string, args []string) (LaunchOptions, error) {
	var o LaunchOptions
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			o.Extra = append(o.Extra, args[i+1:]...)
			break
		}
		name, value, equals := strings.Cut(a, "=")
		switch name {
		case "--pitput-all-activity":
			if equals {
				return o, errors.New("--pitput-all-activity does not take a value")
			}
			o.PitputAllActivity = true
			o.pitputSelectionSet = true
		case "--pitput-combined-preview":
			if equals {
				return o, errors.New("--pitput-combined-preview does not take a value")
			}
			o.PitputCombinedPreview = true
		case "--pitput-targeted":
			if equals {
				return o, errors.New("--pitput-targeted does not take a value")
			}
			o.PitputTargeted = true
			o.pitputSelectionSet = true
		case "--pitput-follow-thread":
			if !equals {
				i++
				if i >= len(args) {
					return o, errors.New("--pitput-follow-thread requires /b/BOARD/TOPIC")
				}
				value = args[i]
			}
			o.PitputTargeted = true
			o.pitputSelectionSet = true
			o.PitputFollowThreads = append(o.PitputFollowThreads, value)
		case "--pitput-board":
			if !equals {
				i++
				if i >= len(args) {
					return o, errors.New("--pitput-board requires a board")
				}
				value = args[i]
			}
			value = strings.TrimPrefix(value, "/b/")
			if !pitputBoardName.MatchString(value) {
				return o, errors.New("invalid Pitput board")
			}
			o.PitputBoards = append(o.PitputBoards, value)
		case "--gitlab":
			if equals || seen["--plugin"] {
				return o, errors.New("--gitlab takes no value and cannot be combined with --plugin")
			}
			seen["--plugin"] = true
			o.Plugin = "gitlab-service-account-broker"
		case "--pitput-no-dm", "--pitput-dm-only":
			if equals {
				return o, fmt.Errorf("%s does not take a value", name)
			}
			mode := map[string]string{"--pitput-no-dm": "off", "--pitput-dm-only": "only"}[name]
			if o.PitputDM != "" && o.PitputDM != mode {
				return o, errors.New("specify only one of --pitput-no-dm or --pitput-dm-only")
			}
			o.PitputDM = mode
		case "--pitput", "--yesod-mail", "--yesod-bbs", "--yesod-comms", "--yolo", "--no-yolo", "--continue":
			if equals {
				return o, fmt.Errorf("%s does not take a value", name)
			}
			switch name {
			case "--pitput":
				o.Pitput = true
			case "--yesod-mail":
				o.YesodMail = true
			case "--yesod-bbs":
				o.YesodBBS = true
			case "--yesod-comms":
				o.YesodMail, o.YesodBBS = true, true
			case "--yolo", "--no-yolo":
				if o.yoloSet {
					return o, errors.New("specify only one of --yolo or --no-yolo")
				}
				o.Yolo, o.yoloSet = name == "--yolo", true
			case "--continue":
				o.Continue = true
			}
		case "--latest":
			if equals {
				return o, errors.New("--latest does not take a value")
			}
			// This controls shell suggestions, never the explicitly chosen model.
		case "--dry-run":
			if equals {
				return o, errors.New("--dry-run does not take a value")
			}
			o.DryRun = true
		case "--profile", "--credential-file", "--model", "--reasoning", "--yesod-agent", "--resume", "--plugin", "--agent-identity",
			"--claude-config-dir", "--claude-init":
			if seen[name] {
				return o, fmt.Errorf("duplicate %s", name)
			}
			seen[name] = true
			if !equals {
				i++
				if i >= len(args) {
					return o, fmt.Errorf("missing value for %s", name)
				}
				value = args[i]
			}
			if value == "" || strings.HasPrefix(value, "-") {
				return o, fmt.Errorf("missing value for %s", name)
			}
			if name == "--profile" {
				o.ProfileName = value
			} else if name == "--credential-file" {
				o.CredentialFile = value
			} else if name == "--plugin" {
				o.Plugin = value
			} else if name == "--agent-identity" {
				o.AgentIdentity = value
			} else if name == "--model" {
				o.Model = value
			} else if name == "--reasoning" {
				o.Reasoning = value
			} else if name == "--resume" {
				o.Resume = value
			} else if name == "--claude-config-dir" {
				o.ClaudeConfigDir = value
			} else if name == "--claude-init" {
				parts, err := parseClaudeInit(value)
				if err != nil {
					return o, err
				}
				o.ClaudeInit = parts
			} else {
				o.YesodAgent = value
			}
		default:
			o.Extra = append(o.Extra, args[i:]...)
			i = len(args)
		}
	}
	for _, token := range nativeArguments(harness, o.Extra) {
		if !token.option {
			continue
		}
		a := o.Extra[token.index]
		name, _, _ := strings.Cut(a, "=")
		if name == "--profile" || name == "--credential-file" {
			return o, errors.New("put HRX profile and credential options before the harness passthrough separator --")
		}
		if slices.Contains([]string{"--gitlab", "--plugin", "--agent-identity", "--pitput", "--pitput-board", "--pitput-combined-preview", "--pitput-targeted", "--pitput-all-activity", "--pitput-follow-thread", "--pitput-no-dm", "--pitput-dm-only", "--yesod-mail", "--yesod-bbs", "--yesod-comms", "--yesod-agent"}, name) {
			return o, errors.New("put Yesod options before the harness passthrough separator --")
		}
		if harness != "codex" {
			blocked := []string{"--model", "--effort", "--variant", "--settings", "--setting-sources", "--fallback-model", "--agents", "--agent", "--attach", "--cloud", "--desktop", "--remote", "--bare"}
			if slices.Contains(blocked, name) || strings.HasPrefix(a, "-m") {
				return o, errors.New("routing overrides are not allowed in passthrough; use hrx --model and --reasoning")
			}
			continue
		}
		if slices.Contains([]string{"--model", "--config", "--profile", "--oss", "--local-provider", "--remote"}, name) || strings.HasPrefix(a, "-m") || strings.HasPrefix(a, "-c") || strings.HasPrefix(a, "-p") {
			return o, errors.New("model/provider/config overrides are not allowed in passthrough; use hrx --model and --reasoning")
		}
	}
	if harness != "claude" && (o.ClaudeConfigDir != "" || len(o.ClaudeInit) > 0) {
		return o, errors.New("--claude-config-dir and --claude-init apply only to hrx claude")
	}
	if o.Plugin != "" && o.Plugin != "gitlab-service-account-broker" {
		return o, errors.New("unknown HRX plugin")
	}
	if o.Plugin != "" && !brokerIdentity.MatchString(o.AgentIdentity) && !(o.AgentIdentity == "" && (o.Resume != "" || o.Continue)) {
		return o, errors.New("broker plugin requires --agent-identity (1..200 letters, digits, or _ . : / -)")
	}
	if o.AgentIdentity != "" && o.Plugin == "" && !pitputAgentName.MatchString(o.AgentIdentity) {
		return o, errors.New("--agent-identity requires a canonical HOST/ROLE/QUALIFIER identity")
	}
	if len(o.PitputBoards) > 0 && !o.Pitput {
		return o, errors.New("--pitput-board requires --pitput")
	}
	if o.Pitput && o.PitputDM != "only" && !o.pitputSelectionSet && o.Resume == "" && !o.Continue {
		o.PitputTargeted = true
	}
	if err := validatePitputSelection(o, harness); err != nil {
		return o, err
	}
	if o.PitputDM != "" && !o.Pitput {
		return o, errors.New("--pitput-no-dm and --pitput-dm-only require --pitput")
	}
	if o.PitputDM == "only" && len(o.PitputBoards) > 0 {
		return o, errors.New("--pitput-dm-only cannot be combined with --pitput-board")
	}
	if err := validateSessionOptions(harness, o); err != nil {
		return o, err
	}
	if harness == "opencode" && slices.Contains([]string{"attach", "acp", "serve", "web", "github", "pr", "debug", "providers", "auth", "agent", "mcp", "models", "export", "import", "session", "db", "plugin", "plug", "uninstall", "upgrade", "completion", "stats"}, nativeCommand(harness, o.Extra)) {
		return o, errors.New("hrx opencode supports interactive projects and run; invoke administrative subcommands directly")
	}
	return o, nil
}

type Launch struct {
	Profile        *session.LaunchProfile `json:"profile,omitempty"`
	Provenance     map[string]string      `json:"provenance,omitempty"`
	hubClient      *hub.Client
	agentIdentity  string
	monitorRun     string
	pitputRuntime  *pitputRuntime
	sidecarErrors  chan error
	resumeProject  string
	Broker         *BrokerLaunch `json:"broker,omitempty"`
	pendingSession *session.Record
	lease          *yesodLease
	Env            []string     `json:"-"`
	Executable     string       `json:"executable"`
	Provider       string       `json:"provider"`
	BaseURL        string       `json:"base_url"`
	Identity       string       `json:"identity"`
	Model          string       `json:"model"`
	Reasoning      string       `json:"reasoning"`
	Passthrough    []string     `json:"passthrough"`
	Args           []string     `json:"-"`
	Yesod          *YesodLaunch `json:"yesod,omitempty"`
	Yolo           bool         `json:"yolo,omitempty"`
	Resume         string       `json:"resume,omitempty"`
	Continue       bool         `json:"continue,omitempty"`
	Pitput         bool         `json:"pitput,omitempty"`
	Hub            bool         `json:"hub,omitempty"`
}

func BuildLaunch(s Settings, snapshot catalog.Snapshot, o LaunchOptions) (Launch, error) {
	var launch Launch
	if o.Model == "" {
		o.Model = s.DefaultModel
	}
	m, err := snapshot.Resolve(o.Model, "codex")
	if err != nil {
		return launch, err
	}
	if err := validateRestoredRoute(o, m.Route); err != nil {
		return launch, err
	}
	cap := m.Harnesses["codex"]
	if cap.CatalogTemplate != "" && m.Context <= 0 {
		return launch, fmt.Errorf("cannot launch %s: gateway input capacity is unknown; publish model limits and refresh the gateway catalog", m.Route)
	}
	if o.Reasoning == "" {
		o.Reasoning = cap.DefaultReasoning
	}
	if !slices.Contains(cap.Reasoning, o.Reasoning) {
		return launch, fmt.Errorf("reasoning %q is not advertised for %s; supported: %s", o.Reasoning, m.ID, strings.Join(cap.Reasoning, ", "))
	}
	q := strconv.Quote
	p := s.Provider
	provider := "{name=" + q("hrx LiteLLM") + ",base_url=" + q(p.BaseURL) + ",wire_api=" + q(p.WireAPI)
	if p.EnvKey != "" {
		provider += ",env_key=" + q(p.EnvKey)
	} else {
		args := make([]string, len(p.Auth.Args))
		for i, a := range p.Auth.Args {
			args[i] = q(a)
		}
		timeout := p.Auth.TimeoutMS
		if timeout <= 0 {
			timeout = 10000
		}
		provider += ",auth={command=" + q(p.Auth.Command) + ",args=[" + strings.Join(args, ",") + "],timeout_ms=" + strconv.Itoa(timeout) + ",refresh_interval_ms=0}"
	}
	provider += "}"
	launch = Launch{Executable: "codex", Provider: s.ProviderName, BaseURL: p.BaseURL, Identity: m.ID, Model: m.Route, Reasoning: o.Reasoning, Passthrough: o.Extra}
	// CLI overrides pin the provider even if a project has its own Codex config.
	launch.Args = []string{"-c", "model_provider=" + q(s.ProviderName), "-c", "model_providers." + s.ProviderName + "=" + provider, "--model", m.Route, "-c", "model_reasoning_effort=" + q(o.Reasoning)}
	if s.GeneratedCatalogPath != "" {
		launch.Args = append(launch.Args, "-c", "model_catalog_json="+q(s.GeneratedCatalogPath))
	}
	extra, err := sessionArguments("codex", o)
	if err != nil {
		return launch, err
	}
	launch.Args = append(launch.Args, extra...)
	launch.Yolo = o.Yolo
	launch.Resume, launch.Continue = o.Resume, o.Continue
	return launch, nil
}
