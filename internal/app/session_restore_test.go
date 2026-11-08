package app

import (
	"path/filepath"
	"testing"

	"github.com/meshcrawler/hrx/internal/session"
)

func TestRestoreSessionSettingsAndOverrides(t *testing.T) {
	for _, harness := range []string{"codex", "claude", "opencode"} {
		t.Run(harness, func(t *testing.T) {
			s := session.Store{Dir: filepath.Join(t.TempDir(), "sessions")}
			r := session.Record{NativeID: "native-id", Harness: harness, Project: "/project", Identity: "model", Route: "provider/model", Reasoning: "high", Mail: true, BBS: true, Agent: "host/hrx/original"}
			if err := s.Save(r); err != nil {
				t.Fatal(err)
			}
			for _, input := range []LaunchOptions{{Resume: r.NativeID}, {Continue: true}} {
				o, saved, err := restoreSession(s, harness, r.Project, input)
				if err != nil || saved == nil || o.Resume != r.NativeID || o.Continue || o.Model != r.Route || o.Reasoning != r.Reasoning || !o.YesodMail || !o.YesodBBS || o.YesodAgent != r.Agent || o.Yolo {
					t.Fatalf("restore: %+v %v", o, err)
				}
				if err := validateRestoredRoute(o, "different-provider/model"); err == nil {
					t.Fatal("silent provider substitution")
				}
			}
			input := LaunchOptions{Resume: r.NativeID, Model: "override", Reasoning: "low", YesodBBS: true, YesodAgent: "host/hrx/override", Yolo: true}
			o, _, err := restoreSession(s, harness, r.Project, input)
			if err != nil || o.Model != input.Model || o.Reasoning != input.Reasoning || o.YesodMail || !o.YesodBBS || o.YesodAgent != input.YesodAgent || !o.Yolo || o.restoredRoute != "" {
				t.Fatalf("override: %+v %v", o, err)
			}
		})
	}
}

func TestRestoreSessionMissingMetadataAndLegacyEscape(t *testing.T) {
	s := session.Store{Dir: filepath.Join(t.TempDir(), "sessions")}
	for _, input := range []LaunchOptions{
		{Resume: "unknown"}, {Continue: true, Model: "explicit"},
		{Resume: "unknown", Model: "explicit", YesodMail: true},
	} {
		if _, _, err := restoreSession(s, "codex", "/project", input); err == nil {
			t.Fatal("missing metadata silently substituted defaults")
		}
	}
	for _, input := range []LaunchOptions{
		{Resume: "unknown", Model: "explicit"},
		{Resume: "unknown", Model: "explicit", YesodMail: true, YesodAgent: "host/hrx/legacy"},
		{},
	} {
		_, saved, err := restoreSession(s, "codex", "/project", input)
		if err != nil || saved != nil {
			t.Fatal("explicit legacy/fresh launch rejected", err)
		}
	}
}

func TestRestoreSessionNewCommsNeedsExplicitIdentity(t *testing.T) {
	s := session.Store{Dir: filepath.Join(t.TempDir(), "sessions")}
	r := session.Record{NativeID: "native", Harness: "claude", Project: "/project", Identity: "model", Route: "route"}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	if _, _, err := restoreSession(s, "claude", r.Project, LaunchOptions{Resume: r.NativeID, YesodMail: true}); err == nil {
		t.Fatal("invented a new identity for resumed session")
	}
}

func TestRestoreSessionGitlabAndPitput(t *testing.T) {
	s := session.Store{Dir: filepath.Join(t.TempDir(), "sessions")}
	r := session.Record{NativeID: "native", Harness: "codex", Project: "/project", Identity: "model", Route: "route", Plugin: "gitlab-service-account-broker", AgentIdentity: "laptop/mayor/agent", Pitput: true, PitputBoards: []string{"dev", "ops"}}
	r.PitputTargeted = true
	r.PitputFollowThreads = []string{"/b/dev/owned"}
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--continue"}, {"--gitlab", "--resume", "native"}} {
		o, err := parseLaunchFor("codex", args)
		if err != nil {
			t.Fatal(err)
		}
		o, _, err = restoreSession(s, "codex", "/project", o)
		if err != nil || o.Plugin != r.Plugin || o.AgentIdentity != r.AgentIdentity || !o.Pitput || o.Yolo {
			t.Fatal("resume lost integration settings or restored permissions", err)
		}
		if len(o.PitputBoards) != 2 || o.PitputBoards[0] != "dev" || o.PitputBoards[1] != "ops" {
			t.Fatal("resume lost selected boards")
		}
		if !o.PitputTargeted || len(o.PitputFollowThreads) != 1 || o.PitputFollowThreads[0] != "/b/dev/owned" {
			t.Fatal("resume broadened targeted selection")
		}
	}
}
