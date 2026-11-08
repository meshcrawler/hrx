// Package codexlaunch translates HRX's final Codex argv into a fresh-admission
// plan: private app-server overrides, HRX's own thread/start parameters, and the
// client-only TUI attach arguments. Design: docs/internal/codex-launch-mapping.md.
//
// The package is pure and dormant. Nothing here launches or connects to a
// process, and no production path calls it yet. A plan is launch intent, not a
// resolved effective configuration: values left to Codex config layers are
// Unset, and the plan always carries intrinsic blockers, so no API here can
// authorize admission.
package codexlaunch

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Provenance records where a requested value came from.
type Provenance string

const (
	HRXFlag    Provenance = "hrx_flag"
	HRXConfig  Provenance = "hrx_config_override"
	UserFlag   Provenance = "user_flag"
	UserConfig Provenance = "user_config_override"
	LaunchDir  Provenance = "launch_dir"
	Planner    Provenance = "planner"
)

// Want is one requested value. Unset means the value is left to config layers
// the planner cannot see (managed, user, profile or project), never "default".
type Want[T any] struct {
	Set    bool
	Value  T
	From   Provenance
	Tokens []int // argv indexes that produced it
}

func want[T any](v T, from Provenance, tokens ...int) Want[T] {
	return Want[T]{Set: true, Value: v, From: from, Tokens: tokens}
}

// SandboxWant mirrors the fields of the native SandboxPolicy variants
// (app-server-protocol v2 permissions.rs:541-568). Roots stay a typed slice.
type SandboxWant struct {
	Mode                Want[string] // read-only, workspace-write, danger-full-access
	NetworkAccess       Want[bool]
	WritableRoots       Want[[]string]
	ExcludeTmpdirEnvVar Want[bool]
	ExcludeSlashTmp     Want[bool]
}

// Intent is what HRX and the user requested, with provenance.
type Intent struct {
	Model, ModelProvider, ReasoningEffort, ServiceTier Want[string]
	Cwd                                                Want[string]
	ApprovalPolicy, ApprovalsReviewer                  Want[string]
	Sandbox                                            SandboxWant
	RuntimeWorkspaceRoots                              Want[[]string]
	WebSearch                                          Want[string]
}

// Destination is where a token's effect goes. One token may have several.
type Destination string

const (
	ToServer Destination = "server_config"
	ToStart  Destination = "thread_start"
	ToAttach Destination = "tui_attach"
	Syntax   Destination = "syntax" // the "--" separator
)

// TokenTrace accounts for one argv token without copying its text, which may
// contain provider auth commands.
type TokenTrace struct {
	Index        int
	Destinations []Destination
}

// ThreadStart is the thread/start params HRX would send. It carries only Set
// intent and never an invented default.
type ThreadStart struct {
	Model                 *string        `json:"model,omitempty"`
	ModelProvider         *string        `json:"modelProvider,omitempty"`
	ServiceTier           *string        `json:"serviceTier,omitempty"`
	Cwd                   string         `json:"cwd"`
	ApprovalPolicy        *string        `json:"approvalPolicy,omitempty"`
	ApprovalsReviewer     *string        `json:"approvalsReviewer,omitempty"`
	Sandbox               *string        `json:"sandbox,omitempty"`
	Config                map[string]any `json:"config,omitempty"`
	HistoryMode           string         `json:"historyMode"`
	Ephemeral             bool           `json:"ephemeral"`
	ThreadSource          string         `json:"threadSource,omitempty"`
	DeveloperInstructions *string        `json:"developerInstructions,omitempty"`
}

// Attach is the client-only TUI invocation. It never carries a permission,
// model or provider option: remote resume rejects or ignores those
// (tui/src/app/startup.rs:591-605; thread_processor.rs:4567-4587).
type Attach struct {
	Flags  []string
	Prompt *string
}

// Argv is the TUI argv after the codex binary.
func (a Attach) Argv(endpoint, thread string) []string {
	argv := append([]string{"--remote", endpoint}, a.Flags...)
	argv = append(argv, "resume", thread)
	if a.Prompt != nil {
		argv = append(argv, "--", *a.Prompt)
	}
	return argv
}

