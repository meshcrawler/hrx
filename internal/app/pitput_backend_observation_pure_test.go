package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBackendOpenCodeFixtureExplicitPermissionsReachNativeOnFreshAndResume(t *testing.T) {
	for _, resume := range []string{"", "ses_original"} {
		options, err := parseLaunchFor("opencode", backendOpenCodeFixtureArgs("test/combined/agent", resume))
		if err != nil || !options.Yolo {
			t.Fatal("fixture permissions were not explicit", err)
		}
		args, err := sessionArguments("opencode", options)
		if err != nil || !nativeHasOption("opencode", args, "--auto") {
			t.Fatal("fixture permissions did not reach native argv", err)
		}
	}
}

func TestBackendOpenCodeResumeObservesBeforeAndAfterActualClientActivation(t *testing.T) {
	var steps []string
	observe := func(stage string) map[string]any {
		steps = append(steps, stage)
		return map[string]any{"stage": stage}
	}
	restart := func() error { steps = append(steps, "restart"); return nil }
	activate := func() error { steps = append(steps, "activate"); return nil }
	observed, err := backendOpenCodeResumeLifecycle(restart, observe, activate)
	if err != nil || len(observed) != 2 || !reflect.DeepEqual(steps, []string{"restart", "after_server_restart_before_client", "activate", "after_production_client_started"}) {
		t.Fatal("native resume lifecycle changed", steps, err)
	}
	steps = nil
	_, err = backendOpenCodeResumeLifecycle(func() error { return errors.New("binding rejected") }, observe, activate)
	if err == nil || len(steps) != 0 {
		t.Fatal("failed restart reached activation")
	}
	observed, err = backendOpenCodeResumeLifecycle(restart, observe, func() error { return errors.New("attachment rejected") })
	if err == nil || len(observed) != 1 {
		t.Fatal("failed activation claimed completion")
	}
}

func TestBackendOpenCodeAttachmentRequiresExactGeneratedBindingWithoutPrompt(t *testing.T) {
	good := &Launch{Executable: "opencode", Args: []string{"--session", "ses_original", "--server", "http://127.0.0.1:1234", "--model", "hrx/fixture"}}
	if err := validateBackendOpenCodeAttachment(good, "ses_original", "http://127.0.0.1:1234"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--session", "ses_other", "--server", "http://127.0.0.1:1234"},
		{"--session", "ses_original", "--server", "http://127.0.0.1:9999"},
		append(append([]string{}, good.Args...), "--prompt", "new input forbidden"),
		append(append([]string{}, good.Args...), "--session", "ses_other"),
		{"--session", "ses_original"},
	} {
		if validateBackendOpenCodeAttachment(&Launch{Executable: "opencode", Args: args}, "ses_original", "http://127.0.0.1:1234") == nil {
			t.Fatal("unsafe attachment accepted")
		}
	}
}

func TestBackendOpenCodeAttachmentPreservesOnlyActualGeneratedOwnedPrimer(t *testing.T) {
	cache := t.TempDir()
	l := &Launch{Executable: "opencode", Args: []string{"--session", "ses_original", "--server", "http://127.0.0.1:1234"},
		Env: []string{"XDG_CACHE_HOME=" + cache, "OPENCODE_CONFIG_CONTENT={}"}}
	if err := injectYesod(Settings{CachePath: filepath.Join(cache, "fixture.json")}, l, "exact owned primer"); err != nil {
		t.Fatal(err)
	}
	before := append([]string{}, l.Args...)
	if err := validateBackendOpenCodeAttachment(l, "ses_original", "http://127.0.0.1:1234"); err != nil || !reflect.DeepEqual(before, l.Args) {
		t.Fatal("production owned initialization changed", err)
	}
	duplicate := *l
	duplicate.Args = append(append([]string{}, l.Args...), "--prompt", "exact owned primer")
	if validateBackendOpenCodeAttachment(&duplicate, "ses_original", "http://127.0.0.1:1234") == nil {
		t.Fatal("duplicate initialization accepted")
	}
	for index, value := range l.Args {
		if value == "--prompt" {
			l.Args[index+1] = "user or modified primer"
		}
	}
	if validateBackendOpenCodeAttachment(l, "ses_original", "http://127.0.0.1:1234") == nil {
		t.Fatal("modified primer accepted")
	}
	outside := filepath.Join(t.TempDir(), "yesod-instructions-foreign.md")
	if err := os.WriteFile(outside, []byte("exact owned primer"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"instructions": []string{outside}})
	l.Env = []string{"XDG_CACHE_HOME=" + cache, "OPENCODE_CONFIG_CONTENT=" + string(raw)}
	if _, err := ownedBackendOpenCodePrimer(l); err == nil {
		t.Fatal("foreign instruction path accepted")
	}
	if backendOpenCodeFailureCode(errors.New("private body credential")) != "native_fixture_failure" {
		t.Fatal("startup classification exported arbitrary error")
	}
}

