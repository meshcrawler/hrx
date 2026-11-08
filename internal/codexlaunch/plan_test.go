package codexlaunch

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

const project = "/work/project"

func TestExplicitTerminalInstructionsRespectLaunchChoices(t *testing.T) {
	const feature = "terminal_visualization_instructions"
	for _, tc := range []struct {
		name    string
		args    []string
		prefix  string
		present bool
	}{
		{"explicit", []string{"--enable", feature, "-c", `developer_instructions="First"`, "-c", `developer_instructions="Last"`}, "Last\n\n- This surface", true},
		{"empty", []string{"--enable", feature, "-c", `developer_instructions=""`}, "- This surface", true},
		{"disabled-wins", []string{"--disable", feature, "--enable", feature, "-c", `developer_instructions="Text"`}, "", false},
		{"inherited-instructions", []string{"--enable", feature}, "", false},
		{"inherited-feature", []string{"-c", `developer_instructions="Text"`}, "", false},
		{"raw-feature", []string{"-c", "features." + feature + "=true", "-c", `developer_instructions="Text"`}, "Text\n\n- This surface", true},
		{"raw-table", []string{"-c", "features={" + feature + "=true}", "-c", `developer_instructions="Text"`}, "Text\n\n- This surface", true},
		{"raw-table-replacement", []string{"-c", "features." + feature + "=true", "-c", "features={}", "-c", `developer_instructions="Text"`}, "", false},
		{"enable-after-raw", []string{"--enable", feature, "-c", "features." + feature + "=false", "-c", `developer_instructions="Text"`}, "Text\n\n- This surface", true},
		{"disable-after-raw", []string{"--disable", feature, "-c", "features." + feature + "=true", "-c", `developer_instructions="Text"`}, "", false},
		{"invalid-final-value", []string{"--enable", feature, "-c", `developer_instructions="Text"`, "-c", `developer_instructions=7`}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := build(t, nil, tc.args)
			got := plan.Start.DeveloperInstructions
			if (got != nil) != tc.present || got != nil && !strings.HasPrefix(*got, tc.prefix) {
				t.Fatal("explicit instruction mapping differs from launch choices")
			}
			if !slices.Contains(plan.Blockers(), BlockDeveloperInstructions) || !slices.Contains(plan.Blockers(), BlockConfigLayers) {
				t.Fatal("explicit mapping incorrectly cleared effective-configuration blockers")
			}
		})
	}
}

func TestExplicitServiceTierReachesNativeStart(t *testing.T) {
	for _, tier := range []string{"default", "fast", "flex"} {
		plan := build(t, []string{"-c", `service_tier="fast"`}, []string{"-c", "service_tier=" + tier})
		raw, err := json.Marshal(plan.Start)
		if err != nil {
			t.Fatal(err)
		}
		var start map[string]any
		if err := json.Unmarshal(raw, &start); err != nil {
			t.Fatal(err)
		}
		if start["serviceTier"] != tier || plan.Intent.ServiceTier.From != UserConfig {
			t.Fatalf("explicit tier lost: start=%s intent=%+v", raw, plan.Intent.ServiceTier)
		}
	}
	raw, err := json.Marshal(build(t, nil, nil).Start)
	if err != nil || strings.Contains(string(raw), `"serviceTier"`) {
		t.Fatalf("invented implicit service tier: %s (%v)", raw, err)
	}
}

// HRX's injected Codex arguments (internal/app/launch.go:285-287).
var hrxArgs = []string{"-c", `model_provider="hrx"`, "-c", `model_providers.hrx={name="hrx",base_url="https://gw/v1"}`,
	"--model", "route-a", "-c", `model_reasoning_effort="high"`}

func build(t *testing.T, hrx, user []string) Plan {
	t.Helper()
	plan, err := Build(Input{HRXArgs: hrx, UserArgs: user, Project: project})
	if err != nil {
		t.Fatalf("Build(%q, %q): %v", hrx, user, err)
	}
	return plan
}

func rejects(t *testing.T, hrx, user []string, option string) {
	t.Helper()
	_, err := Build(Input{HRXArgs: hrx, UserArgs: user, Project: project})
	var unsupported *UnsupportedOption
	if !errors.As(err, &unsupported) || !strings.Contains(unsupported.Option, option) {
		t.Fatalf("Build(%q, %q) = %v, want rejection naming %q", hrx, user, err, option)
	}
}