// Blocker is a prerequisite that prevents admission. Blockers are computed by
// the planner and cannot be supplied or removed by callers.
type Blocker string

const (
	BlockConfigLayers          Blocker = "config_layer_resolution"
	BlockDeveloperInstructions Blocker = "native_tui_developer_instructions"
	BlockPermissionProfile     Blocker = "native_tui_permission_profile_selection"
	BlockSessionStartSource    Blocker = "native_tui_session_start_source"
	BlockThreadSource          Blocker = "native_tui_thread_source"
	unresolvedPrefix                   = "unresolved:"
)

// Plan is the dormant fresh-admission plan for one launch.
type Plan struct {
	ServerArgs []string // after "app-server", excluding --listen
	Start      ThreadStart
	Attach     Attach
	Intent     Intent
	Trace      []TokenTrace
	blockers   []Blocker
}

// Blockers returns the plan's intrinsic prerequisites. In v1 the list is never
// empty: config layers and native TUI start parity are always unresolved.
func (p Plan) Blockers() []Blocker { return slices.Clone(p.blockers) }

// UnsupportedOption rejects a token instead of dropping it.
type UnsupportedOption struct {
	Index  int
	Option string // the option name or a category, never a value
	Reason string
}

func (e *UnsupportedOption) Error() string {
	return fmt.Sprintf("codex argument %d (%s) is not supported for fresh admission: %s", e.Index, e.Option, e.Reason)
}

// Input splits the final native argv by origin. Token indexes refer to
// HRXArgs followed by UserArgs.
type Input struct {
	HRXArgs, UserArgs []string
	Project           string // absolute launch directory
}

type optionSpec struct {
	long, short string
	value       bool
	reject      string // non-empty: unsupported, with this reason
}

var options = []optionSpec{
	{long: "config", short: "c", value: true},
	{long: "model", short: "m", value: true},
	{long: "sandbox", short: "s", value: true},
	{long: "ask-for-approval", short: "a", value: true},
	{long: "cd", short: "C", value: true},
	{long: "enable", value: true},
	{long: "disable", value: true},
	{long: "yolo"},
	{long: "dangerously-bypass-approvals-and-sandbox"},
	{long: "approve-for-me"},
	{long: "not-so-yolo"},
	{long: "search"},
	{long: "strict-config"},
	{long: "no-alt-screen"},
	{long: "profile", short: "p", value: true, reject: "app-server loads no profile; profile materialization is a follow-on"},
	{long: "image", short: "i", value: true, reject: "remote image attachment is unconfirmed"},
	{long: "add-dir", value: true, reject: "runtime workspace roots are a follow-on; remote TUI rejects --add-dir"},
	{long: "local-provider", value: true, reject: "provider selection belongs to HRX routing"},
	{long: "remote", value: true, reject: "HRX owns the transport"},
	{long: "remote-auth-token-env", value: true, reject: "HRX owns the transport"},
	{long: "oss", reject: "provider selection belongs to HRX routing"},
	{long: "dangerously-bypass-hook-trust", reject: "conflicts with HRX hook-trust policy"},
	{long: "worktree", reject: "rejected by Codex in remote mode"},
	{long: "no-daemon", reject: "HRX runs its own private server; not silently removed"},
}

// Codex subcommands (cli/src/main.rs Subcommand), including visible aliases.
var subcommands = []string{"agents", "tcp-tunnel", "exec", "e", "review", "login", "logout", "mcp",
	"plugin", "app-server", "remote-control", "app", "completion", "update", "doctor", "sandbox",
	"debug", "execpolicy", "apply", "a", "resume", "queue", "archive", "delete", "migrate-rollouts",
	"unarchive", "fork", "cloud", "cloud-tasks", "responses-api-proxy", "stdio-to-uds", "exec-server",
	"features", "help"}

var featureName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func findOption(long, short string) (optionSpec, bool) {
	for _, o := range options {
		if (long != "" && o.long == long) || (short != "" && o.short == short) {
			return o, true
		}
	}
	return optionSpec{}, false
}

