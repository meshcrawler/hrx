package codexlaunch

import (
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"
)

var bound = Identity{ThreadID: "T1", SessionID: "S1"}

func TestServiceTierParityUsesPinnedRequestNormalization(t *testing.T) {
	for _, tc := range []struct {
		requested string
		returned  any
		want      Verdict
	}{
		{"fast", "priority", Matched},
		{"priority", "priority", Matched},
		{"fast", "fast", Mismatched},
		{"fast", nil, Unreported},
		{"flex", "flex", Matched},
		{"default", "default", Matched},
		{"default", nil, Unreported},
		{"flex", "priority", Mismatched},
	} {
		plan := build(t, nil, []string{"-c", "service_tier=" + tc.requested})
		report, err := Compare(plan, parse(t, response(map[string]any{"serviceTier": tc.returned})), bound)
		if err != nil || report.Fields["service_tier"] != tc.want {
			t.Fatalf("requested=%s returned=%v: %+v (%v)", tc.requested, tc.returned, report, err)
		}
		if report.Status != "blocked" && tc.want == Matched {
			t.Fatal("tier match removed unrelated admission blockers")
		}
	}
}

func TestSearchParityRequiresNativeEvidence(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want Verdict
	}{
		{nil, Unresolved},
		{[]string{"--search"}, Unreported},
		{[]string{"-c", `web_search="cached"`}, Unreported},
		{[]string{"-c", `web_search="disabled"`}, Unreported},
	} {
		plan := build(t, nil, tc.args)
		// An unknown extension/echo is not evidence defined by the pinned schema.
		for _, extra := range []map[string]any{nil, {"webSearch": "live"}} {
			report, err := Compare(plan, parse(t, response(extra)), bound)
			if err != nil || report.Fields["web_search"] != tc.want || report.Status != "blocked" {
				t.Fatalf("args %q: verdict=%s status=%s err=%v", tc.args, report.Fields["web_search"], report.Status, err)
			}
		}
	}
}

func TestRemoteWorkspaceRootsRemainUnresolved(t *testing.T) {
	plan := build(t, nil, nil)
	if plan.Intent.RuntimeWorkspaceRoots.Set {
		t.Fatal("remote planner invented expected runtime roots")
	}
	for _, roots := range [][]string{{}, {"/server/workspace"}} {
		n := parse(t, response(map[string]any{"runtimeWorkspaceRoots": roots}))
		report, err := Compare(plan, n, bound)
		if err != nil {
			t.Fatal(err)
		}
		if report.Fields["runtime_workspace_roots"] != Unresolved || report.Status != "blocked" || report.SecurityFieldsMatched {
			t.Fatalf("server roots must remain uncertified: %+v", report)
		}
		found := false
		for _, blocker := range report.Blockers {
			found = found || blocker == Blocker("unresolved:runtime_workspace_roots")
		}
		if !found {
			t.Fatal("missing runtime roots admission prerequisite")
		}
	}
}

func TestFreshThreadSourceMatchesPinnedTUI(t *testing.T) {
	plan := build(t, nil, nil)
	raw, err := json.Marshal(plan.Start)
	if err != nil {
		t.Fatal(err)
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(raw, &params) != nil || string(params["threadSource"]) != `"user"` {
		t.Fatal("fresh startup omitted native TUI user source")
	}
	if _, exists := params["sessionStartSource"]; exists {
		t.Fatal("fresh startup invented a session source override")
	}
	for _, source := range []string{"user", "subagent", ""} {
		thread := map[string]any{"id": "T1", "sessionId": "S1"}
		if source != "" {
			thread["threadSource"] = source
		}
		n := parse(t, response(map[string]any{"thread": thread}))
		report, err := Compare(plan, n, bound)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]Verdict{"user": Matched, "subagent": Mismatched, "": Unreported}[source]
		if report.Fields["thread_source"] != want {
			t.Fatal(source, report)
		}
	}
}

