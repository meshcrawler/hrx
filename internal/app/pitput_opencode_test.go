package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/meshcrawler/hrx/internal/session"
)

func TestOpenCodeBridgeRejectsNoninteractive(t *testing.T) {
	err := preparePitputRuntime(context.Background(), LaunchOptions{Pitput: true, Extra: []string{"run", "task"}}, &Launch{Executable: "opencode"})
	if err == nil || !strings.Contains(err.Error(), "interactive OpenCode") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOpenCodeBridgeBinding(t *testing.T) {
	project, _ := filepath.EvalSymlinks(t.TempDir())
	info := openCodeSession{ID: "ses_fixture"}
	info.Location.Directory = project
	if err := validateOpenCodeBinding(info, info.ID, project); err != nil {
		t.Fatal(err)
	}
	info.ParentID = "ses_parent"
	if validateOpenCodeBinding(info, info.ID, project) == nil {
		t.Fatal("accepted child session")
	}
	info.ParentID = ""
	archived := float64(1)
	info.Time.Archived = &archived
	if validateOpenCodeBinding(info, info.ID, project) == nil {
		t.Fatal("accepted archived session")
	}
	info.Time.Archived = nil
	if validateOpenCodeBinding(info, "ses_wrong", project) == nil {
		t.Fatal("accepted different session")
	}
	if validateOpenCodeBinding(info, info.ID, t.TempDir()) == nil {
		t.Fatal("accepted different project")
	}
}

func TestOpenCodeBridgeRequestAuthAndRedirect(t *testing.T) {
	var called bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "opencode" || password != "private-test-secret" {
			t.Error("missing private authentication")
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
			return
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["model"] == nil {
			t.Error("lost model body")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer source.Close()
	if err := openCodeRequest(context.Background(), source.URL, "private-test-secret", "POST", "/model", map[string]any{"model": map[string]string{"id": "route"}}, nil); err != nil {
		t.Fatal(err)
	}
	err := openCodeRequest(context.Background(), source.URL, "private-test-secret", "GET", "/redirect", nil, nil)
	if err == nil || called {
		t.Fatal("redirect could leak credential")
	}
	if strings.Contains(err.Error(), "private-test-secret") {
		t.Fatal("credential in diagnostic")
	}
}

func TestOpenCodeBridgeConfigResumePreservesState(t *testing.T) {
	root := t.TempDir()
	r := pitputRuntime{identity: "host/role/agent", boards: []string{"dev"}, record: session.Record{Project: root}}
	paths, err := r.openCodeBridgeConfigs(root, "http://127.0.0.1:4011", "/tmp/private/password", "ses_fixture")
	if err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(paths[0])
	var initial map[string]any
	if err := toml.Unmarshal(first, &initial); err != nil {
		t.Fatal(err)
	}
	next, err := r.openCodeBridgeConfigs(root, "http://127.0.0.1:4012", "/tmp/new/password", "ses_fixture")
	if err != nil || next[0] != paths[0] {
		t.Fatal("resume moved config", err)
	}
	data, _ := os.ReadFile(next[0])
	var updated map[string]any
	if err := toml.Unmarshal(data, &updated); err != nil {
		t.Fatal(err)
	}
	if initial["state_directory"] != updated["state_directory"] || updated["backend"] != "opencode" || updated["opencode_password_file"] != "/tmp/new/password" {
		t.Fatal("invalid resumed configuration")
	}
	stat, _ := os.Stat(next[0])
	if stat.Mode().Perm() != 0600 {
		t.Fatal("public config")
	}
}