type planner struct {
	in                Input
	argv              []string
	plan              Plan
	typed             map[string]Want[string] // typed flags win over -c
	seen              map[string]int          // non-repeatable options already used
	prompt            *string
	configs           []configOverride
	enables, disables []string
}

type configOverride struct {
	key, raw          string
	from              Provenance
	index, valueIndex int
}

// Build plans one launch. Every token is traced to one or more destinations, or
// the whole plan fails with an *UnsupportedOption naming it.
func Build(in Input) (Plan, error) {
	if !filepath.IsAbs(in.Project) {
		return Plan{}, &UnsupportedOption{Index: -1, Option: "project", Reason: "launch directory must be absolute"}
	}
	p := &planner{in: in, argv: append(slices.Clone(in.HRXArgs), in.UserArgs...),
		typed: map[string]Want[string]{}, seen: map[string]int{}}
	p.plan.Intent.Cwd = want(filepath.Clean(in.Project), LaunchDir)
	// Remote TUI leaves roots to the server (ThreadParamsMode::Remote).
	// Omission is not an assertion that the effective root set is empty.
	if err := p.scan(); err != nil {
		return Plan{}, err
	}
	if err := p.resolve(); err != nil {
		return Plan{}, err
	}
	p.finish()
	return p.plan, nil
}

func (p *planner) origin(index int, hrx, user Provenance) Provenance {
	if index < len(p.in.HRXArgs) {
		return hrx
	}
	return user
}

func (p *planner) trace(index int, to ...Destination) {
	p.plan.Trace = append(p.plan.Trace, TokenTrace{Index: index, Destinations: to})
}

func (p *planner) scan() error {
	literal := false
	for i := 0; i < len(p.argv); i++ {
		token := p.argv[i]
		if !literal && token == "--" {
			literal = true
			p.trace(i, Syntax)
			continue
		}
		if literal || !strings.HasPrefix(token, "-") || token == "-" {
			if err := p.positional(i, token, literal); err != nil {
				return err
			}
			continue
		}
		var spec optionSpec
		var value string
		var inline, known bool
		if strings.HasPrefix(token, "--") {
			name, v, eq := strings.Cut(token[2:], "=")
			spec, known = findOption(name, "")
			value, inline = v, eq
			if known && !spec.value && eq {
				return &UnsupportedOption{Index: i, Option: "--" + name, Reason: "flag takes no value"}
			}
		} else {
			spec, known = findOption("", token[1:2])
			if rest := token[2:]; rest != "" {
				if !known || !spec.value {
					return &UnsupportedOption{Index: i, Option: "-" + token[1:2], Reason: "unknown or combined short option"}
				}
				value, inline = strings.TrimPrefix(rest, "="), true
			}
		}
		if !known {
			return &UnsupportedOption{Index: i, Option: optionName(token), Reason: "unknown option"}
		}
		name := "--" + spec.long
		if spec.reject != "" {
			return &UnsupportedOption{Index: i, Option: name, Reason: spec.reject}
		}
		optionIndex, valueIndex := i, i
		if spec.value && !inline {
			if i+1 >= len(p.argv) || (strings.HasPrefix(p.argv[i+1], "-") && p.argv[i+1] != "-") {
				return &UnsupportedOption{Index: i, Option: name, Reason: "missing value"}
			}
			i++
			valueIndex, value = i, p.argv[i]
		}
		if err := p.option(spec, optionIndex, valueIndex, value); err != nil {
			return err
		}
	}
	return nil
}

func optionName(token string) string {
	if strings.HasPrefix(token, "--") {
		name, _, _ := strings.Cut(token, "=")
		return name
	}
	return token[:2]
}

