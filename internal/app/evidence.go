package app

import "time"

func safeTestDate(value string) string {
	if at, err := time.Parse(time.RFC3339, value); err == nil {
		return at.UTC().Format(time.RFC3339)
	}
	return ""
}

func evidenceBadge(verification string) string {
	switch verification {
	case "verified":
		return "V"
	case "undated-test", "invalid-test-date":
		return "U"
	case "stale-test":
		return "S"
	default:
		return "D"
	}
}

func accessLabel(a CatalogAccess) string {
	label := a.Status
	if label == "ready" {
		label += "/" + evidenceBadge(a.Verification)
	}
	if a.Cached {
		label = "cached/" + label
	}
	return label
}