func TestFreshBaselineRequiresExactNamedEmptySession(t *testing.T) {
	n := parse(t, response(nil))
	const good = `{"thread":{"id":"T1","sessionId":"S1","name":"fixture","canAcceptDirectInput":true,"parentThreadId":null,"historyMode":"paginated","turns":[]}}`
	if err := n.ValidateFreshBaseline([]byte(good), "fixture"); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"wrong thread":       strings.Replace(good, `"T1"`, `"T2"`, 1),
		"wrong session":      strings.Replace(good, `"S1"`, `"S2"`, 1),
		"wrong name":         strings.Replace(good, `"fixture"`, `"other"`, 1),
		"null name":          strings.Replace(good, `"fixture"`, `null`, 1),
		"legacy history":     strings.Replace(good, `"paginated"`, `"legacy"`, 1),
		"missing turns":      strings.Replace(good, `,"turns":[]`, ``, 1),
		"null turns":         strings.Replace(good, `"turns":[]`, `"turns":null`, 1),
		"object turns":       strings.Replace(good, `"turns":[]`, `"turns":{}`, 1),
		"nonempty turns":     strings.Replace(good, `"turns":[]`, `"turns":[{}]`, 1),
		"duplicate turns":    strings.Replace(good, `"turns":[]`, `"turns":[{}],"turns":[]`, 1),
		"duplicate identity": strings.Replace(good, `"id":"T1"`, `"id":"T2","id":"T1"`, 1),
		"trailing data":      good + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := n.ValidateFreshBaseline([]byte(raw), "fixture"); err == nil {
				t.Fatal("invalid fresh baseline accepted")
			}
		})
	}
	if err := n.ValidateFreshBaseline([]byte(good), ""); err == nil {
		t.Fatal("empty expected name treated as wildcard")
	}
	if err := (Native{}).ValidateFreshBaseline([]byte(good), "fixture"); err == nil {
		t.Fatal("unbound start identity accepted")
	}
}

