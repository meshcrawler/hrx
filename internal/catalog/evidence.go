package catalog

import "time"

// Test evidence ages independently of discovery and authorization refreshes.
const TestEvidenceMaxAge = 7 * 24 * time.Hour

func (c Capability) Verification(now time.Time) string {
	if c.Evidence != "live-tested" {
		return "not-tested"
	}
	if c.TestedAt == "" {
		return "undated-test"
	}
	at, err := time.Parse(time.RFC3339, c.TestedAt)
	if err != nil || at.After(now.Add(time.Minute)) {
		return "invalid-test-date"
	}
	if now.Sub(at) > TestEvidenceMaxAge {
		return "stale-test"
	}
	return "verified"
}
