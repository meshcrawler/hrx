package catalog

import (
	"testing"
	"time"
)

func TestVerificationNeverConfusesDiscoveryWithLiveTests(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct{ evidence, date, want string }{
		{"", now.Format(time.RFC3339), "not-tested"},
		{"provider-declared", now.Format(time.RFC3339), "not-tested"},
		{"live-tested", "", "undated-test"},
		{"live-tested", "not-a-date", "invalid-test-date"},
		{"live-tested", now.Add(time.Hour).Format(time.RFC3339), "invalid-test-date"},
		{"live-tested", now.Add(-8 * 24 * time.Hour).Format(time.RFC3339), "stale-test"},
		{"live-tested", now.Add(-time.Hour).Format(time.RFC3339), "verified"},
	} {
		if got := (Capability{Evidence: tc.evidence, TestedAt: tc.date}).Verification(now); got != tc.want {
			t.Errorf("%+v: %s", tc, got)
		}
	}
}

func TestSuccessorExplanationNumericFamilies(t *testing.T) {
	got := Successors([]string{"gpt-5.9-sol", "gpt-5.10-sol", "gpt-5.10-terra", "gpt-5.9-sol-20260101", "unknown", "glm-5p2", "glm-5p3"})
	if len(got) != 2 || got["gpt-5.9-sol"] != "gpt-5.10-sol" || got["glm-5p2"] != "glm-5p3" {
		t.Fatal(got)
	}
}