func TestEveryValueFormIsAccepted(t *testing.T) {
	for _, form := range [][]string{
		{"--model", "m1"}, {"--model=m1"}, {"-m", "m1"}, {"-mm1"}, {"-m=m1"},
	} {
		plan := build(t, form, nil)
		if !plan.Intent.Model.Set || plan.Intent.Model.Value != "m1" || *plan.Start.Model != "m1" {
			t.Fatalf("%q: model intent %+v", form, plan.Intent.Model)
		}
	}
	for _, form := range [][]string{
		{"-c", `model="m2"`}, {"--config", `model="m2"`}, {`--config=model="m2"`}, {`-cmodel="m2"`}, {`-c=model="m2"`},
	} {
		plan := build(t, form, nil)
		if plan.Intent.Model.Value != "m2" || plan.Intent.Model.From != HRXConfig {
			t.Fatalf("%q: model intent %+v", form, plan.Intent.Model)
		}
		if !slices.Equal(plan.ServerArgs, []string{"-c", `model="m2"`}) {
			t.Fatalf("%q: server args %q, want normalized separate form", form, plan.ServerArgs)
		}
	}
}

func TestHRXArgumentsKeepModelAndProvider(t *testing.T) {
	plan := build(t, hrxArgs, nil)
	in := plan.Intent
	if in.Model.Value != "route-a" || in.Model.From != HRXFlag || in.ModelProvider.Value != "hrx" ||
		in.ReasoningEffort.Value != "high" {
		t.Fatalf("intent %+v", in)
	}
	if *plan.Start.Model != "route-a" || *plan.Start.ModelProvider != "hrx" || plan.Start.Config["model_reasoning_effort"] != "high" {
		t.Fatalf("start %+v", plan.Start)
	}
	// Every HRX -c reaches the server layer, including the provider definition.
	if strings.Count(strings.Join(plan.ServerArgs, " "), "-c ") != 3 {
		t.Fatalf("server args %q", plan.ServerArgs)
	}
	if plan.Start.Cwd != project || plan.Intent.Cwd.From != LaunchDir {
		t.Fatalf("cwd %q from %s", plan.Start.Cwd, plan.Intent.Cwd.From)
	}
}

func TestPrecedenceTypedBeatsConfigAndLaterConfigWins(t *testing.T) {
	plan := build(t, []string{"-c", `model="early"`, "-c", `model="late"`}, nil)
	if plan.Intent.Model.Value != "late" {
		t.Fatalf("later -c should win: %+v", plan.Intent.Model)
	}
	plan = build(t, []string{"--model", "typed", "-c", `model="config"`}, nil)
	if plan.Intent.Model.Value != "typed" {
		t.Fatalf("typed flag should win: %+v", plan.Intent.Model)
	}
	plan = build(t, nil, []string{"-s", "read-only", "-c", `sandbox_mode="workspace-write"`})
	if plan.Intent.Sandbox.Mode.Value != "read-only" || plan.Intent.Sandbox.Mode.From != UserFlag {
		t.Fatalf("typed sandbox should win: %+v", plan.Intent.Sandbox.Mode)
	}
	plan = build(t, []string{"-c", `model=bare`}, nil) // raw-string fallback (config_override.rs:69-80)
	if plan.Intent.Model.Value != "bare" {
		t.Fatalf("raw string fallback: %+v", plan.Intent.Model)
	}
}

func TestPermissionMapping(t *testing.T) {
	plan := build(t, []string{"--yolo"}, nil)
	if plan.Intent.Sandbox.Mode.Value != "danger-full-access" || plan.Intent.ApprovalPolicy.Value != "never" ||
		plan.Intent.Sandbox.Mode.From != HRXFlag {
		t.Fatalf("yolo intent %+v / %+v", plan.Intent.Sandbox.Mode, plan.Intent.ApprovalPolicy)
	}
	plan = build(t, nil, []string{"--dangerously-bypass-approvals-and-sandbox"})
	if plan.Intent.Sandbox.Mode.Value != "danger-full-access" || plan.Intent.Sandbox.Mode.From != UserFlag {
		t.Fatalf("long yolo alias %+v", plan.Intent.Sandbox.Mode)
	}
	for _, flag := range []string{"--approve-for-me", "--not-so-yolo"} {
		plan = build(t, nil, []string{flag})
		in := plan.Intent
		if in.ApprovalsReviewer.Value != "auto_review" || in.ApprovalPolicy.Value != "on-request" ||
			in.Sandbox.Mode.Value != "workspace-write" || in.Sandbox.Mode.From != UserConfig {
			t.Fatalf("%s intent %+v", flag, in)
		}
		if in.Sandbox.NetworkAccess.Set || in.Sandbox.WritableRoots.Set {
			t.Fatalf("%s must leave sandbox subfields unresolved", flag)
		}
	}
	plan = build(t, nil, []string{"-a", "on-request", "-s", "workspace-write"})
	if *plan.Start.ApprovalPolicy != "on-request" || *plan.Start.Sandbox != "workspace-write" {
		t.Fatalf("typed permissions %+v", plan.Start)
	}
}

