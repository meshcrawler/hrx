//go:build darwin || linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/matrix"
	"github.com/meshcrawler/hrx/internal/session"
)

// Run the real launch/lease/exec path in a child: execHarness intentionally
// replaces or exits its caller. Fake executables cannot contact providers.
func TestMatrixProcessHelper(t *testing.T) {
	encoded := os.Getenv("HRX_MATRIX_CELL")
	if encoded == "" {
		t.Skip("subprocess helper")
	}
	var cell matrix.Cell
	if json.Unmarshal([]byte(encoded), &cell) != nil {
		os.Exit(90)
	}
	dir := os.Getenv("HRX_MATRIX_DIR")
	o := LaunchOptions{Model: cell.Route, Reasoning: cell.Reasoning, Yolo: cell.Permissions == "yolo",
		YesodMail: cell.Communications == "mail" || cell.Communications == "both",
		YesodBBS:  cell.Communications == "bulletin" || cell.Communications == "both",
		Extra:     []string{map[string]string{"codex": "exec", "claude": "-p", "opencode": "run"}[cell.Harness], "fixture prompt"}}
	if cell.Execution == "interactive" {
		o.Extra = nil
	}
	if o.YesodMail || o.YesodBBS {
		o.YesodAgent = "fixture/hrx/matrix"
	}
	if cell.Session != "fresh" {
		store, err := session.Default()
		if err != nil {
			os.Exit(91)
		}
		r := session.Record{NativeID: "fixture-session", Harness: cell.Harness, Identity: cell.Identity, Route: cell.Route,
			Project: dir, Reasoning: cell.Reasoning, Mail: o.YesodMail, BBS: o.YesodBBS, Agent: o.YesodAgent}
		if store.Save(r) != nil {
			os.Exit(92)
		}
		o.Model, o.Reasoning, o.YesodAgent = "", "", ""
		o.YesodMail, o.YesodBBS = false, false
		o.Resume = "fixture-session"
		if cell.Session == "continue" {
			o.Resume, o.Continue = "", true
		}
		o, _, err = restoreSession(store, cell.Harness, dir, o)
		if err != nil || o.Model != cell.Route || o.YesodAgent != r.Agent || o.Yolo != (cell.Permissions == "yolo") {
			os.Exit(93)
		}
	}
	s := Settings{Harness: cell.Harness, ProviderName: "fixture", CachePath: filepath.Join(dir, "catalog.json"),
		GeneratedCatalogPath: filepath.Join(dir, "codex.json"), Provider: Provider{BaseURL: "http://127.0.0.1:1/v1", WireAPI: "responses", EnvKey: "HRX_MATRIX_KEY"}}
	if os.WriteFile(s.GeneratedCatalogPath, []byte(`{"models":[{"slug":"fixture/route","base_instructions":"fixture base"}]}`), 0600) != nil {
		os.Exit(94)
	}
	var launch Launch
	var err error
	if cell.Harness == "codex" {
		launch, err = BuildLaunch(s, matrixAdapterInput().Snapshots[cell.Harness], o)
	} else {
		launch, err = buildOtherLaunch(s, matrixAdapterInput().Snapshots[cell.Harness], o, "fixture-key-not-real")
	}
	if err != nil {
		os.Exit(95)
	}
	if prepareYesod(context.Background(), s, o, &launch, io.Discard) != nil {
		os.Exit(96)
	}
	// The fake harness observes exactly what the real exec path received.
	want, _ := json.Marshal(append([]string{}, launch.Args...))
	if os.WriteFile(filepath.Join(dir, "expected-argv.json"), want, 0600) != nil {
		os.Exit(97)
	}
	if execHarness(launch) != nil {
		os.Exit(98)
	}
}

