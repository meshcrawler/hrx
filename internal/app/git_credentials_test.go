package app

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrokerGitCredentialProtocol(t *testing.T) {
	t.Setenv("HRX_GITLAB_CREDENTIAL_HOST", "gitlab.example")
	t.Setenv("HRX_GITLAB_CREDENTIAL_USER", "hrx_agent")
	t.Setenv("GITLAB_TOKEN", "fixture-secret")
	t.Setenv("HRX_AGENT_IDENTITY", "") // No broker refresh: the launch token is answered.
	for _, tc := range []struct {
		name, operation, input, want string
		fail                         bool
	}{
		{"get", "get", "protocol=https\nhost=gitlab.example\n\n", "username=hrx_agent\npassword=fixture-secret\n\n", false},
		{"multi-challenge", "get", "protocol=https\nhost=gitlab.example\nwwwauth[]=Basic realm=GitLab\nwwwauth[]=Bearer realm=GitLab\ncapability[]=authtype\ncapability[]=state\n\n", "username=hrx_agent\npassword=fixture-secret\n\n", false},
		{"foreign", "get", "protocol=https\nhost=elsewhere.example\n\n", "quit=true\n\n", false},
		{"suffix", "get", "protocol=https\nhost=gitlab.example.evil\n\n", "quit=true\n\n", false},
		{"http", "get", "protocol=http\nhost=gitlab.example\n\n", "quit=true\n\n", false},
		{"port", "get", "protocol=https\nhost=gitlab.example:1234\n\n", "quit=true\n\n", false},
		{"other-user", "get", "protocol=https\nhost=gitlab.example\nusername=personal\n\n", "quit=true\n\n", false},
		{"duplicate", "get", "protocol=https\nhost=gitlab.example\nhost=evil\n\n", "", true},
		{"store", "store", "password=fixture-secret\n\n", "", false},
		{"erase", "erase", "host=gitlab.example\n\n", "", false},
		{"oversize", "get", "path=" + strings.Repeat("a", 17000), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := gitCredential([]string{tc.operation}, strings.NewReader(tc.input), &out)
			if (err != nil) != tc.fail || out.String() != tc.want {
				t.Fatalf("unexpected helper result: error=%v", err)
			}
		})
	}
	t.Setenv("GITLAB_TOKEN", "")
	var out bytes.Buffer
	if err := gitCredential([]string{"get"}, strings.NewReader("protocol=https\nhost=gitlab.example\n\n"), &out); err != nil || out.String() != "quit=true\n\n" {
		t.Fatal("missing token did not fail closed")
	}
}

func TestBrokerRealGitNoninteractiveCredentials(t *testing.T) {
	dir := t.TempDir()
	// Exercise shell quoting as well as the installed helper dispatch path.
	executable := filepath.Join(dir, "hrx's helper")
	build := exec.Command("go", "build", "-o", executable, "../../cmd/hrx")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	marker := filepath.Join(dir, "personal-helper-called")
	config := "[credential]\n\thelper = !touch '" + marker + "'\n[credential \"https://gitlab.example\"]\n\thelper = !touch '" + marker + "'\n\tusername = personal\n"
	if err := os.WriteFile(filepath.Join(dir, ".gitconfig"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	base := childEnvironment(os.Environ(), map[string]string{
		"HOME": dir, "GIT_CONFIG_GLOBAL": filepath.Join(dir, ".gitconfig"), "GIT_CONFIG_NOSYSTEM": "1",
		"GITLAB_TOKEN": "fixture-secret", "HRX_AGENT_IDENTITY": "", "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "test.preserved", "GIT_CONFIG_VALUE_0": "yes",
		"GIT_CONFIG_PARAMETERS": "'credential.helper=personal'", "GIT_TRACE": "1", "GIT_ASKPASS": marker,
	}, nil)
	env, err := gitCredentialEnvironment(base, executable, "gitlab.example", "hrx_agent", []string{"ssh.gitlab.example"})
	if err != nil {
		t.Fatal(err)
	}
	run := func(input string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Env = env
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(input)
		output, err := cmd.CombinedOutput()
		return string(output), err
	}
	output, err := run("protocol=https\nhost=gitlab.example\n\n", "credential", "fill")
	if err != nil || !strings.Contains(output, "password=fixture-secret") || !strings.Contains(output, "username=hrx_agent") {
		t.Fatal("Git credential fill failed", err)
	}
	for _, request := range []string{"protocol=https\nhost=foreign.example\n\n", "protocol=http\nhost=gitlab.example\n\n"} {
		output, err = run(request, "credential", "fill")
		if err == nil || strings.Contains(output, "fixture-secret") {
			t.Fatal("foreign host received credentials")
		}
	}
	for _, op := range []string{"approve", "reject"} {
		if _, err := run("protocol=https\nhost=gitlab.example\nusername=hrx_agent\npassword=fixture-secret\n\n", "credential", op); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("personal credential helper invoked")
	}
	if output, err := run("", "config", "--get", "test.preserved"); err != nil || strings.TrimSpace(output) != "yes" {
		t.Fatal("lost inherited Git config")
	}
	for _, remote := range []string{"git@ssh.gitlab.example:group/repo.git", "ssh://git@gitlab.example/group/repo.git", "ssh://git@ssh.gitlab.example:22/group/repo.git"} {
		if output, err := run("", "ls-remote", "--get-url", remote); err != nil || strings.TrimSpace(output) != "https://gitlab.example/group/repo.git" {
			t.Fatalf("rewrite failed: %q %v", output, err)
		}
	}
	if output, err := run("", "ls-remote", "--get-url", "git@foreign.example:group/repo.git"); err != nil || strings.TrimSpace(output) != "git@foreign.example:group/repo.git" {
		t.Fatal("rewrote unrelated host")
	}
	data, err := os.ReadFile(filepath.Join(dir, ".gitconfig"))
	if err != nil || string(data) != config {
		t.Fatal("changed saved Git config")
	}
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "GIT_CONFIG_PARAMETERS=") || strings.Contains(joined, "GIT_TRACE=") || !strings.Contains(joined, "GIT_TERMINAL_PROMPT=0") {
		t.Fatal("unsafe prompting or config inheritance")
	}
}