func (p *planner) positional(i int, token string, literal bool) error {
	if !literal && p.prompt == nil && slices.Contains(subcommands, token) {
		return &UnsupportedOption{Index: i, Option: "subcommand", Reason: "fresh admission supports new interactive sessions only"}
	}
	if p.prompt != nil {
		return &UnsupportedOption{Index: i, Option: "prompt", Reason: "more than one prompt argument"}
	}
	if i < len(p.in.HRXArgs) {
		return &UnsupportedOption{Index: i, Option: "prompt", Reason: "HRX arguments carry no prompt"}
	}
	prompt := token
	p.prompt = &prompt
	p.trace(i, ToAttach)
	return nil
}

func (p *planner) once(name string, i int) error {
	if _, dup := p.seen[name]; dup {
		return &UnsupportedOption{Index: i, Option: name, Reason: "given more than once"}
	}
	p.seen[name] = i
	return nil
}

func (p *planner) traceBoth(i, valueIndex int, to ...Destination) {
	p.trace(i, to...)
	if valueIndex != i {
		p.trace(valueIndex, to...)
	}
}

func (p *planner) option(spec optionSpec, i, valueIndex int, value string) error {
	flag := p.origin(i, HRXFlag, UserFlag)
	switch spec.long {
	case "config":
		key, raw, ok := strings.Cut(value, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return &UnsupportedOption{Index: i, Option: "--config", Reason: "override must be key=value"}
		}
		p.configs = append(p.configs, configOverride{key, strings.TrimSpace(raw), p.origin(i, HRXConfig, UserConfig), i, valueIndex})
		p.traceBoth(i, valueIndex, ToServer)
	case "enable", "disable":
		if !featureName.MatchString(value) {
			return &UnsupportedOption{Index: i, Option: "--" + spec.long, Reason: "invalid feature name"}
		}
		if spec.long == "enable" {
			p.enables = append(p.enables, value)
		} else {
			p.disables = append(p.disables, value)
		}
		p.traceBoth(i, valueIndex, ToServer)
	case "model":
		if err := p.once("--model", i); err != nil {
			return err
		}
		p.typed["model"] = want(value, flag, i, valueIndex)
		p.traceBoth(i, valueIndex, ToStart)
	case "sandbox":
		if err := p.once("--sandbox", i); err != nil {
			return err
		}
		if !slices.Contains([]string{"read-only", "workspace-write", "danger-full-access"}, value) {
			return &UnsupportedOption{Index: i, Option: "--sandbox", Reason: "unknown sandbox mode"}
		}
		p.typed["sandbox"] = want(value, flag, i, valueIndex)
		p.traceBoth(i, valueIndex, ToStart)
	case "ask-for-approval":
		if err := p.once("--ask-for-approval", i); err != nil {
			return err
		}
		if !slices.Contains([]string{"on-request", "never"}, value) { // approval_mode_cli_arg.rs:9-16
			return &UnsupportedOption{Index: i, Option: "--ask-for-approval", Reason: "unknown approval mode"}
		}
		p.typed["approval"] = want(value, flag, i, valueIndex)
		p.traceBoth(i, valueIndex, ToStart)
	case "cd":
		if err := p.once("--cd", i); err != nil {
			return err
		}
		dir := value
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(p.in.Project, dir)
		}
		// Lexical only: no claim of realpath or canonical identity.
		p.plan.Intent.Cwd = want(filepath.Clean(dir), flag, i, valueIndex)
		p.traceBoth(i, valueIndex, ToStart)
	case "yolo", "dangerously-bypass-approvals-and-sandbox":
		if err := p.once("--yolo", i); err != nil {
			return err
		}
		p.typed["yolo"] = want("true", flag, i)
		p.trace(i, ToStart)
	case "approve-for-me", "not-so-yolo":
		if err := p.once("--approve-for-me", i); err != nil {
			return err
		}
		p.typed["approve-for-me"] = want("true", flag, i)
		p.trace(i, ToStart)
	case "search":
		if err := p.once("--search", i); err != nil {
			return err
		}
		p.typed["search"] = want("live", flag, i)
		p.trace(i, ToStart)
	case "strict-config":
		if err := p.once("--strict-config", i); err != nil {
			return err
		}
		p.plan.ServerArgs = append(p.plan.ServerArgs, "--strict-config")
		p.plan.Attach.Flags = append(p.plan.Attach.Flags, "--strict-config")
		p.trace(i, ToServer, ToAttach)
	case "no-alt-screen":
		if err := p.once("--no-alt-screen", i); err != nil {
			return err
		}
		p.plan.Attach.Flags = append(p.plan.Attach.Flags, "--no-alt-screen")
		p.trace(i, ToAttach)
	default:
		return &UnsupportedOption{Index: i, Option: "--" + spec.long, Reason: "unmapped option"}
	}
	return nil
}

