package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestYesodOptionsAndSessionMappings(t *testing.T) {
	for _, harness := range []string{"codex", "claude", "opencode"} {
		for _, flags := range [][]string{{"--yesod-comms"}, {"--yesod-mail", "--yesod-bbs"}, {"--yesod-comms", "--yesod-mail"}} {
			o, err := parseLaunchFor(harness, flags)
			if err != nil || !o.YesodMail || !o.YesodBBS {
				t.Fatalf("%s %v: %+v %v", harness, flags, o, err)
			}
		}
	}
	for _, tc := range []struct {
		harness string
		options LaunchOptions
		want    []string
	}{
		{"codex", LaunchOptions{Yolo: true}, []string{"--yolo"}},
		{"claude", LaunchOptions{Yolo: true}, []string{"--dangerously-skip-permissions"}},
		{"opencode", LaunchOptions{Yolo: true}, []string{"--auto"}},
		{"codex", LaunchOptions{Resume: "session-1", Extra: []string{"hello"}}, []string{"resume", "session-1", "hello"}},
		{"codex", LaunchOptions{Continue: true, Extra: []string{"exec", "hello"}}, []string{"exec", "resume", "--last", "hello"}},
		{"codex", LaunchOptions{Yolo: true, Resume: "id", Extra: []string{"exec", "hello"}}, []string{"--yolo", "exec", "resume", "id", "hello"}},
		{"claude", LaunchOptions{Resume: "id", Extra: []string{"-p", "hello"}}, []string{"--resume", "id", "-p", "hello"}},
		{"claude", LaunchOptions{Continue: true}, []string{"--continue"}},
		{"opencode", LaunchOptions{Resume: "id", Yolo: true, Extra: []string{"run", "hello"}}, []string{"run", "--auto", "--session", "id", "hello"}},
		{"opencode", LaunchOptions{Continue: true}, []string{"--continue"}},
	} {
		got, err := sessionArguments(tc.harness, tc.options)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s %+v: %v %v, want %v", tc.harness, tc.options, got, err, tc.want)
		}
	}
}

func TestYesodUnsafeOrAmbiguousOptionsFailBeforeRegistration(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for _, tc := range []struct {
		harness string
		args    []string
	}{
		{"opencode", []string{"--yesod-bbs", "--", "-s", "existing"}},
		{"opencode", []string{"--yesod-mail", "--", "run", "--server=http://localhost:4096", "task"}},
		{"codex", []string{"--yesod-mail", "--", "exec", "--json", "resume", "existing"}},
	} {
		o, err := parseLaunchFor(tc.harness, tc.args)
		if err == nil {
			_, err = resolveLaunchSession(tc.harness, o)
		}
		if err == nil {
			t.Fatal("native resume lost identity guard")
		}
	}
	for _, args := range [][]string{
		{"--yesod-mail=false"}, {"--yesod-bbs=yes"}, {"--yesod-comms=true"}, {"--yolo=true"},
		{"--resume"}, {"--resume=a", "--continue"}, {"--resume=a", "--resume=b"},
		{"--yesod-agent=a/b/c"}, {"--yesod-mail", "--yesod-agent=bad"},
		{"--resume=id", "--", "resume", "other"},
		{"--", "exec", "--yesod-mail"}, {"--yolo", "--", "--sandbox", "read-only"},
	} {
		if _, err := ParseLaunch(args); err == nil {
			t.Errorf("accepted unsafe options %v", args)
		}
	}
	o, err := ParseLaunch([]string{"--resume=id", "--yesod-comms", "--yesod-agent=host/hrx-codex/project"})
	if err != nil || o.YesodAgent != "host/hrx-codex/project" {
		t.Fatalf("explicit resume identity: %+v %v", o, err)
	}
}

func TestYesodRejectsDetachedAndAlternateSessionModes(t *testing.T) {
	for _, tc := range []struct {
		harness string
		args    []string
	}{
		{"claude", []string{"--bg"}}, {"claude", []string{"--background"}},
		{"claude", []string{"--tmux=classic"}}, {"claude", []string{"--remote-control"}},
		{"claude", []string{"--fork-session"}}, {"claude", []string{"attach", "id"}},
		{"codex", []string{"fork", "id"}}, {"opencode", []string{"run", "--fork", "task"}},
	} {
		args := append([]string{"--yesod-comms", "--yesod-agent", "host/hrx/session", "--"}, tc.args...)
		if _, err := parseLaunchFor(tc.harness, args); err == nil {
			t.Fatalf("accepted lifecycle escape: %s %v", tc.harness, tc.args)
		}
	}
}

