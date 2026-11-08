package codexlaunch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
)

// Identity binds evidence to one native thread and session.
type Identity struct{ ThreadID, SessionID string }

// Optional distinguishes a present value from null or absent.
type Optional[T any] struct {
	Present bool
	Value   T
}

// NativeSandbox holds a SandboxPolicy variant. Fields that do not belong to the
// variant are not present (inapplicable), never defaulted.
type NativeSandbox struct {
	Type                string // dangerFullAccess, readOnly, externalSandbox, workspaceWrite
	NetworkAccess       Optional[bool]
	ExternalNetwork     Optional[string] // externalSandbox only: restricted or enabled
	WritableRoots       Optional[[]string]
	ExcludeTmpdirEnvVar Optional[bool]
	ExcludeSlashTmp     Optional[bool]
}

// NativeApproval is an AskForApproval value: untrusted, on-request, never, or
// the structured granular variant (app-server-protocol v2 shared.rs:206-222).
type NativeApproval struct {
	Kind     string
	Granular *GranularApproval // set only when Kind is "granular"
}

// GranularApproval holds the pinned granular fields. skill_approval and
// request_permissions are serde-defaulted to false when absent.
type GranularApproval struct {
	SandboxApproval, Rules, SkillApproval, RequestPermissions, MCPElicitations bool
}

// Native is the effective configuration a native ThreadStartResponse reports
// (app-server-protocol v2 thread.rs:188-217).
type Native struct {
	Identity
	Model, ModelProvider, Cwd, ApprovalsReviewer string
	Approval                                     NativeApproval
	Sandbox                                      NativeSandbox
	ServiceTier, ReasoningEffort                 Optional[string]
	ActivePermissionProfile                      Optional[json.RawMessage]
	CollaborationMode                            Optional[json.RawMessage]
	RuntimeWorkspaceRoots                        []string
	ThreadSource                                 Optional[string]
	DisabledPluginIDs                            []string
}

func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	object := make(map[string]json.RawMessage)
	// Evidence must have one interpretation; map unmarshalling silently keeps
	// the last duplicate identity or permission field, including escaped keys.
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid object key")
		}
		if _, exists := object[key]; exists {
			return nil, fmt.Errorf("duplicate field %q", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		object[key] = value
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return nil, errors.New("invalid object ending")
	}
	// The whole input must be exactly one value. Decoder.More is false at a
	// closing delimiter, so a stray "}" or "]" would slip past it.
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	return object, nil
}

func required[T any](object map[string]json.RawMessage, key string) (T, error) {
	var value T
	raw, ok := object[key]
	if !ok || string(raw) == "null" {
		return value, fmt.Errorf("missing required field %q", key)
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, fmt.Errorf("invalid field %q", key)
	}
	return value, nil
}

func nonEmpty(object map[string]json.RawMessage, key string) (string, error) {
	value, err := required[string](object, key)
	if err == nil && value == "" {
		err = fmt.Errorf("empty field %q", key)
	}
	return value, err
}

// nullable requires the key (Option fields serialize as null) and decodes it.
func nullable[T any](object map[string]json.RawMessage, key string) (Optional[T], error) {
	raw, ok := object[key]
	if !ok {
		return Optional[T]{}, fmt.Errorf("missing field %q", key)
	}
	if string(raw) == "null" {
		return Optional[T]{}, nil
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return Optional[T]{}, fmt.Errorf("invalid field %q", key)
	}
	return Optional[T]{Present: true, Value: value}, nil
}

func parseIdentity(object map[string]json.RawMessage) (Identity, error) {
	raw, ok := object["thread"]
	if !ok {
		return Identity{}, errors.New(`missing required field "thread"`)
	}
	thread, err := decodeObject(raw)
	if err != nil {
		return Identity{}, errors.New("invalid thread object")
	}
	id, err := nonEmpty(thread, "id")
	if err != nil {
		return Identity{}, err
	}
	session, err := nonEmpty(thread, "sessionId")
	if err != nil {
		return Identity{}, err
	}
	return Identity{ThreadID: id, SessionID: session}, nil
}

