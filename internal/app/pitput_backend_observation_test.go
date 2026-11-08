package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

func backendOpenCodeFixtureArgs(owner, resume string) []string {
	args := []string{"--yolo", "--model", "fixture"}
	if resume != "" {
		return append(args, "--resume", resume)
	}
	return append(args, "--pitput", "--pitput-board", targetedBoard, "--pitput-targeted", "--pitput-combined-preview", "--agent-identity", owner)
}

type backendOwnedInstruction struct{ Text, Path, Root string }

func ownedBackendOpenCodeInstruction(l *Launch) (backendOwnedInstruction, error) {
	var raw, cache string
	for _, item := range l.Env {
		if strings.HasPrefix(item, "OPENCODE_CONFIG_CONTENT=") {
			raw = strings.TrimPrefix(item, "OPENCODE_CONFIG_CONTENT=")
		}
		if strings.HasPrefix(item, "XDG_CACHE_HOME=") {
			cache = strings.TrimPrefix(item, "XDG_CACHE_HOME=")
		}
	}
	var config struct{ Instructions []string }
	if json.Unmarshal([]byte(raw), &config) != nil || len(config.Instructions) != 1 || !filepath.IsAbs(cache) {
		return backendOwnedInstruction{}, errors.New("owned primer configuration unavailable")
	}
	path := config.Instructions[0]
	relative, err := filepath.Rel(cache, path)
	if err != nil || !filepath.IsAbs(path) || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) ||
		!strings.HasPrefix(filepath.Base(path), "yesod-instructions-") || !strings.HasSuffix(path, ".md") {
		return backendOwnedInstruction{}, errors.New("owned primer path binding mismatch")
	}
	// Preserve the exact configured path bytes: native instruction updates name
	// those bytes. Canonical aliases (notably Darwin /var) remain valid. Existing
	// private-file/no-follow checks still apply; no ancestor permission policy is added.
	realCache, cacheErr := filepath.EvalSymlinks(cache)
	realPath, pathErr := filepath.EvalSymlinks(path)
	realRelative, relativeErr := filepath.Rel(realCache, realPath)
	if cacheErr != nil || pathErr != nil || relativeErr != nil || realRelative == ".." || strings.HasPrefix(realRelative, ".."+string(os.PathSeparator)) {
		return backendOwnedInstruction{}, errors.New("owned primer path binding mismatch")
	}
	for _, ownedPath := range []string{cache, path} {
		info, err := os.Stat(ownedPath)
		if err != nil {
			return backendOwnedInstruction{}, errors.New("owned primer file unavailable")
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != uint32(os.Getuid()) {
			return backendOwnedInstruction{}, errors.New("owned primer file unavailable")
		}
	}
	data, err := readClaudePrivate(path)
	if err != nil || len(data) == 0 {
		return backendOwnedInstruction{}, errors.New("owned primer file unavailable")
	}
	return backendOwnedInstruction{Text: string(data), Path: path, Root: cache}, nil
}

func ownedBackendOpenCodePrimer(l *Launch) (string, error) {
	value, err := ownedBackendOpenCodeInstruction(l)
	return value.Text, err
}

func backendOpenCodeFailureCode(err error) string {
	for message, code := range map[string]string{
		"attachment must not submit a prompt":         "unbound_attachment_prompt",
		"owned primer configuration unavailable":      "owned_primer_configuration",
		"owned primer path binding mismatch":          "owned_primer_path_binding",
		"owned primer file unavailable":               "owned_primer_file",
		"native attachment binding mismatch":          "attachment_binding",
		"ambiguous native attachment":                 "attachment_ambiguous",
		"native attachment exited during startup":     "attachment_early_exit",
		"native observation session binding mismatch": "observation_binding",
		"native observation shape unavailable":        "observation_shape",
		"native observation read unavailable":         "observation_read",
	} {
		if err != nil && err.Error() == message {
			return code
		}
	}
	return "native_fixture_failure"
}