func TestPermissionConflictsAreRejected(t *testing.T) {
	rejects(t, []string{"--yolo"}, []string{"-a", "on-request"}, "--yolo")
	rejects(t, []string{"--yolo"}, []string{"-s", "read-only"}, "--yolo")
	rejects(t, []string{"--yolo"}, []string{"--approve-for-me"}, "--yolo")
	rejects(t, nil, []string{"--approve-for-me", "-s", "read-only"}, "--approve-for-me")
	rejects(t, nil, []string{"-a", "untrusted"}, "--ask-for-approval")
	rejects(t, nil, []string{"-s", "bogus"}, "--sandbox")
}

func TestNoPermissionOrRoutingTokenReachesTheTUI(t *testing.T) {
	inputs := [][]string{{"--yolo"}, {"--approve-for-me"}, {"-a", "never", "-s", "read-only"},
		{"--search", "--no-alt-screen", "--strict-config", "--", "-starts with a dash"}, {"-C", "sub"}}
	for _, user := range inputs {
		plan := build(t, hrxArgs, user)
		for _, flag := range plan.Attach.Flags {
			if !slices.Contains([]string{"--strict-config", "--no-alt-screen"}, flag) {
				t.Fatalf("%q: TUI received %q", user, flag)
			}
		}
		argv := plan.Attach.Argv("unix:///s", "T")
		for _, token := range argv[:len(argv)-2] {
			if strings.Contains(token, "yolo") || token == "-a" || token == "-s" || token == "-m" || token == "-c" {
				t.Fatalf("%q: TUI argv %q", user, argv)
			}
		}
	}
}

func TestPromptAndSeparator(t *testing.T) {
	plan := build(t, hrxArgs, []string{"--no-alt-screen", "--", "-x is data"})
	if plan.Attach.Prompt == nil || *plan.Attach.Prompt != "-x is data" {
		t.Fatalf("prompt %v", plan.Attach.Prompt)
	}
	got := plan.Attach.Argv("unix:///s", "T")
	want := []string{"--remote", "unix:///s", "--no-alt-screen", "resume", "T", "--", "-x is data"}
	if !slices.Equal(got, want) {
		t.Fatalf("attach argv %q, want %q", got, want)
	}
	plan = build(t, nil, []string{"--", "resume"}) // literal after "--" is a prompt, not a subcommand
	if *plan.Attach.Prompt != "resume" {
		t.Fatal("literal resume should be a prompt")
	}
	rejects(t, nil, []string{"one", "two"}, "prompt")
}

func TestEveryTokenIsTraced(t *testing.T) {
	user := []string{"-C", "sub", "--search", "--enable", "f1", "--disable=f2", "-s", "read-only",
		"--strict-config", "--", "hello"}
	plan := build(t, hrxArgs, user)
	traced := map[int]bool{}
	for _, entry := range plan.Trace {
		if len(entry.Destinations) == 0 {
			t.Fatalf("token %d has no destination", entry.Index)
		}
		traced[entry.Index] = true
	}
	for i := range len(hrxArgs) + len(user) {
		if !traced[i] {
			t.Fatalf("token %d is not traced", i)
		}
	}
	if !slices.Contains(plan.ServerArgs, "--enable") || !slices.Contains(plan.ServerArgs, "--disable") || !slices.Contains(plan.ServerArgs, "f1") || !slices.Contains(plan.ServerArgs, "f2") {
		t.Fatalf("server args %q", plan.ServerArgs)
	}
	if plan.Start.Cwd != project+"/sub" || plan.Start.Config["web_search"] != "live" {
		t.Fatalf("start %+v", plan.Start)
	}
}