func TestBackendOpenCodeObservationIsReadOnlyBoundAndBodyFree(t *testing.T) {
	project, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := "ses_original"
	message := "msg_delivery-" + strings.Repeat("a", 64)
	var paths []string
	read := func(path string, result any) error {
		paths = append(paths, path)
		value := any(map[string]any{"data": []any{map[string]any{"info": map[string]any{"id": message}, "parts": []any{map[string]any{"text": "private body credentials"}}}}})
		if path == "/api/session/"+id {
			value = map[string]any{"data": map[string]any{"id": id, "location": map[string]any{"directory": project}}}
		}
		raw, _ := json.Marshal(value)
		return json.Unmarshal(raw, result)
	}
	observation, err := backendOpenCodeObservation(id, project, message, read)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{"/api/session/" + id, "/api/session/" + id + "/inbox", "/api/session/" + id + "/message?order=desc&limit=16"}) {
		t.Fatal("observation changed native operation", paths)
	}
	raw, _ := json.Marshal(observation)
	if strings.Contains(string(raw), "private") || observation["inbox"].(map[string]any)["target_id_occurrences"] != 1 {
		t.Fatal("native observation leaked or lost exact identity")
	}
	paths = nil
	_, err = backendOpenCodeObservation("ses_foreign", project, message, read)
	if err == nil || len(paths) != 1 {
		t.Fatal("foreign session reached inbox/history")
	}
}

func TestCombinedOpenCodeMessageIdentityComesOnlyFromRetainedPendingSlot(t *testing.T) {
	key := "delivery-" + strings.Repeat("a", 64)
	id, err := combinedOpenCodeMessageID([]byte(`{"pending":{"event":{"event_key":"` + key + `"}}}`))
	if err != nil || id != "msg_"+key {
		t.Fatal("retained native identity changed", err)
	}
	for _, raw := range []string{`{"pending":{"event":{"event_key":"foreign"}}}`, `invalid`} {
		if _, err := combinedOpenCodeMessageID([]byte(raw)); err == nil {
			t.Fatal("unknown identity accepted")
		}
	}
}

func TestBackendOpenCodeTargetHistoryProofRequiresPinnedFlatUserText(t *testing.T) {
	// Same flat User shape as v2.1.2 packages/schema/src/session-message.ts.
	id := "msg_delivery-" + strings.Repeat("a", 64)
	entry := map[string]any{"id": id, "type": "user", "text": "private notification", "files": []any{}}
	proof := backendTargetMessage([]any{entry}, id)
	if proof["status"] != "observed" || proof["text_sha256"] == "" {
		t.Fatal("exact message proof unavailable")
	}
	raw, _ := json.Marshal(proof)
	if strings.Contains(string(raw), "private") {
		t.Fatal("native proof exported content")
	}
	for _, entries := range [][]any{
		{}, {entry, entry},
		{map[string]any{"id": "foreign", "type": "user", "text": "private"}},
		{map[string]any{"id": id, "type": "assistant", "text": "private"}},
		{map[string]any{"id": id, "type": "user", "text": 123}},
		{map[string]any{"id": id, "type": "user", "text": "private", "files": []any{map[string]any{"data": "private"}}}},
		{map[string]any{"id": id, "type": "user", "text": "private", "skills": "malformed"}},
		{map[string]any{"info": map[string]any{"id": id}, "parts": []any{map[string]any{"type": "text", "text": "private"}}}},
	} {
		result := backendTargetMessage(entries, id)
		if !reflect.DeepEqual(result, map[string]any{"status": "unavailable"}) {
			t.Fatal("unproven history accepted", result)
		}
	}
}

func TestBackendOpenCodeOwnedInstructionMetadataPreservesConfiguredAliasAndPrivateFile(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "cache-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	l := &Launch{Executable: "opencode", Env: []string{"XDG_CACHE_HOME=" + alias, "OPENCODE_CONFIG_CONTENT={}"}}
	if err := injectYesod(Settings{CachePath: filepath.Join(alias, "fixture.json")}, l, "canonical & <primer>"); err != nil {
		t.Fatal(err)
	}
	value, err := ownedBackendOpenCodeInstruction(l)
	if err != nil || value.Text != "canonical & <primer>" || value.Root != alias || filepath.Dir(value.Path) != alias {
		t.Fatal("metadata lost exact configured owned path", err)
	}
	// Root mode tightening is outside this fixture's contract; the owned file
	// must still remain private. Canonical aliases must not be rewritten.
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := ownedBackendOpenCodeInstruction(l); err != nil {
		t.Fatal("new ancestor mode policy", err)
	}
	if err := os.Chmod(value.Path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ownedBackendOpenCodeInstruction(l); err == nil {
		t.Fatal("public instruction file accepted")
	}
	if err := os.Chmod(value.Path, 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("canonical & <primer>"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(value.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, value.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := ownedBackendOpenCodeInstruction(l); err == nil {
		t.Fatal("outside instruction target accepted")
	}
}
