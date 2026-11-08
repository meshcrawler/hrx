package codexlaunch

import (
	"path/filepath"
	"slices"
)

// Verdict is the outcome for one field.
type Verdict string

const (
	Matched      Verdict = "matched"
	Mismatched   Verdict = "mismatched"
	Unresolved   Verdict = "unresolved"   // intent Unset: left to config layers
	Inapplicable Verdict = "inapplicable" // field does not exist for the native variant
	Unreported   Verdict = "unreported"   // native evidence omits the field
)

// Report compares launch intent with native evidence. It never authorizes
// admission: Status is "blocked" whenever the plan has blockers, which in v1
// is always.
type Report struct {
	Fields                map[string]Verdict
	SecurityFieldsMatched bool // every security field matched or inapplicable
	RouteFieldsMatched    bool
	Blockers              []Blocker
	Status                string // "mismatch" or "blocked"
}

var securityFields = []string{"approval_policy", "approvals_reviewer", "sandbox.mode", "sandbox.network_access",
	"sandbox.writable_roots", "sandbox.exclude_tmpdir_env_var", "sandbox.exclude_slash_tmp", "cwd",
	"runtime_workspace_roots", "active_permission_profile"}

var routeFields = []string{"model", "model_provider", "reasoning_effort"}

var nativeSandboxType = map[string]string{"read-only": "readOnly", "workspace-write": "workspaceWrite",
	"danger-full-access": "dangerFullAccess"}

func compareValue[T comparable](w Want[T], native Optional[T]) Verdict {
	switch {
	case !w.Set:
		return Unresolved
	case !native.Present:
		return Unreported
	case w.Value == native.Value:
		return Matched
	}
	return Mismatched
}

func compareServiceTier(w Want[string], native Optional[string]) Verdict {
	// Pinned ServiceTier::from_request_value accepts fast as priority. Native
	// Config preserves default as an explicit standard-routing sentinel; null
	// is not equivalent (and can also mean a feature-disabled tier).
	if w.Value == "fast" {
		w.Value = "priority"
	}
	return compareValue(w, native)
}

// sameRoots compares lexically cleaned path sets. It does not claim filesystem
// identity: aliases and symlinks that differ lexically are a mismatch.
func sameRoots(a, b []string) bool {
	clean := func(paths []string) []string {
		out := make([]string, len(paths))
		for i, p := range paths {
			out[i] = filepath.Clean(p)
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	return slices.Equal(clean(a), clean(b))
}

func compareRoots(w Want[[]string], native Optional[[]string]) Verdict {
	switch {
	case !w.Set:
		return Unresolved
	case !native.Present:
		return Unreported
	case sameRoots(w.Value, native.Value):
		return Matched
	}
	return Mismatched
}

// compareApproval never claims parity from a kind string alone: a granular
// policy is a structured policy the planner cannot request, so it is mismatched
// against any requested intent.
func compareApproval(w Want[string], native NativeApproval) Verdict {
	switch {
	case !w.Set:
		return Unresolved
	case native.Kind == "granular":
		return Mismatched
	case native.Kind == w.Value:
		return Matched
	}
	return Mismatched
}

// Compare binds native evidence to the identity learned at start and compares
// every field. A binding failure is an error, not a verdict.
func Compare(plan Plan, native Native, binding Identity) (Report, error) {
	if err := native.Identity.Bind(binding); err != nil {
		return Report{}, err
	}
	in := plan.Intent
	present := func(v string) Optional[string] { return Optional[string]{Present: true, Value: v} }
	f := map[string]Verdict{
		"thread_source":    compareValue(Want[string]{Set: plan.Start.ThreadSource != "", Value: plan.Start.ThreadSource}, native.ThreadSource),
		"model":            compareValue(in.Model, present(native.Model)),
		"model_provider":   compareValue(in.ModelProvider, present(native.ModelProvider)),
		"reasoning_effort": compareValue(in.ReasoningEffort, native.ReasoningEffort),
		"service_tier":     compareServiceTier(in.ServiceTier, native.ServiceTier),
		// Pinned ThreadStartResponse has no effective web-search field. Never
		// infer it from the request or an unknown response extension.
		"web_search":         compareValue(in.WebSearch, Optional[string]{}),
		"approvals_reviewer": compareValue(in.ApprovalsReviewer, present(native.ApprovalsReviewer)),
		"approval_policy":    compareApproval(in.ApprovalPolicy, native.Approval),
		"cwd":                compareValue(Want[string]{Set: in.Cwd.Set, Value: filepath.Clean(in.Cwd.Value)}, present(filepath.Clean(native.Cwd))),
		"runtime_workspace_roots": compareRoots(in.RuntimeWorkspaceRoots,
			Optional[[]string]{Present: true, Value: native.RuntimeWorkspaceRoots}),
		// The planner cannot request a permission profile in v1 (blocker).
		"active_permission_profile": Unresolved,
	}
	sandbox := native.Sandbox
	mode := in.Sandbox.Mode
	modeWant := Want[string]{Set: mode.Set, Value: nativeSandboxType[mode.Value]}
	f["sandbox.mode"] = compareValue(modeWant, present(sandbox.Type))
	sub := func(name string, verdict func() Verdict, applicable bool, wanted bool) {
		switch {
		case applicable:
			f[name] = verdict()
		case wanted:
			// A requested field the native variant does not have is a different policy.
			f[name] = Mismatched
		default:
			f[name] = Inapplicable
		}
	}
	readOrWrite := sandbox.Type == "readOnly" || sandbox.Type == "workspaceWrite"
	write := sandbox.Type == "workspaceWrite"
	sub("sandbox.network_access", func() Verdict { return compareValue(in.Sandbox.NetworkAccess, sandbox.NetworkAccess) },
		readOrWrite, in.Sandbox.NetworkAccess.Set)
	sub("sandbox.writable_roots", func() Verdict { return compareRoots(in.Sandbox.WritableRoots, sandbox.WritableRoots) },
		write, in.Sandbox.WritableRoots.Set)
	sub("sandbox.exclude_tmpdir_env_var", func() Verdict {
		return compareValue(in.Sandbox.ExcludeTmpdirEnvVar, sandbox.ExcludeTmpdirEnvVar)
	}, write, in.Sandbox.ExcludeTmpdirEnvVar.Set)
	sub("sandbox.exclude_slash_tmp", func() Verdict { return compareValue(in.Sandbox.ExcludeSlashTmp, sandbox.ExcludeSlashTmp) },
		write, in.Sandbox.ExcludeSlashTmp.Set)
	if sandbox.Type == "externalSandbox" {
		// The planner never requests an external sandbox; its network mode is unrepresented.
		f["sandbox.network_access"] = Mismatched
	}

	all := func(names []string) bool {
		for _, name := range names {
			if v := f[name]; v != Matched && v != Inapplicable {
				return false
			}
		}
		return true
	}
	report := Report{Fields: f, SecurityFieldsMatched: all(securityFields), RouteFieldsMatched: all(routeFields),
		Blockers: plan.Blockers(), Status: "blocked"}
	for _, v := range f {
		if v == Mismatched {
			report.Status = "mismatch"
		}
	}
	return report, nil
}