func TestFeatureTogglePrecedenceMatchesPinnedCLI(t *testing.T) {
	// Codex cli/main.rs:976-987,1061-1063 appends enables then disables
	// after raw -c overrides, irrespective of their positions in argv.
	for _, args := range [][]string{
		{"--disable", "web_search_request", "--enable", "web_search_request", "-c", "features.web_search_request=true"},
		{"-c", "features.web_search_request=true", "--enable", "web_search_request", "--disable", "web_search_request"},
		{"--enable=web_search_request", "-cfeatures.web_search_request=true", "--disable=web_search_request"},
	} {
		plan := build(t, nil, args)
		want := []string{"-c", "features.web_search_request=true", "--enable", "web_search_request", "--disable", "web_search_request"}
		if !slices.Equal(plan.ServerArgs, want) {
			t.Fatalf("argv %q: server args %q, want %q", args, plan.ServerArgs, want)
		}
	}
	plan := build(t, []string{"--disable", "web_search_cached"}, []string{"--enable", "web_search_cached"})
	if !slices.Equal(plan.ServerArgs, []string{"--enable", "web_search_cached", "--disable", "web_search_cached"}) {
		t.Fatalf("HRX/user boundary changed native toggle precedence: %q", plan.ServerArgs)
	}
}

func TestUnsupportedModesAreRejectedNotStripped(t *testing.T) {
	cases := map[string][]string{
		"--profile": {"-p", "x"}, "--image": {"-i", "a.png"}, "--add-dir": {"--add-dir", "/x"},
		"--dangerously-bypass-hook-trust": {"--dangerously-bypass-hook-trust"},
		"--worktree":                      {"--worktree"}, "--no-daemon": {"--no-daemon"},
		"--remote": {"--remote", "ws://x"}, "--oss": {"--oss"}, "--local-provider": {"--local-provider=o"},
		"subcommand": {"resume", "abc"}, "unknown option": {"--frobnicate"},
	}
	for option, user := range cases {
		if option == "unknown option" {
			option = "--frobnicate"
		}
		rejects(t, nil, user, option)
	}
	for _, sub := range []string{"exec", "e", "fork", "apply", "a", "review"} {
		rejects(t, nil, []string{sub}, "subcommand")
	}
	rejects(t, nil, []string{"-m"}, "--model")                    // missing value
	rejects(t, nil, []string{"-m", "--search"}, "--model")        // a value may not be an option
	rejects(t, nil, []string{"--search=yes"}, "--search")         // flags take no value
	rejects(t, nil, []string{"-m", "a", "-m", "b"}, "--model")    // non-repeatable
	rejects(t, nil, []string{"-xy"}, "-x")                        // unknown short option
	rejects(t, []string{"prompt"}, nil, "prompt")                 // HRX args carry no prompt
	rejects(t, []string{"-c", "novalue"}, nil, "--config")        // override must be key=value
	rejects(t, nil, []string{"--enable", "bad name"}, "--enable") // feature name
	_, err := Build(Input{Project: "relative"})
	if err == nil {
		t.Fatal("relative project must be rejected")
	}
}

func TestPermissionConfigKeys(t *testing.T) {
	plan := build(t, []string{"-c", `sandbox_mode="workspace-write"`,
		"-c", `sandbox_workspace_write.writable_roots=["/a", "/b/../c"]`,
		"-c", "sandbox_workspace_write.network_access=true",
		"-c", "sandbox_workspace_write.exclude_tmpdir_env_var=false",
		"-c", "sandbox_workspace_write.exclude_slash_tmp=true"}, nil)
	sb := plan.Intent.Sandbox
	if !slices.Equal(sb.WritableRoots.Value, []string{"/a", "/c"}) || !sb.NetworkAccess.Value ||
		sb.ExcludeTmpdirEnvVar.Value || !sb.ExcludeSlashTmp.Value || sb.Mode.From != HRXConfig {
		t.Fatalf("sandbox intent %+v", sb)
	}
	rejects(t, []string{"-c", "sandbox_workspace_write.network_access=yes"}, nil, "network_access")
	rejects(t, []string{"-c", `sandbox_workspace_write.writable_roots=["rel"]`}, nil, "writable_roots")
	rejects(t, []string{"-c", `sandbox_workspace_write={network_access=true}`}, nil, "sandbox_workspace_write")
	for _, key := range []string{"permissions.x=1", `default_permissions="p"`, "network.enabled=true", "profiles.p.model=1", `profile="p"`} {
		rejects(t, []string{"-c", key}, nil, "--config")
	}
	rejects(t, []string{"-c", "approval_policy=true"}, nil, "approval_policy")
}