func validateBackendOpenCodeAttachment(l *Launch, id, endpoint string) error {
	if l.Executable != "opencode" {
		return errors.New("wrong native attachment backend")
	}
	values := map[string]string{}
	for _, token := range nativeArguments("opencode", l.Args) {
		name, value, equal := strings.Cut(l.Args[token.index], "=")
		if !token.option {
			continue
		}
		if name != "--session" && name != "--server" && name != "--prompt" {
			continue
		}
		if _, duplicate := values[name]; duplicate {
			return errors.New("ambiguous native attachment")
		}
		if !equal && token.index+1 < len(l.Args) {
			value = l.Args[token.index+1]
		}
		values[name] = value
	}
	if prompt, found := values["--prompt"]; found {
		owned, err := ownedBackendOpenCodePrimer(l)
		if err != nil {
			return err
		}
		if prompt != owned {
			return errors.New("attachment must not submit a prompt")
		}
	}
	if values["--session"] != id || values["--server"] != endpoint {
		return errors.New("native attachment binding mismatch")
	}
	return nil
}

// Use the exact PTY adapter from TestNativeOpenCodePitputBridge and the
// production-generated argv, including only its exact owned initialization
// primer. No new user/notification prompt, guessed resume flag or queue POST.
func startBackendOpenCodeClient(ctx context.Context, l *Launch, id, endpoint string) (func() bool, func() error, error) {
	if err := validateBackendOpenCodeAttachment(l, id, endpoint); err != nil {
		return nil, nil, err
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		return nil, nil, err
	}
	clientCtx, cancel := context.WithCancel(ctx)
	process, err := startPitputProcess(clientCtx, python, append([]string{"-c", nativeOpenCodePTYScript}, l.Args...), l.Env, l.pitputRuntime.record.Project)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	alive := func() bool {
		select {
		case err := <-process.done:
			process.done <- err
			return false
		default:
			return true
		}
	}
	close := func() error {
		cancel()
		select {
		case <-process.done:
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("native attachment cleanup unverified")
		}
	}
	// Catch immediate argv/auth/startup exits. Continued liveness is checked on
	// every owner observation; only real later native read/ACK proves progress.
	select {
	case err := <-process.done:
		process.done <- err
		cancel()
		return nil, nil, errors.New("native attachment exited during startup")
	case <-time.After(200 * time.Millisecond):
	}
	return alive, close, nil
}

func backendOpenCodeResumeLifecycle(restart func() error, observe func(string) map[string]any, activate func() error) ([]map[string]any, error) {
	if err := restart(); err != nil {
		return nil, err
	}
	before := observe("after_server_restart_before_client")
	if err := activate(); err != nil {
		return []map[string]any{before}, err
	}
	return []map[string]any{before, observe("after_production_client_started")}, nil
}

func combinedOpenCodeMessageID(state []byte) (string, error) {
	var value struct {
		Pending *struct {
			Event struct {
				Key string `json:"event_key"`
			} `json:"event"`
		} `json:"pending"`
	}
	if json.Unmarshal(state, &value) != nil {
		return "", errors.New("invalid combined spool observation")
	}
	if value.Pending == nil {
		return "", nil
	}
	if !regexp.MustCompile(`^delivery-[0-9a-f]{64}$`).MatchString(value.Pending.Event.Key) {
		return "", errors.New("invalid combined pending identity")
	}
	return "msg_" + value.Pending.Event.Key, nil
}