func TestStrictParseRejectsDuplicateEvidence(t *testing.T) {
	valid := string(response(nil))
	for name, raw := range map[string]string{
		"model":            strings.Replace(valid, `"model":"route-a"`, `"model":"wrong","model":"route-a"`, 1),
		"escaped approval": strings.Replace(valid, `"approvalPolicy":"never"`, `"approvalPolicy":"untrusted","approval\u0050olicy":"never"`, 1),
		"thread identity":  strings.Replace(valid, `"id":"T1"`, `"id":"other","id":"T1"`, 1),
		"session identity": strings.Replace(valid, `"sessionId":"S1"`, `"sessionId":"other","sessionId":"S1"`, 1),
		"sandbox":          strings.Replace(valid, `"type":"dangerFullAccess"`, `"type":"readOnly","type":"dangerFullAccess"`, 1),
		"granular":         strings.Replace(valid, `"approvalPolicy":"never"`, `"approvalPolicy":{"granular":{"rules":true,"rules":false,"sandbox_approval":false,"mcp_elicitations":false}}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if raw == valid {
				t.Fatal("fixture did not introduce duplicate")
			}
			if _, err := ParseThreadStart([]byte(raw)); err == nil {
				t.Fatal("ambiguous evidence accepted")
			}
		})
	}
	for _, raw := range []string{
		`{"thread":{"id":"wrong","id":"T1","sessionId":"S1"}}`,
		`{"thread":{"id":"T1","sessionId":"wrong","sessionId":"S1"}}`,
		`{"thread":{"id":"wrong","sessionId":"S1"},"thread":{"id":"T1","sessionId":"S1"}}`,
	} {
		if _, err := ParseThreadReadIdentity([]byte(raw)); err == nil {
			t.Fatal("ambiguous baseline identity accepted")
		}
	}
}

// A ThreadStartResponse shaped like the pinned v2 serialization.
func response(overrides map[string]any) []byte {
	r := map[string]any{
		"thread":                map[string]any{"id": "T1", "sessionId": "S1", "name": nil},
		"model":                 "route-a",
		"modelProvider":         "hrx",
		"serviceTier":           nil,
		"cwd":                   project,
		"runtimeWorkspaceRoots": []string{},
		"instructionSources":    []string{},
		"approvalPolicy":        "never",
		"approvalsReviewer":     "user",
		"sandbox":               map[string]any{"type": "dangerFullAccess"},
		"reasoningEffort":       "high",
	}
	maps.Copy(r, overrides)
	for key, value := range overrides {
		if value == "<absent>" {
			delete(r, key)
		}
	}
	raw, _ := json.Marshal(r)
	return raw
}

func parse(t *testing.T, raw []byte) Native {
	t.Helper()
	n, err := ParseThreadStart(raw)
	if err != nil {
		t.Fatalf("ParseThreadStart: %v", err)
	}
	return n
}

func workspace(network bool, roots []string, tmpdir, slashTmp bool) map[string]any {
	return map[string]any{"type": "workspaceWrite", "networkAccess": network, "writableRoots": roots,
		"excludeTmpdirEnvVar": tmpdir, "excludeSlashTmp": slashTmp}
}

func TestStrictParseRejectsMalformedEvidence(t *testing.T) {
	cases := map[string]map[string]any{
		"missing model":           {"model": "<absent>"},
		"null model":              {"model": nil},
		"empty provider":          {"modelProvider": ""},
		"relative cwd":            {"cwd": "rel"},
		"unknown approval":        {"approvalPolicy": "on-failure"},
		"null approval":           {"approvalPolicy": nil},
		"unknown reviewer":        {"approvalsReviewer": "robot"},
		"missing service tier":    {"serviceTier": "<absent>"},
		"missing roots":           {"runtimeWorkspaceRoots": "<absent>"},
		"null roots":              {"runtimeWorkspaceRoots": nil},
		"unknown sandbox type":    {"sandbox": map[string]any{"type": "mystery"}},
		"null sandbox":            {"sandbox": nil},
		"readOnly missing net":    {"sandbox": map[string]any{"type": "readOnly"}},
		"readOnly string net":     {"sandbox": map[string]any{"type": "readOnly", "networkAccess": "yes"}},
		"danger with roots":       {"sandbox": map[string]any{"type": "dangerFullAccess", "writableRoots": []string{}}},
		"workspace missing tmp":   {"sandbox": map[string]any{"type": "workspaceWrite", "networkAccess": false, "writableRoots": []string{}, "excludeSlashTmp": false}},
		"workspace relative root": {"sandbox": workspace(false, []string{"rel"}, false, false)},
		"external bad network":    {"sandbox": map[string]any{"type": "externalSandbox", "networkAccess": true}},
		"thread without session":  {"thread": map[string]any{"id": "T1"}},
		"empty thread id":         {"thread": map[string]any{"id": "", "sessionId": "S1"}},
	}
	for name, override := range cases {
		if _, err := ParseThreadStart(response(override)); err == nil {
			t.Errorf("%s: accepted malformed evidence", name)
		}
	}
	for _, raw := range []string{"[]", "null", `{"thread":{"id":"T1","sessionId":"S1"}} {}`} {
		if _, err := ParseThreadStart([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	// A thread/read response has a different schema: identity only, never configuration.
	read := []byte(`{"thread":{"id":"T1","sessionId":"S1","turns":[]},"readState":null}`)
	if _, err := ParseThreadStart(read); err == nil {
		t.Error("thread/read payload accepted as thread/start evidence")
	}
	id, err := ParseThreadReadIdentity(read)
	if err != nil || id != bound {
		t.Fatalf("thread/read identity %v %v", id, err)
	}
}

func TestParseKeepsVariantFieldsAndNulls(t *testing.T) {
	n := parse(t, response(map[string]any{"sandbox": workspace(true, []string{"/a"}, false, true),
		"approvalPolicy": map[string]any{"granular": map[string]any{"sandbox_approval": true, "rules": true,
			"mcp_elicitations": false}}, "reasoningEffort": nil}))
	if n.Sandbox.Type != "workspaceWrite" || !n.Sandbox.NetworkAccess.Value || !n.Sandbox.ExcludeSlashTmp.Value ||
		n.Sandbox.ExcludeTmpdirEnvVar.Value || n.Sandbox.WritableRoots.Value[0] != "/a" {
		t.Fatalf("sandbox %+v", n.Sandbox)
	}
	if n.Approval.Kind != "granular" || n.ReasoningEffort.Present || n.ServiceTier.Present {
		t.Fatalf("approval/optional parse %+v", n)
	}
	danger := parse(t, response(nil)).Sandbox
	if danger.NetworkAccess.Present || danger.WritableRoots.Present {
		t.Fatal("dangerFullAccess has no subfields; they must not be defaulted")
	}
}

func TestBindingRequiresTheLearnedPair(t *testing.T) {
	n := parse(t, response(nil))
	if err := n.Bind(bound); err != nil {
		t.Fatal(err)
	}
	for _, wrong := range []Identity{{"T1", "S2"}, {"T2", "S1"}} {
		if !errors.Is(n.Bind(wrong), ErrIdentity) {
			t.Fatalf("binding %v accepted", wrong)
		}
	}
	for _, empty := range []Identity{{}, {"T1", ""}, {"", "S1"}} {
		if err := n.Bind(empty); err == nil || errors.Is(err, ErrIdentity) {
			t.Fatalf("empty expectation %v must be refused, not treated as a wildcard", empty)
		}
	}
	if _, err := Compare(build(t, hrxArgs, nil), n, Identity{"T1", "other"}); !errors.Is(err, ErrIdentity) {
		t.Fatal("Compare must refuse evidence from another session")
	}
}

func TestComparisonNeverAuthorizesAdmission(t *testing.T) {
	plan := build(t, hrxArgs, []string{"--yolo"})
	report, err := Compare(plan, parse(t, response(nil)), bound)
	if err != nil {
		t.Fatal(err)
	}
	if !report.SecurityFieldsMatched && report.Fields["active_permission_profile"] != Unresolved {
		t.Fatalf("unexpected security verdicts %v", report.Fields)
	}
	if report.SecurityFieldsMatched {
		t.Fatal("permission profile is unresolved in v1, so security cannot be fully matched")
	}
	if report.Status != "blocked" || len(report.Blockers) == 0 {
		t.Fatalf("status %q, blockers %v: admission must stay blocked", report.Status, report.Blockers)
	}
	for _, field := range []string{"model", "model_provider", "reasoning_effort", "approval_policy", "sandbox.mode", "cwd"} {
		if report.Fields[field] != Matched {
			t.Fatalf("%s = %s, want matched", field, report.Fields[field])
		}
	}
	for _, field := range []string{"sandbox.network_access", "sandbox.writable_roots", "sandbox.exclude_slash_tmp"} {
		if report.Fields[field] != Inapplicable {
			t.Fatalf("%s = %s, want inapplicable for dangerFullAccess", field, report.Fields[field])
		}
	}
	if report.Fields["approvals_reviewer"] != Unresolved {
		t.Fatal("an unrequested reviewer must be unresolved, never matched against the server")
	}
}

func TestSameSandboxTypeDifferentPolicyMismatches(t *testing.T) {
	configured := []string{"-c", `sandbox_mode="workspace-write"`, "-c", `sandbox_workspace_write.writable_roots=["/a"]`,
		"-c", "sandbox_workspace_write.network_access=false", "-c", "sandbox_workspace_write.exclude_tmpdir_env_var=false",
		"-c", "sandbox_workspace_write.exclude_slash_tmp=false"}
	plan := build(t, append(clone(hrxArgs), configured...), nil)
	same := parse(t, response(map[string]any{"sandbox": workspace(false, []string{"/a"}, false, false)}))
	report, _ := Compare(plan, same, bound)
	for _, field := range []string{"sandbox.mode", "sandbox.network_access", "sandbox.writable_roots",
		"sandbox.exclude_tmpdir_env_var", "sandbox.exclude_slash_tmp"} {
		if report.Fields[field] != Matched {
			t.Fatalf("baseline %s = %s", field, report.Fields[field])
		}
	}
	variants := map[string]map[string]any{
		"sandbox.network_access":         workspace(true, []string{"/a"}, false, false),
		"sandbox.writable_roots":         workspace(false, []string{"/a", "/extra"}, false, false),
		"sandbox.exclude_tmpdir_env_var": workspace(false, []string{"/a"}, true, false),
		"sandbox.exclude_slash_tmp":      workspace(false, []string{"/a"}, false, true),
	}
	for field, sandbox := range variants {
		report, _ := Compare(plan, parse(t, response(map[string]any{"sandbox": sandbox})), bound)
		if report.Fields[field] != Mismatched || report.Status != "mismatch" {
			t.Fatalf("%s: verdict %s status %s, want mismatch", field, report.Fields[field], report.Status)
		}
	}
	// Roots compare lexically only: an alias spelling is a mismatch, not a match.
	alias := parse(t, response(map[string]any{"sandbox": workspace(false, []string{"/private/a"}, false, false)}))
	if report, _ := Compare(plan, alias, bound); report.Fields["sandbox.writable_roots"] != Mismatched {
		t.Fatal("a different spelling of a root must not match without filesystem evidence")
	}
	// Requested workspace fields against a readOnly or external sandbox are a different policy.
	readOnly := parse(t, response(map[string]any{"sandbox": map[string]any{"type": "readOnly", "networkAccess": false}}))
	if report, _ := Compare(plan, readOnly, bound); report.Fields["sandbox.mode"] != Mismatched ||
		report.Fields["sandbox.writable_roots"] != Mismatched {
		t.Fatalf("readOnly verdicts %v", report.Fields)
	}
	external := parse(t, response(map[string]any{"sandbox": map[string]any{"type": "externalSandbox", "networkAccess": "enabled"}}))
	if report, _ := Compare(plan, external, bound); report.Fields["sandbox.network_access"] != Mismatched {
		t.Fatal("external sandbox network must never match")
	}
}

func TestUnsetIntentIsUnresolvedNeverMatched(t *testing.T) {
	plan := build(t, hrxArgs, nil) // no permission flags: left to config layers
	report, err := Compare(plan, parse(t, response(map[string]any{"sandbox": workspace(false, []string{}, false, false),
		"approvalPolicy": "on-request"})), bound)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"approval_policy", "approvals_reviewer", "sandbox.mode", "sandbox.network_access",
		"sandbox.writable_roots", "sandbox.exclude_tmpdir_env_var", "sandbox.exclude_slash_tmp"} {
		if report.Fields[field] != Unresolved {
			t.Fatalf("%s = %s, want unresolved", field, report.Fields[field])
		}
	}
	if report.SecurityFieldsMatched || report.Status != "blocked" {
		t.Fatalf("unresolved security must block: %+v", report)
	}
	noEffort := parse(t, response(map[string]any{"reasoningEffort": nil}))
	if report, _ := Compare(plan, noEffort, bound); report.Fields["reasoning_effort"] != Unreported || report.RouteFieldsMatched {
		t.Fatal("requested effort that the native side does not report must be unreported, not matched")
	}
	wrongModel := parse(t, response(map[string]any{"model": "other"}))
	if report, _ := Compare(plan, wrongModel, bound); report.Fields["model"] != Mismatched || report.Status != "mismatch" {
		t.Fatal("route mismatch must be reported")
	}
	if !strings.Contains(string(response(nil)), `"serviceTier":null`) {
		t.Fatal("fixture should serialize Option fields as null, like the pinned server")
	}
}

func clone(values []string) []string { return append([]string(nil), values...) }

func TestGranularApprovalIsStrictlyParsed(t *testing.T) {
	for name, approval := range map[string]any{
		"null granular":         map[string]any{"granular": nil},
		"bool granular":         map[string]any{"granular": true},
		"string granular":       map[string]any{"granular": "rules"},
		"empty granular":        map[string]any{"granular": map[string]any{}},
		"missing rules":         map[string]any{"granular": map[string]any{"sandbox_approval": true, "mcp_elicitations": true}},
		"null required field":   map[string]any{"granular": map[string]any{"sandbox_approval": nil, "rules": true, "mcp_elicitations": true}},
		"string field":          map[string]any{"granular": map[string]any{"sandbox_approval": "yes", "rules": true, "mcp_elicitations": true}},
		"null defaulted field":  map[string]any{"granular": map[string]any{"sandbox_approval": true, "rules": true, "mcp_elicitations": true, "skill_approval": nil}},
		"unknown field":         map[string]any{"granular": map[string]any{"sandbox_approval": true, "rules": true, "mcp_elicitations": true, "extra": true}},
		"camelCase field names": map[string]any{"granular": map[string]any{"sandboxApproval": true, "rules": true, "mcpElicitations": true}},
		"extra variant key":     map[string]any{"granular": map[string]any{"sandbox_approval": true, "rules": true, "mcp_elicitations": true}, "never": true},
	} {
		if _, err := ParseThreadStart(response(map[string]any{"approvalPolicy": approval})); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Pinned defaults: skill_approval and request_permissions may be absent (shared.rs:215-218).
	n := parse(t, response(map[string]any{"approvalPolicy": map[string]any{"granular": map[string]any{
		"sandbox_approval": true, "rules": false, "mcp_elicitations": true}}}))
	if n.Approval.Kind != "granular" || n.Approval.Granular == nil ||
		*n.Approval.Granular != (GranularApproval{SandboxApproval: true, MCPElicitations: true}) {
		t.Fatalf("granular %+v", n.Approval)
	}
	full := parse(t, response(map[string]any{"approvalPolicy": map[string]any{"granular": map[string]any{
		"sandbox_approval": false, "rules": true, "skill_approval": true, "request_permissions": true,
		"mcp_elicitations": false}}}))
	if *full.Approval.Granular != (GranularApproval{Rules: true, SkillApproval: true, RequestPermissions: true}) {
		t.Fatalf("granular %+v", full.Approval.Granular)
	}
	// No kind-only parity: a valid granular policy never matches a requested intent.
	plan := build(t, hrxArgs, []string{"--yolo"})
	if report, _ := Compare(plan, full, bound); report.Fields["approval_policy"] != Mismatched {
		t.Fatalf("granular approval verdict %s", report.Fields["approval_policy"])
	}
}

func TestTrailingDataAfterTheDocumentIsRejected(t *testing.T) {
	valid := response(nil)
	if _, err := ParseThreadStart(valid); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"}", "]", " }", "\n]", "{}", "[]", "null", "1", "\"x\"", "garbage"} {
		suffix = strings.ReplaceAll(suffix, "\\n", "\n")
		if _, err := ParseThreadStart(append(append([]byte{}, valid...), []byte(suffix)...)); err == nil {
			t.Errorf("accepted trailing %q", suffix)
		}
		read := []byte(`{"thread":{"id":"T1","sessionId":"S1"}}` + suffix)
		if _, err := ParseThreadReadIdentity(read); err == nil {
			t.Errorf("thread/read identity accepted trailing %q", suffix)
		}
	}
	for _, ok := range []string{" ", "\n", "\t\n  "} {
		ok = strings.NewReplacer("\\n", "\n", "\\t", "\t").Replace(ok)
		if _, err := ParseThreadStart(append(append([]byte{}, valid...), []byte(ok)...)); err != nil {
			t.Errorf("trailing whitespace %q rejected: %v", ok, err)
		}
	}
}

func TestResumeBaselineRetainsHistoryButRequiresIdleIdentity(t *testing.T) {
	n := parse(t, response(nil))
	const good = `{"thread":{"id":"T1","sessionId":"S1","name":"fixture","canAcceptDirectInput":true,"parentThreadId":null,"historyMode":"paginated","status":{"type":"idle"},"turns":[{"id":"old","status":"completed"}]}}`
	if err := n.ValidateResumeBaseline([]byte(good), "fixture"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(good, `"idle"`, `"active"`, 1),
		strings.Replace(good, `"T1"`, `"other"`, 1),
		strings.Replace(good, `"S1"`, `"other"`, 1),
		strings.Replace(good, `"completed"`, `"inProgress"`, 1),
		strings.Replace(good, `"status":{"type":"idle"}`, `"status":null`, 1),
		strings.Replace(good, `"paginated"`, `"unknown"`, 1),
	} {
		if err := n.ValidateResumeBaseline([]byte(bad), "fixture"); err == nil {
			t.Fatal("invalid resume baseline accepted", bad)
		}
	}
}

func TestBaselineRequiresDirectInputRoot(t *testing.T) {
	n := parse(t, response(nil))
	for _, resume := range []bool{false, true} {
		baseline := `{"thread":{"id":"T1","sessionId":"S1","name":"fixture","canAcceptDirectInput":true,"parentThreadId":null,"historyMode":"paginated","status":{"type":"idle"},"turns":[]}}`
		validate := n.ValidateFreshBaseline
		if resume {
			validate = n.ValidateResumeBaseline
		}
		if err := validate([]byte(baseline), "fixture"); err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{
			strings.Replace(baseline, `"canAcceptDirectInput":true,`, "", 1),
			strings.Replace(baseline, `"canAcceptDirectInput":true`, `"canAcceptDirectInput":false`, 1),
			strings.Replace(baseline, `"canAcceptDirectInput":true`, `"canAcceptDirectInput":null`, 1),
			strings.Replace(baseline, `"parentThreadId":null`, `"parentThreadId":"parent"`, 1),
			strings.Replace(baseline, `"parentThreadId":null`, `"parentThreadId":false`, 1),
		} {
			if err := validate([]byte(bad), "fixture"); err == nil {
				t.Fatal("invalid input target admitted", resume, bad)
			}
		}
	}
}