func TestBlockersAreIntrinsic(t *testing.T) {
	plan := build(t, hrxArgs, []string{"--yolo"})
	blockers := plan.Blockers()
	for _, required := range []Blocker{BlockConfigLayers, BlockDeveloperInstructions, BlockPermissionProfile,
		BlockSessionStartSource, BlockThreadSource} {
		if !slices.Contains(blockers, required) {
			t.Fatalf("missing intrinsic blocker %s in %v", required, blockers)
		}
	}
	if slices.Contains(blockers, Blocker("unresolved:sandbox.network_access")) {
		t.Fatal("danger-full-access has no network subfield to resolve")
	}
	blockers[0] = "tampered" // callers get a copy
	if plan.Blockers()[0] != BlockConfigLayers {
		t.Fatal("blockers must not be mutable through the returned slice")
	}
	plain := build(t, hrxArgs, nil)
	for _, field := range []string{"approval_policy", "approvals_reviewer", "sandbox.mode", "sandbox.network_access",
		"sandbox.writable_roots", "sandbox.exclude_tmpdir_env_var", "sandbox.exclude_slash_tmp"} {
		if !slices.Contains(plain.Blockers(), Blocker("unresolved:"+field)) {
			t.Fatalf("unset %s must be an unresolved blocker", field)
		}
	}
}

func TestSearchWinsOverWebSearchConfigInEitherOrder(t *testing.T) {
	// startup_orchestration.rs:67-71 appends web_search="live" after every override.
	for _, user := range [][]string{
		{"--search", "-c", "web_search=disabled"},
		{"-c", "web_search=disabled", "--search"},
		{"-c", `web_search="cached"`, "--search", "-c", "web_search=disabled"},
	} {
		plan := build(t, nil, user)
		if plan.Intent.WebSearch.Value != "live" || plan.Intent.WebSearch.From != UserFlag ||
			plan.Start.Config["web_search"] != "live" {
			t.Fatalf("%q: web search %+v, start %v", user, plan.Intent.WebSearch, plan.Start.Config)
		}
	}
	plan := build(t, nil, []string{"-c", "web_search=disabled"})
	if plan.Intent.WebSearch.Value != "disabled" || plan.Intent.WebSearch.From != UserConfig {
		t.Fatalf("config alone: %+v", plan.Intent.WebSearch)
	}
}

func TestConfigValuesFollowNativeTOMLAndFallback(t *testing.T) {
	// config_override.rs:49-104: parse "_x_ = <raw>" as TOML; on failure keep
	// the trimmed raw text with surrounding quote characters removed.
	cases := map[string]string{
		`model="gpt-5"`:       "gpt-5",
		`model='gpt-5'`:       "gpt-5",        // TOML literal string
		`model = "gpt-5"  `:   "gpt-5",        // key and value are trimmed
		`model="a\\b"`:        `a\b`,          // TOML escape, not Go's
		`model='C:\path'`:     `C:\path`,      // literal strings keep backslashes
		`model="m" # comment`: "m",            // comments are TOML
		`model=o3`:            "o3",           // invalid TOML: raw fallback
		`model="\x41"`:        `\x41`,         // invalid TOML escape: raw fallback, quotes trimmed
		`model='unterminated`: "unterminated", // raw fallback trims quote characters
		`model=""quoted""`:    "quoted",       // fallback trims every surrounding quote
		`model="""multi"""`:   "multi",        // TOML multi-line basic string
	}
	for override, want := range cases {
		plan := build(t, []string{"-c", override}, nil)
		if plan.Intent.Model.Value != want {
			t.Errorf("%s: model %q, want %q", override, plan.Intent.Model.Value, want)
		}
	}
	plan := build(t, []string{"-c", `sandbox_workspace_write.writable_roots=['/a', "/b"] # roots`}, nil)
	if !slices.Equal(plan.Intent.Sandbox.WritableRoots.Value, []string{"/a", "/b"}) {
		t.Fatalf("array with literal strings and comment: %q", plan.Intent.Sandbox.WritableRoots.Value)
	}
	// A value TOML parses to the wrong type is rejected, as native config loading would.
	for _, override := range []string{"model=42", "model=true", `model=["a"]`, `model={a=1}`,
		`sandbox_workspace_write.network_access="true"`, `sandbox_workspace_write.writable_roots="/a"`,
		`sandbox_workspace_write.writable_roots=[1]`} {
		key, _, _ := strings.Cut(override, "=")
		rejects(t, []string{"-c", override}, nil, key)
	}
	// Server args keep the raw value; the native server applies the same parser.
	plan = build(t, []string{"-c", `model='gpt-5'`}, nil)
	if !slices.Equal(plan.ServerArgs, []string{"-c", `model='gpt-5'`}) {
		t.Fatalf("server args %q", plan.ServerArgs)
	}
}
