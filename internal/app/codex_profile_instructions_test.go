package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseCodexProfileInstructions(t *testing.T) {
	for _, tc := range []struct {
		raw, value    string
		present, fail bool
	}{
		{`model="fixture"`, "", false, false},
		{`developer_instructions=""`, "", true, false},
		{`developer_instructions="profile guidance"`, "profile guidance", true, false},
		{`developer_instructions=5`, "", false, true},
		{"developer_instructions='one'\ndeveloper_instructions='two'", "", false, true},
		{"[nested]\ndeveloper_instructions='ignored'", "", false, false},
	} {
		got, err := parseCodexProfileInstructions([]byte(tc.raw))
		if (err != nil) != tc.fail || !tc.fail && (got.Present != tc.present || got.Value != tc.value) {
			t.Fatal(tc, got, err)
		}
	}
	for _, args := range [][]string{{"--", "--profile", "ignored"}, {"-c", `key="--profile"`}} {
		got, err := codexProfileInstructions(args, nil, "")
		if err != nil || got.Present {
			t.Fatal("literal text selected profile", args, got, err)
		}
	}
	for _, name := range []string{"", "..", "../escape", "a/b", `a\b`} {
		if _, err := codexProfileInstructions([]string{"--profile", name}, nil, ""); err == nil {
			t.Fatal("invalid profile accepted", name)
		}
		if name != "" {
			if _, err := codexProfileInstructions([]string{"-p" + name}, nil, ""); err == nil {
				t.Fatal("invalid attached profile accepted", name)
			}
		}
	}
}

// Filesystem coverage runs in the full GitLab suite, not the local pure checks.
func TestCodexProfileInstructionsUsesChildEnvironment(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "work.config.toml"), []byte(`developer_instructions="child profile"`), 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"CODEX_HOME=/unrelated", "CODEX_HOME=" + home}
	for _, args := range [][]string{{"--profile", "work"}, {"-p", "work"}, {"--profile=work"}, {"-p=work"}, {"-pwork"}} {
		got, err := codexProfileInstructions(args, env, home)
		if err != nil || !got.Present || got.Value != "child profile" {
			t.Fatal(args, got, err)
		}
	}
	if got, err := codexProfileInstructions([]string{"--profile", "missing"}, env, home); err != nil || got.Present {
		t.Fatal("missing file should inherit", got, err)
	}
	if err := os.Mkdir(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "fallback.config.toml"), []byte(`developer_instructions="home fallback"`), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := codexProfileInstructions([]string{"-p", "fallback"}, []string{"HOME=" + home}, home)
	if err != nil || got.Value != "home fallback" {
		t.Fatal(got, err)
	}
}