func absolutePaths(values []string, key string) error {
	for _, value := range values {
		if !filepath.IsAbs(value) {
			return fmt.Errorf("non-absolute path in %q", key)
		}
	}
	return nil
}

// ParseThreadStart strictly parses a thread/start response. Required fields
// must be present with the right JSON type; unknown approval, reviewer or
// sandbox values are errors, never matches. A thread/read payload lacks the
// configuration fields and is rejected.
func ParseThreadStart(raw []byte) (Native, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return Native{}, err
	}
	var n Native
	if n.Identity, err = parseIdentity(object); err != nil {
		return Native{}, err
	}
	thread, err := decodeObject(object["thread"])
	if err != nil {
		return Native{}, err
	}
	if raw, ok := thread["threadSource"]; ok && string(raw) != "null" {
		value, err := nonEmpty(thread, "threadSource")
		if err != nil {
			return Native{}, err
		}
		n.ThreadSource = Optional[string]{Present: true, Value: value}
	}
	for key, target := range map[string]*string{"model": &n.Model, "modelProvider": &n.ModelProvider, "cwd": &n.Cwd} {
		if *target, err = nonEmpty(object, key); err != nil {
			return Native{}, err
		}
	}
	if !filepath.IsAbs(n.Cwd) {
		return Native{}, errors.New(`field "cwd" is not absolute`)
	}
	if n.ApprovalsReviewer, err = required[string](object, "approvalsReviewer"); err != nil {
		return Native{}, err
	}
	if !slices.Contains([]string{"user", "auto_review"}, n.ApprovalsReviewer) {
		return Native{}, errors.New(`unknown "approvalsReviewer"`)
	}
	if n.Approval, err = parseApproval(object["approvalPolicy"]); err != nil {
		return Native{}, err
	}
	if n.Sandbox, err = parseSandbox(object["sandbox"]); err != nil {
		return Native{}, err
	}
	if n.ServiceTier, err = nullable[string](object, "serviceTier"); err != nil {
		return Native{}, err
	}
	if n.ReasoningEffort, err = nullable[string](object, "reasoningEffort"); err != nil {
		return Native{}, err
	}
	// Experimental; it may be filtered out. Absent or null both mean "not reported".
	if raw, ok := object["activePermissionProfile"]; ok && string(raw) != "null" {
		n.ActivePermissionProfile = Optional[json.RawMessage]{Present: true, Value: raw}
	}
	if raw, ok := object["disabledPluginIds"]; ok {
		if json.Unmarshal(raw, &n.DisabledPluginIDs) != nil || n.DisabledPluginIDs == nil {
			return Native{}, errors.New("invalid disabledPluginIds")
		}
	} else {
		n.DisabledPluginIDs = []string{}
	}
	if raw, ok := object["collaborationMode"]; ok && string(raw) != "null" {
		if _, err := decodeObject(raw); err != nil {
			return Native{}, err
		}
		n.CollaborationMode = Optional[json.RawMessage]{Present: true, Value: append(json.RawMessage(nil), raw...)}
	}
	if raw, ok := object["runtimeWorkspaceRoots"]; ok {
		if err := json.Unmarshal(raw, &n.RuntimeWorkspaceRoots); err != nil || n.RuntimeWorkspaceRoots == nil {
			return Native{}, errors.New(`invalid field "runtimeWorkspaceRoots"`)
		}
		if err := absolutePaths(n.RuntimeWorkspaceRoots, "runtimeWorkspaceRoots"); err != nil {
			return Native{}, err
		}
	} else {
		return Native{}, errors.New(`missing field "runtimeWorkspaceRoots"`)
	}
	return n, nil
}