// Config roots and keys whose meaning the planner cannot represent faithfully yet.
var (
	unsupportedConfigRoots = []string{"permissions", "default_permissions", "network", "profiles"}
	unsupportedConfigKeys  = []string{"sandbox_workspace_write", "profile"}
)

func (p *planner) resolve() error {
	in := &p.plan.Intent
	for _, c := range p.configs {
		root, _, _ := strings.Cut(c.key, ".")
		if slices.Contains(unsupportedConfigKeys, c.key) || slices.Contains(unsupportedConfigRoots, root) {
			return &UnsupportedOption{Index: c.index, Option: "--config " + c.key, Reason: "permission profile and network config are not yet representable"}
		}
		p.plan.ServerArgs = append(p.plan.ServerArgs, "-c", c.key+"="+c.raw)
		value := overrideValue(c.raw)
		str := func() (string, error) {
			v, ok := value.(string)
			if !ok {
				return "", &UnsupportedOption{Index: c.index, Option: "--config " + c.key, Reason: "value is not a string"}
			}
			return v, nil
		}
		boolean := func() (bool, error) {
			v, ok := value.(bool)
			if !ok {
				return false, &UnsupportedOption{Index: c.index, Option: "--config " + c.key, Reason: "value is not a boolean"}
			}
			return v, nil
		}
		var err error
		switch c.key {
		case "model", "model_provider", "model_reasoning_effort", "service_tier", "approval_policy", "approvals_reviewer", "sandbox_mode", "web_search":
			var v string
			if v, err = str(); err == nil {
				w := want(v, c.from, c.index, c.valueIndex)
				switch c.key {
				case "model":
					in.Model = w
				case "model_provider":
					in.ModelProvider = w
				case "model_reasoning_effort":
					in.ReasoningEffort = w
				case "service_tier":
					in.ServiceTier = w
				case "approval_policy":
					in.ApprovalPolicy = w
				case "approvals_reviewer":
					in.ApprovalsReviewer = w
				case "sandbox_mode":
					in.Sandbox.Mode = w
				case "web_search":
					in.WebSearch = w
				}
			}
		case "sandbox_workspace_write.network_access", "sandbox_workspace_write.exclude_tmpdir_env_var", "sandbox_workspace_write.exclude_slash_tmp":
			var v bool
			if v, err = boolean(); err == nil {
				w := want(v, c.from, c.index, c.valueIndex)
				switch c.key {
				case "sandbox_workspace_write.network_access":
					in.Sandbox.NetworkAccess = w
				case "sandbox_workspace_write.exclude_tmpdir_env_var":
					in.Sandbox.ExcludeTmpdirEnvVar = w
				default:
					in.Sandbox.ExcludeSlashTmp = w
				}
			}
		case "sandbox_workspace_write.writable_roots":
			items, ok := value.([]any)
			if !ok {
				return &UnsupportedOption{Index: c.index, Option: "--config " + c.key, Reason: "value is not a string array"}
			}
			roots := make([]string, len(items))
			for i, item := range items {
				root, isString := item.(string)
				if !isString {
					return &UnsupportedOption{Index: c.index, Option: "--config " + c.key, Reason: "value is not a string array"}
				}
				if !filepath.IsAbs(root) {
					return &UnsupportedOption{Index: c.index, Option: "--config " + c.key, Reason: "writable roots must be absolute"}
				}
				roots[i] = filepath.Clean(root)
			}
			in.Sandbox.WritableRoots = want(roots, c.from, c.index, c.valueIndex)
		}
		if err != nil {
			return err
		}
	}
	// Native CLI folds toggles after all raw overrides, enables before disables
	// (cli/src/main.rs:976-987,1061-1063), regardless of argv ordering. Keep the
	// native flags so its known-feature validation cannot be bypassed with -c.
	for _, feature := range p.enables {
		p.plan.ServerArgs = append(p.plan.ServerArgs, "--enable", feature)
	}
	for _, feature := range p.disables {
		p.plan.ServerArgs = append(p.plan.ServerArgs, "--disable", feature)
	}
	// Typed flags win over merged TOML (core/src/config/mod.rs:3854, 4083).
	yolo, approve := p.typed["yolo"], p.typed["approve-for-me"]
	if yolo.Set && (approve.Set || p.typed["approval"].Set || p.typed["sandbox"].Set) {
		return &UnsupportedOption{Index: yolo.Tokens[0], Option: "--yolo", Reason: "conflicts with an explicit approval or sandbox option"}
	}
	if approve.Set && (p.typed["approval"].Set || p.typed["sandbox"].Set) {
		return &UnsupportedOption{Index: approve.Tokens[0], Option: "--approve-for-me", Reason: "conflicts with --ask-for-approval or --sandbox"}
	}
	if w := p.typed["model"]; w.Set {
		in.Model = w
	}
	if w := p.typed["sandbox"]; w.Set {
		in.Sandbox.Mode = w
	}
	if w := p.typed["approval"]; w.Set {
		in.ApprovalPolicy = w
	}
	// --search is appended as web_search="live" after every CLI override
	// (tui/src/startup_orchestration.rs:67-71), so it wins in either order.
	if w := p.typed["search"]; w.Set {
		in.WebSearch = w
	}
	if yolo.Set { // shared_options.rs:54-59; startup_orchestration.rs:51-56
		in.Sandbox.Mode = want("danger-full-access", yolo.From, yolo.Tokens...)
		in.ApprovalPolicy = want("never", yolo.From, yolo.Tokens...)
	}
	if approve.Set { // shared_options.rs:80-93 applies these as -c overrides
		from := UserConfig
		if approve.From == HRXFlag {
			from = HRXConfig
		}
		in.ApprovalsReviewer = want("auto_review", from, approve.Tokens...)
		in.ApprovalPolicy = want("on-request", from, approve.Tokens...)
		in.Sandbox.Mode = want("workspace-write", from, approve.Tokens...)
	}
	return nil
}