func TestYesodDryRunAndDisabledNeverRunHelper(t *testing.T) {
	t.Setenv("PATH", "")
	s := testSettings(t)
	var out bytes.Buffer
	l := Launch{Executable: "codex"}
	if err := prepareYesod(context.Background(), s, LaunchOptions{}, &l, &out); err != nil || l.Yesod != nil {
		t.Fatal("disabled mode changed launch")
	}
	o := LaunchOptions{YesodMail: true, YesodBBS: true, DryRun: true}
	if err := prepareYesod(context.Background(), s, o, &l, &out); err != nil {
		t.Fatal(err)
	}
	if l.Yesod == nil || !l.Yesod.Mail || !l.Yesod.BBS || len(l.Args) != 0 || len(l.Env) != 0 || out.Len() != 0 {
		t.Fatalf("dry-run mutated launch: %+v", l)
	}
	if _, err := os.Stat(filepath.Dir(s.CachePath)); err != nil {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(filepath.Dir(s.CachePath))
	if len(files) != 0 {
		t.Fatal("dry-run wrote instructions")
	}
}

func TestYesodPromptSelectionAndIdentity(t *testing.T) {
	agent := "laptop/hrx-codex/example"
	for _, modes := range [][2]bool{{true, false}, {false, true}, {true, true}} {
		p := yesodPrompt(agent, modes[0], modes[1])
		if !strings.Contains(p, agent) || !strings.Contains(p, "untrusted peer input") ||
			strings.Contains(p, "### Yesod mail") != modes[0] || strings.Contains(p, "### Yesod bulletin") != modes[1] {
			t.Fatalf("wrong prompt scope: %v %s", modes, p)
		}
	}
}

func TestYesodPrelaunchRegistersOnceAndSharesIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pitput"), []byte("#!/bin/sh\nif [ \"$1\" = agent ]; then printf '%s' '{\"identity\":\"host/hrx-claude/new\",\"boards\":[\"dev\"],\"status\":\"ready\"}'; fi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "registrations")
	script := "#!/bin/sh\nprintf x >> '" + marker + "'\nprintf '%s' '{\"agent\":\"host/hrx-claude/new\"}'\n"
	for _, name := range []string{"python3", "claude"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "yesod"), []byte("#!/bin/sh\nread -r token\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("YESOD_MAIL_IDENTITY", "parent/agent/old")
	s := testSettings(t)
	l := Launch{Executable: "claude", Model: "route", Args: []string{"--model", "route"}, Env: []string{"YESOD_MAIL_IDENTITY=parent/agent/old", "KEEP=value"}}
	var out bytes.Buffer
	if err := prepareYesod(context.Background(), s, LaunchOptions{YesodMail: true, YesodBBS: true, Pitput: true}, &l, &out); err != nil {
		t.Fatal(err)
	}
	calls, _ := os.ReadFile(marker)
	if l.pitputRuntime == nil || l.pitputRuntime.identity != "host/hrx-claude/new" {
		t.Fatal("Pitput did not use the allocated Yesod identity")
	}
	if string(calls) != "x" {
		t.Fatalf("registered %d times", len(calls))
	}
	for _, key := range []string{"YESOD_AGENT_IDENTITY", "YESOD_MAIL_IDENTITY", "YESOD_MAIL_SELF"} {
		count := 0
		for _, entry := range l.Env {
			if strings.HasPrefix(entry, key+"=") {
				count++
				if entry != key+"=host/hrx-claude/new" {
					t.Fatal("inconsistent child identity")
				}
			}
		}
		if count != 1 {
			t.Fatalf("%s occurs %d times", key, count)
		}
	}
	if os.Getenv("YESOD_MAIL_IDENTITY") != "parent/agent/old" {
		t.Fatal("parent environment modified")
	}
	if !strings.Contains(l.Args[1], "### Yesod mail") || !strings.Contains(l.Args[1], "### Yesod bulletin") || !strings.Contains(l.Args[1], "## Pitput communications") {
		t.Fatal("combined guidance missing")
	}
}

func TestYesodInjectionFailureReleasesClaim(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "lease-calls")
	t.Setenv("HRX_LEASE_TEST_LOG", marker)
	for name, script := range map[string]string{
		"python3": "#!/bin/sh\nprintf '%s' '{\"agent\":\"host/hrx-codex/new\"}'\n",
		"codex":   "#!/bin/sh\nexit 0\n",
		"yesod":   "#!/bin/sh\nread -r token\nprintf '%s\\n' \"$2\" >> \"$HRX_LEASE_TEST_LOG\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	s := testSettings(t)
	s.GeneratedCatalogPath = filepath.Join(dir, "missing-catalog")
	l := Launch{Executable: "codex", Model: "route"}
	var out bytes.Buffer
	if err := prepareYesod(context.Background(), s, LaunchOptions{YesodMail: true}, &l, &out); err == nil {
		t.Fatal("missing catalog did not fail injection")
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "claim\nrelease\n" {
		t.Fatalf("claim leaked: %q %v", data, err)
	}
	if l.lease != nil {
		t.Fatal("failed launch retained lease")
	}
}

func TestYesodInjectionPreservesPromptsAndRouting(t *testing.T) {
	s := testSettings(t)
	s.GeneratedCatalogPath = filepath.Join(filepath.Dir(s.CachePath), "generated.json")
	original := `{"models":[{"slug":"route","model_messages":{"instructions_template":"original template"},"base_instructions":"original base"}]}`
	if err := os.WriteFile(s.GeneratedCatalogPath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	l := Launch{Executable: "codex", Args: []string{"-c", "model_catalog_json=\"old\"", "--model", "route", "exec", "user prompt"}}
	prompt := yesodPrompt("host/hrx-codex/test", true, true)
	if err := injectYesod(s, &l, prompt); err != nil {
		t.Fatal(err)
	}
	if l.Args[0] != "--no-daemon" || l.Args[4] != "route" || l.Args[6] != "user prompt" {
		t.Fatal("routing or user prompt overwritten")
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(s.CachePath), "codex-yesod-*.json"))
	if len(files) != 1 {
		t.Fatal("missing injected catalog")
	}
	generated, _ := os.ReadFile(files[0])
	if !bytes.Contains(generated, []byte("original template")) || !bytes.Contains(generated, []byte("original base")) || !bytes.Contains(generated, []byte("host/hrx-codex/test")) {
		t.Fatal("instructions not appended")
	}
	unchanged, _ := os.ReadFile(s.GeneratedCatalogPath)
	if string(unchanged) != original {
		t.Fatal("original catalog modified")
	}
	l = Launch{Executable: "claude", Args: []string{"--model", "route", "-p", "user prompt"}}
	if err := injectYesod(s, &l, prompt); err != nil {
		t.Fatal(err)
	}
	if l.Args[0] != "--append-system-prompt" || l.Args[1] != prompt || l.Args[len(l.Args)-1] != "user prompt" {
		t.Fatal("Claude prompt not appended")
	}
	l = Launch{Executable: "opencode", Args: []string{"run", "user prompt"}, Env: []string{`OPENCODE_CONFIG_CONTENT={"model":"hrx/route","provider":{"hrx":{"options":{"apiKey":"{env:HRX_GATEWAY_KEY}"}}}}`, "HRX_GATEWAY_KEY=test-key"}}
	if err := injectYesod(s, &l, prompt); err != nil {
		t.Fatal(err)
	}
	if l.Args[0] != "run" || l.Args[1] != "--file" || l.Args[3] != "--standalone" {
		t.Fatal("OpenCode did not isolate the identity environment")
	}
	for _, e := range l.Env {
		if !strings.HasPrefix(e, "OPENCODE_CONFIG_CONTENT=") {
			continue
		}
		var config map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(e, "OPENCODE_CONFIG_CONTENT=")), &config); err != nil {
			t.Fatal(err)
		}
		if config["model"] != "hrx/route" {
			t.Fatal("OpenCode model lost")
		}
		path := config["instructions"].([]any)[0].(string)
		b, err := os.ReadFile(path)
		if err != nil || string(b) != prompt {
			t.Fatal("OpenCode instructions missing")
		}
		st, _ := os.Stat(path)
		if st.Mode().Perm() != 0600 {
			t.Fatal("instructions file permissions")
		}
	}
}

