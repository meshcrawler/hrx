package monitor

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRefreshedCredentialUpdatesOnlyMatchingGitLabAccount(t *testing.T) {
	s := Store{filepath.Join(t.TempDir(), "monitor")}
	launch := Credential{Provider: "gitlab", Host: "gitlab.example", Username: "hrx_fixture", Profile: "developer", Scopes: []string{"api"}, ExpiresAt: "2026-10-31"}
	r, err := s.Register(Record{Agent: "host/dev/agent", Harness: "claude", Credential: launch})
	if err != nil {
		t.Fatal(err)
	}
	expiry := func() string {
		rows, err := s.Snapshot(context.Background())
		if err != nil || len(rows) != 1 {
			t.Fatal(err)
		}
		return rows[0].Credential.ExpiresAt
	}
	for _, other := range [][2]string{{"other.example", "hrx_fixture"}, {"gitlab.example", "hrx_other"}} {
		if err := s.RefreshedCredential(r.RunID, other[0], other[1], "2026-11-06"); err != nil {
			t.Fatal(err)
		}
	}
	if expiry() != "2026-10-31" {
		t.Fatal("refresh for a different host or account changed the credential")
	}
	if err := s.RefreshedCredential(r.RunID, "gitlab.example", "hrx_fixture", "not-a-date"); err == nil {
		t.Fatal("invalid expiry accepted")
	}
	if err := s.RefreshedCredential(r.RunID, "gitlab.example", "hrx_fixture", "2026-11-06"); err != nil {
		t.Fatal(err)
	}
	if expiry() != "2026-11-06" {
		t.Fatal("refreshed expiry not recorded")
	}
}

func TestRefreshedCredentialNeverCreatesOrConvertsCredential(t *testing.T) {
	s := Store{filepath.Join(t.TempDir(), "monitor")}
	r, err := s.Register(Record{Agent: "host/dev/agent", Harness: "codex", Credential: Credential{Provider: "unmanaged"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshedCredential(r.RunID, "gitlab.example", "hrx_fixture", "2026-11-06"); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshedCredential("0123456789abcdef0123456789abcdef", "gitlab.example", "hrx_fixture", "2026-11-06"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Snapshot(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Credential.Provider != "unmanaged" || rows[0].Credential.ExpiresAt != "" {
		t.Fatal("refresh created or converted a credential", err)
	}
}