func (p *planner) finish() {
	in := p.plan.Intent
	ptr := func(w Want[string]) *string {
		if !w.Set {
			return nil
		}
		v := w.Value
		return &v
	}
	p.plan.Start = ThreadStart{Model: ptr(in.Model), ModelProvider: ptr(in.ModelProvider), ServiceTier: ptr(in.ServiceTier), Cwd: in.Cwd.Value,
		ApprovalPolicy: ptr(in.ApprovalPolicy), ApprovalsReviewer: ptr(in.ApprovalsReviewer),
		Sandbox: ptr(in.Sandbox.Mode), HistoryMode: "paginated", ThreadSource: "user"}
	config := map[string]any{}
	if in.ReasoningEffort.Set {
		config["model_reasoning_effort"] = in.ReasoningEffort.Value
	}
	if in.WebSearch.Set {
		config["web_search"] = in.WebSearch.Value
	}
	if len(config) > 0 {
		p.plan.Start.Config = config
	}
	p.plan.Start.DeveloperInstructions = p.explicitTerminalInstructions()
	p.plan.Attach.Prompt = p.prompt
	p.plan.blockers = intrinsicBlockers(in)
}

// Only invocation-selected inputs are available here. Inherited instructions
// and managed feature policy still require config-layer resolution.
func (p *planner) explicitTerminalInstructions() *string {
	const feature = "terminal_visualization_instructions"
	var enabled bool
	var text *string
	for _, c := range p.configs {
		if c.key == "features" {
			enabled = false
			if table, ok := overrideValue(c.raw).(map[string]any); ok {
				enabled, _ = table[feature].(bool)
			}
		} else if c.key == "features."+feature {
			enabled, _ = overrideValue(c.raw).(bool)
		}
		if c.key == "developer_instructions" {
			text = nil
			if value, ok := overrideValue(c.raw).(string); ok {
				text = &value
			}
		}
	}
	// Native applies all enables, then all disables, after raw overrides.
	if slices.Contains(p.enables, feature) {
		enabled = true
	}
	if slices.Contains(p.disables, feature) {
		enabled = false
	}
	if !enabled || text == nil {
		return nil
	}
	// Codex 83086188, tui/src/terminal_visualization_instructions.rs.
	const guidance = "- This surface is a terminal. When the formatting rules require a visual, include one in the final answer using compact ASCII diagrams, trees, timelines, or tables.\n" +
		"- Use tables for exact mappings or comparisons rather than collapsing known mappings into prose.\n" +
		"- Use trees for hierarchy or one-to-many relationships, and diagrams or timelines for sequence, change, or state transferred between records across event order.\n" +
		"- Use only ASCII characters in visuals."
	result := guidance
	if strings.TrimSpace(*text) != "" {
		result = *text + "\n\n" + guidance
	}
	return &result
}

