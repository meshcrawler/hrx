package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/catalog"
)

func TestAutoInputAndNoninteractive(t *testing.T) {
	for _, args := range [][]string{{"--auto", "claude"}, {"--auto=claude"}} {
		request, _, err := autoInput(args)
		if err != nil || request != "claude" {
			t.Fatal(request, err)
		}
	}
	for _, args := range [][]string{{"--auto"}, {"--auto="}, {"--auto=x", "--bogus"}, {"--auto=x", "--model"}, {"--auto=x", "--yolo", "--no-yolo"}, {"--auto=x", "--gitlab=true"}, {"--auto=x", "--model=a", "--model=b"}, {"--auto=x", "--", "echo hi"}} {
		if _, _, err := autoInput(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	err := runAuto(context.Background(), []string{"--auto=claude"}, strings.NewReader("y\n"), io.Discard, io.Discard, false, func([]string) error { t.Fatal("launched"); return nil })
	if err == nil || !strings.Contains(err.Error(), "interactive") {
		t.Fatal(err)
	}
}

func TestAutoProposalSchema(t *testing.T) {
	for _, data := range []string{`{"harness":"claude","yolo":true}`, `{"args":["sh","-c","bad"]}`, `{"harness":"claude"} {}`, `not json`, `null`, `{"harness":null}`, `{"harness":"codex","harness":"claude"}`, `{"task":"x\u001b[1m"}`, strings.Repeat("x", autoLimit+1)} {
		if _, err := decodeAuto([]byte(data)); err == nil {
			t.Fatalf("accepted %q", data[:min(len(data), 80)])
		}
	}
	if _, err := decodeAuto([]byte(`{"harness":"claude","pitput":true,"agent_identity":"laptop/project-mayor/agent"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestAutoExampleBoardsIdentityAndConflicts(t *testing.T) {
	request := "claude as laptop/project-mayor/agent using pitput"
	p := autoProposal{Harness: "claude", Identity: "laptop/project-mayor/agent", Pitput: true}
	if _, question, err := autoArguments(p, nil, request); err != nil || !strings.Contains(question, "board") {
		t.Fatal(question, err)
	}
	p.Boards = []string{"pitput-dev"}
	args, q, err := autoArguments(p, nil, request)
	if err != nil || q != "" {
		t.Fatal(q, err)
	}
	o, err := parseLaunchFor("claude", args[1:])
	if err != nil || o.AgentIdentity != p.Identity || !o.Pitput || o.Plugin != "" || o.Yolo || !o.yoloSet {
		t.Fatal(o, err)
	}
	p.Identity = "laptop/wrong/agent"
	if _, _, err := autoArguments(p, nil, request); err == nil {
		t.Fatal("identity changed")
	}
	p.Identity = "laptop/project-mayor/agent"
	p.Model = "a"
	if _, _, err := autoArguments(p, map[string][]string{"--model": {"b"}}, request); err == nil {
		t.Fatal("silently overrode model")
	}
	if _, _, err := autoArguments(p, map[string][]string{"--pitput-board": {"other"}}, request); err == nil {
		t.Fatal("silently overrode boards")
	}
	p.Boards = []string{"$(touch /tmp/bad)"}
	if _, _, err := autoArguments(p, nil, request); err == nil {
		t.Fatal("accepted invalid board")
	}
}

func TestAutoTaskLiteralAndPermissions(t *testing.T) {
	for _, h := range []string{"codex", "claude", "opencode"} {
		for _, task := range []string{"exec", "resume", "$(touch /tmp/hrx-auto-injection); `id`"} {
			args, q, err := autoArguments(autoProposal{Harness: h, Task: task}, nil, "")
			if err != nil || q != "" {
				t.Fatal(err, q)
			}
			o, err := parseLaunchFor(h, args[1:])
			if err != nil || o.Yolo || nativeCommand(h, o.Extra) != "" {
				t.Fatal(args, o, err)
			}
			if o.Extra[len(o.Extra)-1] != task {
				t.Fatal("task changed")
			}
		}
		args, _, err := autoArguments(autoProposal{Harness: h}, map[string][]string{"--yolo": {"true"}}, "")
		if err != nil || !slices.Contains(args, "--yolo") {
			t.Fatal(args, err)
		}
	}
	if _, _, err := autoArguments(autoProposal{Harness: "codex", Task: "--yolo"}, nil, ""); err == nil {
		t.Fatal("accepted option task")
	}
}

func TestAutoRouteNoFallback(t *testing.T) {
	s := catalog.Snapshot{Models: []catalog.Model{{ID: "inkling", Route: "inkling-a", Harnesses: map[string]catalog.Capability{"opencode": {WireAPI: "chat_completions"}}}}}
	if got, err := autoRoute(s, ""); err != nil || got != "inkling-a" {
		t.Fatal(got, err)
	}
	s.Models = append(s.Models, catalog.Model{ID: "inkling", Route: "inkling-b", Harnesses: s.Models[0].Harnesses})
	if _, err := autoRoute(s, ""); err == nil {
		t.Fatal("ambiguous route accepted")
	}
	s.Models = append(s.Models, catalog.Model{ID: "inkling", Route: "inkling:free", Harnesses: s.Models[0].Harnesses})
	if _, err := autoRoute(s, ""); err == nil {
		t.Fatal("multiple routes require explicit selection")
	}
	if _, err := autoRoute(s, "nonexistent-expensive"); err == nil {
		t.Fatal("unsupported route accepted")
	}
	if _, err := autoRoute(catalog.Snapshot{}, ""); err == nil {
		t.Fatal("missing Inkling accepted")
	}
}

func TestAutoConfirmationNoSideEffects(t *testing.T) {
	for _, tc := range []struct {
		name, input     string
		calls, launches int
	}{{"yes", "y\n", 1, 1}, {"no", "n\n", 1, 0}, {"reprompt-no", "r\nuse opencode\nn\n", 2, 0}, {"reprompt-yes", "reprompt\nuse opencode\ny\n", 2, 1}, {"invalid-answer", "maybe\nn\n", 1, 0}, {"EOF", "", 1, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			calls, launches := 0, 0
			var out bytes.Buffer
			infer := func(_ context.Context, request string, _ map[string][]string) (autoProposal, error) {
				if launches != 0 {
					t.Fatal("side effects before approval")
				}
				calls++
				h := "claude"
				if strings.Contains(request, "opencode") {
					h = "opencode"
				}
				return autoProposal{Harness: h}, nil
			}
			validate := func(_ context.Context, args []string) ([]string, error) {
				if launches != 0 {
					t.Fatal("launched before validation")
				}
				return args, nil
			}
			err := autoLoop(context.Background(), "claude", nil, strings.NewReader(tc.input), &out, infer, validate, func(args []string) error {
				launches++
				if calls == 2 && args[0] != "opencode" {
					t.Fatal(args)
				}
				return nil
			})
			if tc.name == "EOF" {
				if err == nil {
					t.Fatal("EOF accepted")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if calls != tc.calls || launches != tc.launches {
				t.Fatal(calls, launches)
			}
		})
	}
}

func TestAutoClarificationAndBound(t *testing.T) {
	calls, launches := 0, 0
	infer := func(_ context.Context, request string, _ map[string][]string) (autoProposal, error) {
		calls++
		p := autoProposal{Harness: "claude", Pitput: true, Identity: "laptop/project-mayor/agent"}
		if strings.Contains(request, "Clarification:") {
			p.Boards = []string{"pitput-dev"}
		}
		return p, nil
	}
	validate := func(_ context.Context, a []string) ([]string, error) { return a, nil }
	err := autoLoop(context.Background(), "claude as laptop/project-mayor/agent using pitput", nil, strings.NewReader("pitput-dev\ny\n"), io.Discard, infer, validate, func(a []string) error { launches++; return nil })
	if err != nil || calls != 2 || launches != 1 {
		t.Fatal(err, calls, launches)
	}
	infer = func(context.Context, string, map[string][]string) (autoProposal, error) {
		return autoProposal{Question: "clarify"}, nil
	}
	err = autoLoop(context.Background(), "x", nil, strings.NewReader(strings.Repeat("x\n", 9)), io.Discard, infer, validate, func([]string) error { t.Fatal("launched"); return nil })
	if err == nil || !strings.Contains(err.Error(), "eight") {
		t.Fatal(err)
	}
}

func TestAutoGatewayFixture(t *testing.T) {
	for _, tc := range []struct {
		name            string
		status          int
		content, finish string
		wantErr         bool
	}{{"ok", 200, `{"harness":"claude"}`, "stop", false}, {"bad-json", 200, `blah`, "stop", true}, {"unknown-fields", 200, `{"harness":"claude","command":"rm -rf"}`, "stop", true}, {"truncated", 200, `{"harness":"claude"}`, "length", true}, {"auth", 401, "SECRET", "", true}, {"failure", 503, "SECRET", "", true}, {"size", 200, strings.Repeat("s", autoLimit), "stop", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer SECRET" {
					t.Error("bad endpoint/auth")
				}
				data, _ := io.ReadAll(r.Body)
				if bytes.Contains(data, []byte("SECRET")) {
					t.Error("credential in prompt")
				}
				var payload map[string]any
				if json.Unmarshal(data, &payload) != nil || payload["model"] != "inkling-fixture" {
					t.Error("bad body")
				}
				w.WriteHeader(tc.status)
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": tc.content}, "finish_reason": tc.finish}}})
			}))
			defer server.Close()
			g := autoGateway{settings: Settings{Provider: Provider{BaseURL: server.URL + "/v1"}}, key: "SECRET", route: "inkling-fixture", client: server.Client()}
			_, err := g.infer(context.Background(), "claude", nil)
			if (err != nil) != tc.wantErr {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatal("retried")
			}
			if err != nil && strings.Contains(err.Error(), "SECRET") {
				t.Fatal("secret leak")
			}
		})
	}
	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(100 * time.Millisecond) }))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		g := autoGateway{settings: Settings{Provider: Provider{BaseURL: server.URL}}, client: server.Client()}
		if _, err := g.infer(ctx, "x", nil); err == nil {
			t.Fatal("accepted timeout")
		}
	})
}

func TestAutoValidationFailureNeverLaunches(t *testing.T) {
	err := autoLoop(context.Background(), "x", nil, strings.NewReader("y\n"), io.Discard, func(context.Context, string, map[string][]string) (autoProposal, error) {
		return autoProposal{Harness: "codex", Model: "unsupported"}, nil
	}, func(context.Context, []string) ([]string, error) { return nil, errors.New("unsupported model") }, func([]string) error { t.Fatal("launched invalid model"); return nil })
	if err == nil {
		t.Fatal("expected validation failure")
	}
}

// Opt-in smoke only translates a fixed synthetic request; it cannot launch,
// enroll, provision credentials, or read any project files.
func TestAutoLiveTranslation(t *testing.T) {
	if os.Getenv("HRX_AUTO_LIVE_TEST") != "1" {
		t.Skip("set HRX_AUTO_LIVE_TEST=1 for a read-only gateway translation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var g autoGateway
	var err error
	if route := os.Getenv("HRX_AUTO_LIVE_ROUTE"); route != "" {
		g, err = openAutoGateway(ctx, "opencode", route)
	} else {
		g, err = newAutoGateway(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Log("authorized parser route:", g.route)
	request := "claude as laptop/project-mayor/agent using pitput board pitput-dev"
	p, err := g.infer(ctx, request, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, q, err := autoArguments(p, nil, request)
	if err != nil || q != "" {
		t.Fatal(err, q)
	}
	if a[0] != "claude" || !p.Pitput || p.Identity != "laptop/project-mayor/agent" || !slices.Equal(p.Boards, []string{"pitput-dev"}) {
		t.Fatalf("wrong interpretation: %#v", p)
	}
	t.Logf("validated proposal: %q", a)
}

func TestAutoLocalGatewayEndToEnd(t *testing.T) {
	for _, harness := range []string{"claude", "codex", "opencode"} {
		t.Run(harness, func(t *testing.T) {
			for _, accept := range []bool{false, true} {
				t.Run(fmt.Sprint(accept), func(t *testing.T) {
					configDir := t.TempDir()
					t.Setenv("XDG_CONFIG_HOME", configDir)
					t.Setenv("XDG_CACHE_HOME", t.TempDir())
					t.Setenv("CODEX_HOME", configDir)
					t.Setenv("HRX_CONFIG", filepath.Join(configDir, "hrx.toml"))
					t.Setenv("HRX_AUTO_FIXTURE_KEY", "fixture-secret")
					t.Setenv("HRX_CODEX_PROVIDER", "")
					var inferred atomic.Int32
					caps := map[string]catalog.Capability{"codex": {WireAPI: "responses", Reasoning: []string{"low"}, DefaultReasoning: "low"}, "claude": {WireAPI: "messages"}, "opencode": {WireAPI: "chat_completions"}}
					server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/dashboard/models.json":
							var d catalog.Dashboard
							d.Version = 1
							d.GeneratedAt = time.Now()
							d.Coverage.Providers = append(d.Coverage.Providers, struct {
								Source string `json:"source"`
								OK     bool   `json:"ok"`
							}{"gateway", true})
							for _, id := range []string{"inkling", "fixture-model"} {
								source := catalog.Source{Route: id, Available: true}
								source.Details.Harnesses = caps
								d.Models = append(d.Models, catalog.Identity{ID: id, Available: true, Sources: []catalog.Source{source}})
							}
							json.NewEncoder(w).Encode(d)
						case "/v1/models":
							if r.Header.Get("Authorization") != "Bearer fixture-secret" {
								t.Error("missing model authorization")
							}
							io.WriteString(w, `{"data":[{"id":"inkling"},{"id":"fixture-model"}]}`)
						case "/v1/chat/completions":
							inferred.Add(1)
							body, _ := io.ReadAll(r.Body)
							if bytes.Contains(body, []byte("fixture-secret")) || bytes.Contains(body, []byte(configDir)) {
								t.Error("private configuration leaked")
							}
							p := autoProposal{Harness: harness, Identity: "fixture/mayor/agent", Pitput: true, Boards: []string{"fixture-board"}, GitLab: true, Task: "Inspect the handoff"}
							content, _ := json.Marshal(p)
							json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(content)}, "finish_reason": "stop"}}})
						default:
							t.Errorf("unexpected side effect endpoint %s", r.URL.Path)
							w.WriteHeader(500)
						}
					}))
					defer server.Close()
					old := http.DefaultTransport
					http.DefaultTransport = server.Client().Transport
					defer func() { http.DefaultTransport = old }()
					config := fmt.Sprintf("[permissions]\nyolo = true\n[opencode]\nmodel = 'fixture-model'\n[opencode.provider]\nbase_url = %q\nenv_key = 'HRX_AUTO_FIXTURE_KEY'\n[claude]\nmodel = 'fixture-model'\n[claude.provider]\nbase_url = %q\nenv_key = 'HRX_AUTO_FIXTURE_KEY'\n", server.URL+"/v1", server.URL+"/v1")
					if err := os.WriteFile(os.Getenv("HRX_CONFIG"), []byte(config), 0600); err != nil {
						t.Fatal(err)
					}
					codex := fmt.Sprintf("model = 'fixture-model'\nmodel_provider = 'fixture'\n[model_providers.fixture]\nbase_url = %q\nwire_api = 'responses'\nenv_key = 'HRX_AUTO_FIXTURE_KEY'\n", server.URL+"/v1")
					if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(codex), 0600); err != nil {
						t.Fatal(err)
					}
					var output bytes.Buffer
					launches := 0
					answer := "n\n"
					if accept {
						answer = "y\n"
					}
					err := runAuto(context.Background(), []string{"--auto", harness + " as fixture/mayor/agent using pitput board fixture-board and gitlab. Task: Inspect the handoff"}, strings.NewReader(answer), io.Discard, &output, true, func(argv []string) error {
						launches++
						if argv[0] != harness || !slices.Contains(argv, "fixture-model") || !slices.Contains(argv, "--gitlab") || !slices.Contains(argv, "--no-yolo") {
							t.Fatal(argv)
						}
						opts, err := parseLaunchFor(harness, argv[1:])
						if err != nil || opts.AgentIdentity != "fixture/mayor/agent" || opts.Plugin != "gitlab-service-account-broker" || !opts.Pitput || opts.Yolo {
							t.Fatal(opts, err)
						}
						return nil
					})
					if err != nil {
						t.Fatal(err, output.String())
					}
					if inferred.Load() != 1 || launches != map[bool]int{false: 0, true: 1}[accept] {
						t.Fatal(inferred.Load(), launches)
					}
					if strings.Contains(output.String(), "fixture-secret") {
						t.Fatal("credential in preview")
					}
					for _, flag := range []string{"--model=not-authorized", "--reasoning=unsupported"} {
						err = runAuto(context.Background(), []string{"--auto", harness + " as fixture/mayor/agent using pitput board fixture-board and gitlab", flag}, strings.NewReader("y\n"), io.Discard, io.Discard, true, func([]string) error { t.Fatal("invalid launch passed validation"); return nil })
						if err == nil {
							t.Fatalf("accepted unsupported setting %s", flag)
						}
					}
				})
			}
		})
	}
}

func TestAutoReviewIdentityAndSchemaRegressions(t *testing.T) {
	for _, data := range []string{`{"Harness":"claude"}`, `{"harness":"claude","HARNESS":"codex"}`, `{"task":"invisible\u202eoverride"}`} {
		if _, err := decodeAuto([]byte(data)); err == nil {
			t.Fatalf("accepted misleading schema/preview: %s", data)
		}
	}
	request := "claude as laptop/mayor/agent using pitput board dev. Task: record the worker as laptop/worker/agent"
	p := autoProposal{Harness: "claude", Identity: "laptop/mayor/agent", Pitput: true, Boards: []string{"dev"}, Task: "record the worker as laptop/worker/agent"}
	if _, q, err := autoArguments(p, nil, request); err != nil || q != "" {
		t.Fatal(q, err)
	}
	p.Identity = "laptop/worker/agent"
	if _, _, err := autoArguments(p, map[string][]string{"--agent-identity": {"laptop/worker/agent"}}, request); err == nil {
		t.Fatal("explicit identity silently overrode natural request")
	}
	p.Identity = "laptop/other/service"
	if _, q, err := autoArguments(p, nil, request+"\nCorrection: identity laptop/other/service"); err != nil || q != "" {
		t.Fatal(q, err)
	}
}

func TestAutoCancelledBeforeApprovalNeverLaunches(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := autoLoop(ctx, "claude", nil, strings.NewReader("y\n"), io.Discard,
		func(context.Context, string, map[string][]string) (autoProposal, error) {
			return autoProposal{Harness: "claude"}, nil
		},
		func(_ context.Context, args []string) ([]string, error) { cancel(); return args, nil },
		func([]string) error { t.Fatal("launched cancelled request"); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestAutoPrintBestGuessNeverLaunchesOrAsks(t *testing.T) {
	var sent map[string][]string
	infer := func(_ context.Context, _ string, explicit map[string][]string) (autoProposal, error) {
		sent = explicit
		return autoProposal{Harness: "opencode", Identity: "laptop/dev/agent", Pitput: true, Boards: []string{"pitput-dev"}, Task: "it's done"}, nil
	}
	validate := func(_ context.Context, a []string) ([]string, error) { return a, nil }
	var stdout, notes bytes.Buffer
	if err := autoPrint(context.Background(), "x", map[string][]string{"--gitlab": {"true"}}, &stdout, &notes, infer, validate); err != nil {
		t.Fatal(err)
	}
	line := stdout.String()
	if !strings.HasPrefix(line, "hrx 'opencode' ") || !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
		t.Fatal(line)
	}
	for _, want := range []string{"'--agent-identity' 'laptop/dev/agent'", "'--pitput-board' 'pitput-dev'", "'--no-yolo'", `'it'"'"'s done'`} {
		if !strings.Contains(line, want) {
			t.Fatal(want, line)
		}
	}
	if sent["--print"] != nil || notes.Len() != 0 {
		t.Fatal(sent, notes.String())
	}

	for name, tc := range map[string]struct {
		infer    autoInfer
		validate autoValidate
	}{
		"parser question": {func(context.Context, string, map[string][]string) (autoProposal, error) {
			return autoProposal{Question: "Which board?"}, nil
		}, validate},
		"validation question": {infer, func(context.Context, []string) ([]string, error) {
			return nil, autoQuestion("Which board?")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			stdout.Reset()
			notes.Reset()
			err := autoPrint(context.Background(), "x", nil, &stdout, &notes, tc.infer, tc.validate)
			if err == nil || stdout.Len() != 0 || !strings.Contains(notes.String(), "Which board?") {
				t.Fatal(err, stdout.String(), notes.String())
			}
		})
	}
}

func TestAutoPrintIsAllowedWithoutTerminalAndNotForwarded(t *testing.T) {
	_, explicit, err := autoInput([]string{"--auto", "claude", "--print"})
	if err != nil || explicit["--print"] == nil {
		t.Fatal(err, explicit)
	}
	if _, _, err := autoInput([]string{"--auto", "claude", "--print=yes"}); err == nil {
		t.Fatal("valued --print accepted")
	}
	t.Setenv("HRX_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))
	err = runAuto(context.Background(), []string{"--auto", "claude", "--print"}, strings.NewReader(""), io.Discard, io.Discard, false, func([]string) error { t.Fatal("launched"); return nil })
	if err != nil && strings.Contains(err.Error(), "interactive terminal") {
		t.Fatal(err)
	}
}