func TestMatrixOneShotProcesses(t *testing.T) {
	manifest, err := matrix.Generate(matrixAdapterInput())
	if err != nil {
		t.Fatal(err)
	}
	executed := 0
	for _, cell := range manifest.Cells {
		if cell.Execution != "one-shot" {
			continue // Interactive cells require a PTY, never silently count these.
		}
		t.Run(cell.ID, func(t *testing.T) {
			dir := t.TempDir()
			ps, err := exec.LookPath("ps")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(ps, filepath.Join(dir, "ps")); err != nil {
				t.Fatal(err)
			}
			for name, script := range map[string]string{
				"python3": `#!/bin/sh
printf 'allocate\n' >> "$HRX_MATRIX_DIR/events"
printf '%s' '{"agent":"fixture/hrx/matrix"}'
`,
				"yesod": `#!/bin/sh
[ -z "$YESOD_AGENT_IDENTITY$YESOD_MAIL_IDENTITY$YESOD_MAIL_SELF" ] || exit 9
read -r token
[ "${#token}" = 64 ] || exit 9
case "$2" in claim|release|renew) printf '%s\n' "$2" >> "$HRX_MATRIX_DIR/events";; *) exit 9;; esac
`,
				cell.Harness: `#!/bin/sh
printf '%s\000' "$@" > "$HRX_MATRIX_DIR/actual-argv"
case "$HRX_MATRIX_HARNESS" in
  claude)
    [ "$ANTHROPIC_MODEL" = fixture/route ] || exit 9
    [ "$ANTHROPIC_CUSTOM_HEADERS" = 'x-litellm-api-key: Bearer fixture-key-not-real' ] || exit 9
    [ "$ANTHROPIC_AUTH_TOKEN" = fixture-key-not-real ] || exit 9
    [ -z "$ANTHROPIC_API_KEY" ] || exit 9
    ;;
  opencode)
    [ "$HRX_GATEWAY_KEY" = fixture-key-not-real ] || exit 9
    case "$OPENCODE_CONFIG_CONTENT" in *'"model":"hrx/fixture/route"'*) ;; *) exit 9;; esac
    ;;
esac
if [ "$HRX_MATRIX_COMMS" != off ]; then
  [ "$YESOD_AGENT_IDENTITY" = fixture/hrx/matrix ] || exit 9
  [ "$YESOD_MAIL_IDENTITY" = "$YESOD_AGENT_IDENTITY" ] || exit 9
  [ "$YESOD_MAIL_SELF" = "$YESOD_AGENT_IDENTITY" ] || exit 9
fi
printf 'launch\n' >> "$HRX_MATRIX_DIR/events"
printf 'matrix-output'
exit 37
`,
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			encoded, _ := json.Marshal(cell)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMatrixProcessHelper$")
			cmd.WaitDelay = time.Second
			cmd.Env = []string{"PATH=" + dir, "HOME=" + dir, "XDG_STATE_HOME=" + filepath.Join(dir, "state"), "GORACE=atexit_sleep_ms=0",
				"HRX_MATRIX_DIR=" + dir, "HRX_MATRIX_CELL=" + string(encoded), "HRX_MATRIX_COMMS=" + cell.Communications,
				"HRX_MATRIX_HARNESS=" + cell.Harness, "ANTHROPIC_AUTH_TOKEN=parent-fixture-token", "ANTHROPIC_API_KEY=parent-fixture-key",
				"YESOD_AGENT_IDENTITY=parent/agent/old", "YESOD_MAIL_IDENTITY=parent/agent/old", "YESOD_MAIL_SELF=parent/agent/old"}
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 37 || string(output) != "matrix-output" {
				// No child diagnostics are reported, even for a broken fixture.
				code := -1
				if exit != nil {
					code = exit.ExitCode()
				}
				t.Fatalf("fixture exit/output mismatch (code=%d output_bytes=%d deadline=%v)", code, len(output), ctx.Err())
			}
			events, err := os.ReadFile(filepath.Join(dir, "events"))
			wantEvents := "launch\n"
			if cell.Communications != "off" {
				wantEvents = "allocate\nclaim\nlaunch\nrelease\n"
			}
			if err != nil || string(events) != wantEvents {
				t.Fatal("allocation/claim/launch/release order or count mismatch")
			}
			actual, err := os.ReadFile(filepath.Join(dir, "actual-argv"))
			if err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(filepath.Join(dir, "expected-argv.json"))
			var expected []string
			if json.Unmarshal(data, &expected) != nil || string(actual) != strings.Join(expected, "\x00")+"\x00" {
				t.Fatal("exec changed argv boundaries")
			}
		})
		executed++
	}
	if executed != 216 {
		t.Fatalf("incomplete one-shot matrix: %d", executed)
	}
}