// intrinsicBlockers lists what the planner cannot resolve. Config layers and
// native TUI start behaviour (app_server_session.rs:2150-2188) are always
// unresolved in v1, so admission is never authorized.
func intrinsicBlockers(in Intent) []Blocker {
	blockers := []Blocker{BlockConfigLayers, BlockDeveloperInstructions, BlockPermissionProfile,
		BlockSessionStartSource, BlockThreadSource}
	unset := func(name string, set bool) {
		if !set {
			blockers = append(blockers, Blocker(unresolvedPrefix+name))
		}
	}
	unset("model", in.Model.Set)
	unset("model_provider", in.ModelProvider.Set)
	unset("reasoning_effort", in.ReasoningEffort.Set)
	unset("approval_policy", in.ApprovalPolicy.Set)
	unset("approvals_reviewer", in.ApprovalsReviewer.Set)
	unset("runtime_workspace_roots", in.RuntimeWorkspaceRoots.Set)
	unset("sandbox.mode", in.Sandbox.Mode.Set)
	for _, field := range applicableSandboxFields(in.Sandbox.Mode) {
		switch field {
		case "network_access":
			unset("sandbox.network_access", in.Sandbox.NetworkAccess.Set)
		case "writable_roots":
			unset("sandbox.writable_roots", in.Sandbox.WritableRoots.Set)
		case "exclude_tmpdir_env_var":
			unset("sandbox.exclude_tmpdir_env_var", in.Sandbox.ExcludeTmpdirEnvVar.Set)
		case "exclude_slash_tmp":
			unset("sandbox.exclude_slash_tmp", in.Sandbox.ExcludeSlashTmp.Set)
		}
	}
	return blockers
}

// applicableSandboxFields lists the policy fields of the requested mode. An
// unset mode leaves every field applicable and unresolved.
func applicableSandboxFields(mode Want[string]) []string {
	all := []string{"network_access", "writable_roots", "exclude_tmpdir_env_var", "exclude_slash_tmp"}
	if !mode.Set {
		return all
	}
	switch mode.Value {
	case "danger-full-access":
		return nil
	case "read-only":
		return []string{"network_access"}
	}
	return all
}

// overrideValue mirrors Codex's -c value semantics (utils/cli/src/config_override.rs:49-104).
// The value is parsed as the TOML document "_x_ = <raw>". When that fails it is
// kept as a raw string with any surrounding quote characters trimmed. A
// successful parse may yield a string, bool, number, array or table; callers
// reject a type their key cannot take, as native config loading would.
func overrideValue(raw string) any {
	var table map[string]any
	if err := toml.Unmarshal([]byte("_x_ = "+raw), &table); err == nil {
		if value, ok := table["_x_"]; ok {
			return value
		}
	}
	return strings.Trim(strings.TrimSpace(raw), `"'`)
}
