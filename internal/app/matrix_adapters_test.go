package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/catalog"
	"github.com/meshcrawler/hrx/internal/matrix"
)

// This is exhaustive adapter coverage, not evidence of subprocess, PTY or
// provider success. The runner requires those additional checks separately.
func matrixAdapterInput() matrix.Input {
	input := matrix.Input{HRXVersion: "fixture", HarnessVersions: map[string]string{}, Snapshots: map[string]catalog.Snapshot{}}
	for _, harness := range []string{"codex", "claude", "opencode"} {
		cap := catalog.Capability{WireAPI: map[string]string{"codex": "responses", "claude": "messages", "opencode": "chat_completions"}[harness], Reasoning: []string{"low", "high"}, DefaultReasoning: "low"}
		caps := map[string]catalog.Capability{harness: cap}
		input.HarnessVersions[harness] = "fixture"
		input.Snapshots[harness] = catalog.Snapshot{
			Version: 1, GeneratedAt: time.Unix(100, 0), RefreshedAt: time.Unix(101, 0),
			Models:    []catalog.Model{{ID: "fixture-model", Route: "fixture/route", Harnesses: caps}},
			Inventory: []catalog.Entry{{ID: "fixture-model", Route: "fixture/route", Provider: "fixture", Account: "isolated", Available: true, Authorized: true, Harnesses: caps}},
		}
	}
	return input
}

func TestMatrixAdaptersEveryOptionCell(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "absent-state"))
	input := matrixAdapterInput()
	manifest, err := matrix.Generate(input)
	if err != nil || len(manifest.Cells) != 432 {
		t.Fatalf("matrix size=%d error=%v", len(manifest.Cells), err)
	}
	s := testSettings(t)
	for _, cell := range manifest.Cells {
		t.Run(cell.ID, func(t *testing.T) {
			args := []string{"--dry-run", "--latest", "--model", cell.Route}
			if cell.Reasoning != "" {
				args = append(args, "--reasoning", cell.Reasoning)
			}
			switch cell.Communications {
			case "mail":
				args = append(args, "--yesod-mail")
			case "bulletin":
				args = append(args, "--yesod-bbs")
			case "both":
				args = append(args, "--yesod-comms")
			}
			if cell.Communications != "off" {
				args = append(args, "--yesod-agent", "fixture/hrx/matrix")
			}
			if cell.Permissions == "yolo" {
				args = append(args, "--yolo")
			}
			switch cell.Session {
			case "resume":
				args = append(args, "--resume", "fixture-session")
			case "continue":
				args = append(args, "--continue")
			}
			if cell.Execution == "one-shot" {
				args = append(args, "--", map[string]string{"codex": "exec", "claude": "-p", "opencode": "run"}[cell.Harness], "fixture prompt")
			}
			o, err := parseLaunchFor(cell.Harness, args)
			if err != nil {
				t.Fatal(err)
			}
			settings := s
			settings.Harness = cell.Harness
			var launch Launch
			if cell.Harness == "codex" {
				launch, err = BuildLaunch(settings, input.Snapshots[cell.Harness], o)
			} else {
				launch, err = buildOtherLaunch(settings, input.Snapshots[cell.Harness], o, "matrix-secret-sentinel")
			}
			if err != nil {
				t.Fatal(err)
			}
			wantReasoning := cell.Reasoning
			if wantReasoning == "" {
				wantReasoning = "low"
			}
			if launch.Model != cell.Route || launch.Identity != cell.Identity || launch.Reasoning != wantReasoning {
				t.Fatal("route or effort mismatch")
			}
			yolo := map[string]string{"codex": "--yolo", "claude": "--dangerously-skip-permissions", "opencode": "--auto"}[cell.Harness]
			if slices.Contains(launch.Args, yolo) != (cell.Permissions == "yolo") {
				t.Fatal("permission mapping mismatch")
			}
			var diagnostics bytes.Buffer
			if err := prepareYesod(context.Background(), settings, o, &launch, &diagnostics); err != nil {
				t.Fatal(err)
			}
			if err := prepareSessionCapture(context.Background(), o, &launch, &diagnostics); err != nil {
				t.Fatal(err)
			}
			if launch.lease != nil || diagnostics.Len() != 0 {
				t.Fatal("dry-run allocated a lease or emitted lifecycle diagnostics")
			}
			if cell.Communications == "off" {
				if launch.Yesod != nil {
					t.Fatal("unexpected communications")
				}
			} else if launch.Yesod == nil || launch.Yesod.Agent != "fixture/hrx/matrix" || launch.Yesod.Mail != o.YesodMail || launch.Yesod.BBS != o.YesodBBS {
				t.Fatal("communications mapping mismatch")
			}
			encoded, err := json.Marshal(launch)
			if err != nil || strings.Contains(string(encoded), "matrix-secret-sentinel") {
				t.Fatal("dry-run exposed credential")
			}
		})
	}
	if _, err := os.Stat(os.Getenv("XDG_STATE_HOME")); !os.IsNotExist(err) {
		t.Fatal("dry-run created session state")
	}
}