func parseApproval(raw json.RawMessage) (NativeApproval, error) {
	if raw == nil || string(raw) == "null" {
		return NativeApproval{}, errors.New(`missing required field "approvalPolicy"`)
	}
	var kind string
	if json.Unmarshal(raw, &kind) == nil {
		if !slices.Contains([]string{"untrusted", "on-request", "never"}, kind) {
			return NativeApproval{}, errors.New(`unknown "approvalPolicy"`)
		}
		return NativeApproval{Kind: kind}, nil
	}
	object, err := decodeObject(raw)
	body, ok := object["granular"]
	if err != nil || !ok || len(object) != 1 {
		return NativeApproval{}, errors.New(`invalid "approvalPolicy"`)
	}
	fields, err := decodeObject(body)
	if err != nil {
		return NativeApproval{}, errors.New(`invalid granular "approvalPolicy"`)
	}
	flag := func(key string, defaulted bool) (bool, error) {
		value, present := fields[key]
		if !present && defaulted {
			return false, nil
		}
		var b bool
		if !present || string(value) == "null" || json.Unmarshal(value, &b) != nil {
			return false, fmt.Errorf("invalid granular approval field %q", key)
		}
		return b, nil
	}
	known := map[string]bool{"sandbox_approval": false, "rules": false, "mcp_elicitations": false,
		"skill_approval": true, "request_permissions": true}
	for key := range fields {
		if _, ok := known[key]; !ok {
			return NativeApproval{}, fmt.Errorf("unknown granular approval field %q", key)
		}
	}
	var g GranularApproval
	for key, target := range map[string]*bool{"sandbox_approval": &g.SandboxApproval, "rules": &g.Rules,
		"mcp_elicitations": &g.MCPElicitations, "skill_approval": &g.SkillApproval,
		"request_permissions": &g.RequestPermissions} {
		if *target, err = flag(key, known[key]); err != nil {
			return NativeApproval{}, err
		}
	}
	return NativeApproval{Kind: "granular", Granular: &g}, nil
}

func parseSandbox(raw json.RawMessage) (NativeSandbox, error) {
	if raw == nil || string(raw) == "null" {
		return NativeSandbox{}, errors.New(`missing required field "sandbox"`)
	}
	object, err := decodeObject(raw)
	if err != nil {
		return NativeSandbox{}, errors.New(`invalid "sandbox"`)
	}
	var s NativeSandbox
	if s.Type, err = required[string](object, "type"); err != nil {
		return NativeSandbox{}, err
	}
	allowed := map[string][]string{
		"dangerFullAccess": {"type"},
		"readOnly":         {"type", "networkAccess"},
		"externalSandbox":  {"type", "networkAccess"},
		"workspaceWrite":   {"type", "writableRoots", "networkAccess", "excludeTmpdirEnvVar", "excludeSlashTmp"},
	}
	fields, known := allowed[s.Type]
	if !known {
		return NativeSandbox{}, errors.New(`unknown sandbox "type"`)
	}
	for key := range object {
		if !slices.Contains(fields, key) {
			return NativeSandbox{}, fmt.Errorf("unexpected sandbox field %q for %s", key, s.Type)
		}
	}
	present := func(key string) Optional[bool] {
		v, e := required[bool](object, key)
		if e != nil && err == nil {
			err = e
		}
		return Optional[bool]{Present: e == nil, Value: v}
	}
	switch s.Type {
	case "readOnly":
		s.NetworkAccess = present("networkAccess")
	case "externalSandbox":
		mode, e := required[string](object, "networkAccess")
		if e != nil || !slices.Contains([]string{"restricted", "enabled"}, mode) {
			return NativeSandbox{}, errors.New(`invalid externalSandbox "networkAccess"`)
		}
		s.ExternalNetwork = Optional[string]{Present: true, Value: mode}
	case "workspaceWrite":
		roots, e := required[[]string](object, "writableRoots")
		if e != nil {
			return NativeSandbox{}, e
		}
		if e := absolutePaths(roots, "writableRoots"); e != nil {
			return NativeSandbox{}, e
		}
		s.WritableRoots = Optional[[]string]{Present: true, Value: roots}
		s.NetworkAccess = present("networkAccess")
		s.ExcludeTmpdirEnvVar = present("excludeTmpdirEnvVar")
		s.ExcludeSlashTmp = present("excludeSlashTmp")
	}
	if err != nil {
		return NativeSandbox{}, err
	}
	return s, nil
}

// ParseThreadReadIdentity reads only the identity from a thread/read response,
// whose schema differs from thread/start. It is never compared as configuration.
func ParseThreadReadIdentity(raw []byte) (Identity, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return Identity{}, err
	}
	return parseIdentity(object)
}