func TestYesodOpenCodeRunStandalonePlacement(t *testing.T) {
	l := Launch{Executable: "opencode", Args: []string{"run", "--model", "hrx/route", "--auto", "user prompt"}, Env: []string{`OPENCODE_CONFIG_CONTENT={"model":"hrx/route"}`}}
	if err := injectYesod(testSettings(t), &l, "instructions"); err != nil {
		t.Fatal(err)
	}
	want := []string{"--standalone", "--model", "hrx/route", "--auto", "user prompt"}
	if l.Args[0] != "run" || l.Args[1] != "--file" || !reflect.DeepEqual(l.Args[3:], want) {
		t.Fatalf("%v != %v", l.Args, want)
	}
}

func TestYesodOpenCodeInteractiveKeepsUserPrompt(t *testing.T) {
	for _, args := range [][]string{nil, {"--prompt", "user task"}, {"--prompt=user task"}} {
		l := Launch{Executable: "opencode", Args: args, Env: []string{`OPENCODE_CONFIG_CONTENT={"model":"hrx/route"}`}}
		if err := injectYesod(testSettings(t), &l, "session guidance"); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(l.Args, " ")
		if !strings.Contains(joined, "--standalone") || !strings.Contains(joined, "session guidance") {
			t.Fatal("missing native prompt or isolated environment")
		}
		if len(args) > 0 && !strings.Contains(joined, "user task") {
			t.Fatal("user prompt overwritten")
		}
	}
}