// Pinned @opencode/cli 2.1.2, commit a517687fde98a7329ada3ae644a01a9fc6c4e99e:
// packages/schema/src/session-message.ts User is flat {id,type:"user",text,...};
// prompt.ts makes text a string, with optional files/agents/skills arrays.
// No v1 info/parts fallback or recursive text extraction is accepted.
func backendTargetMessage(entries []any, messageID string) map[string]any {
	result := map[string]any{"status": "unavailable"}
	matches := 0
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok || entry["id"] != messageID {
			continue
		}
		matches++
		if entry["type"] != "user" {
			continue
		}
		text, ok := entry["text"].(string)
		if !ok {
			continue
		}
		textOnly := true
		for _, key := range []string{"files", "agents", "skills"} {
			value, exists := entry[key]
			if !exists {
				continue
			}
			items, ok := value.([]any)
			if !ok || len(items) != 0 {
				textOnly = false
			}
		}
		if !textOnly {
			continue
		}
		digest := sha256.Sum256([]byte(text))
		result = map[string]any{"status": "observed", "text_sha256": hex.EncodeToString(digest[:]), "text_bytes": len([]byte(text))}
	}
	if matches != 1 {
		return map[string]any{"status": "unavailable"}
	}
	return result
}

// Project only metadata from authenticated exact-session reads. Never export
// content/parts/tool output, arbitrary native statuses, credentials or paths.
func backendOpenCodeObservation(id, project, messageID string, read func(string, any) error) (map[string]any, error) {
	return backendOpenCodeProjection(id, project, messageID, "", false, read)
}

func backendOpenCodeFailureObservation(id, project, messageID, callHash string, read func(string, any) error) (map[string]any, error) {
	if callHash != "" && !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(callHash) {
		return nil, errors.New("native observation call identity rejected")
	}
	return backendOpenCodeProjection(id, project, messageID, callHash, true, read)
}

func backendOpenCodeProjection(id, project, messageID, callHash string, detailed bool, read func(string, any) error) (map[string]any, error) {
	var session struct{ Data json.RawMessage }
	var info openCodeSession
	if read("/api/session/"+id, &session) != nil || json.Unmarshal(session.Data, &info) != nil || validateOpenCodeBinding(info, id, project) != nil {
		return nil, errors.New("native observation session binding mismatch")
	}
	result := map[string]any{"session": id, "exact_root_project_binding": true, "target_message_id": messageID}
	if detailed {
		result["persisted_permissions"] = backendSessionPermissions(session.Data)
		result["emitted_call_sha256"] = callHash
	}
	for _, item := range []struct{ name, path string }{{"inbox", "/inbox"}, {"history", "/message?order=desc&limit=16"}} {
		var response struct{ Data json.RawMessage }
		if read("/api/session/"+id+item.path, &response) != nil {
			return nil, errors.New("native observation read unavailable")
		}
		var entries []any
		if json.Unmarshal(response.Data, &entries) != nil || len(entries) > 256 {
			return nil, errors.New("native observation shape unavailable")
		}
		matches := 0
		var targetMetadata []map[string]any
		var inspect func(any)
		inspect = func(value any) {
			switch value := value.(type) {
			case map[string]any:
				for key, child := range value {
					if messageID != "" && (key == "id" || key == "messageID") && child == messageID {
						matches++
						classify := func(field string, allowed ...string) string {
							actual, _ := value[field].(string)
							for _, item := range allowed {
								if actual == item {
									return actual
								}
							}
							return "other_or_absent"
						}
						targetMetadata = append(targetMetadata, map[string]any{
							"role":            classify("role", "user", "assistant", "system", "tool"),
							"status":          classify("status", "pending", "queued", "running", "completed", "interrupted", "cancelled", "failed"),
							"session_matches": value["sessionID"] == id,
						})
					}
					// Parts and content carry private bodies and are not metadata.
					if key != "parts" && key != "content" && key != "text" && key != "body" {
						inspect(child)
					}
				}
			case []any:
				for _, child := range value {
					inspect(child)
				}
			}
		}
		inspect(entries)
		digest := sha256.Sum256(response.Data)
		result[item.name] = map[string]any{"entry_count": len(entries), "target_id_occurrences": matches,
			"target_metadata": targetMetadata, "response_bytes": len(response.Data), "response_sha256": hex.EncodeToString(digest[:])}
		if item.name == "history" {
			result[item.name].(map[string]any)["target_message"] = backendTargetMessage(entries, messageID)
		}
		if detailed {
			result[item.name+"_native_state"] = backendNativeTimeline(entries, callHash)
		}
	}
	return result, nil
}