// ValidateFreshBaseline checks the named empty paginated thread/read evidence
// used by the pinned fresh-creation protocol. It does not prove reader ownership,
// a consumed message prefix, journal durability or permission to release work.
func (n Native) ValidateFreshBaseline(raw []byte, expectedName string) error {
	if expectedName == "" {
		return errors.New("fresh baseline requires an expected name")
	}
	object, err := decodeObject(raw)
	if err != nil {
		return err
	}
	id, err := parseIdentity(object)
	if err != nil {
		return err
	}
	if err := id.Bind(n.Identity); err != nil {
		return err
	}
	thread, err := decodeObject(object["thread"])
	if err != nil {
		return err
	}
	if err := validateDirectInputRoot(thread); err != nil {
		return err
	}
	name, err := nonEmpty(thread, "name")
	if err != nil || name != expectedName {
		return errors.New("fresh baseline name mismatch or missing")
	}
	mode, err := required[string](thread, "historyMode")
	if err != nil || mode != "paginated" {
		return errors.New("fresh baseline requires paginated history")
	}
	turns, err := required[[]json.RawMessage](thread, "turns")
	if err != nil || turns == nil || len(turns) != 0 {
		return errors.New("fresh baseline requires an explicit empty turns array")
	}
	return nil
}

// ErrIdentity reports evidence from a different native thread or session.
var ErrIdentity = errors.New("native evidence is bound to a different thread or session")

// Bind checks evidence against the identity learned from the initial start.
// Both halves are required: an empty expectation is never a wildcard.
func (id Identity) Bind(want Identity) error {
	if want.ThreadID == "" || want.SessionID == "" {
		return errors.New("binding requires an expected thread and session")
	}
	if id != want {
		return ErrIdentity
	}
	return nil
}

// ValidateResumeBaseline establishes a new idle baseline for an existing native
// session. Historical turns are retained, not mistaken for activity on the new
// connection. An active session needs explicit active-turn recovery and is not
// admitted by this idle path. This does not certify coverage before attachment.
func (n Native) ValidateResumeBaseline(raw []byte, expectedName string) error {
	object, err := decodeObject(raw)
	if err != nil {
		return err
	}
	identity, err := parseIdentity(object)
	if err != nil {
		return err
	}
	if err := identity.Bind(n.Identity); err != nil {
		return err
	}
	thread, err := decodeObject(object["thread"])
	if err != nil {
		return err
	}
	if err := validateDirectInputRoot(thread); err != nil {
		return err
	}
	name, err := nonEmpty(thread, "name")
	if err != nil || expectedName == "" || name != expectedName {
		return errors.New("resume baseline name mismatch")
	}
	status, err := decodeObject(thread["status"])
	if err != nil {
		return err
	}
	kind, err := required[string](status, "type")
	if err != nil || kind != "idle" {
		return errors.New("resume baseline requires idle native session")
	}
	mode, err := required[string](thread, "historyMode")
	if err != nil || !slices.Contains([]string{"paginated", "legacy"}, mode) {
		return errors.New("unknown resume history mode")
	}
	turns, err := required[[]json.RawMessage](thread, "turns")
	if err != nil || turns == nil {
		return errors.New("resume baseline missing turns array")
	}
	for _, rawTurn := range turns {
		turn, err := decodeObject(rawTurn)
		if err != nil {
			return err
		}
		if _, err := nonEmpty(turn, "id"); err != nil {
			return err
		}
		status, err := required[string](turn, "status")
		if err != nil || !slices.Contains([]string{"completed", "interrupted", "failed"}, status) {
			return errors.New("resume history contains unresolved turn")
		}
	}
	return nil
}

// Bulletin input belongs only to an interactive root thread, matching the
// launcher's former discovery checks before it binds any bridge endpoint.
func validateDirectInputRoot(thread map[string]json.RawMessage) error {
	accepts, err := required[bool](thread, "canAcceptDirectInput")
	if err != nil || !accepts {
		return errors.New("baseline thread does not accept direct input")
	}
	if parent, ok := thread["parentThreadId"]; ok && !bytes.Equal(bytes.TrimSpace(parent), []byte("null")) {
		return errors.New("baseline thread is not a root thread")
	}
	return nil
}
