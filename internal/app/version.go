package app

import "runtime/debug"

var buildRevision string

// Version includes build provenance without requiring Git at runtime.
func Version() string {
	result := "hrx " + version
	if buildRevision != "" {
		return result + " (" + buildRevision + ")"
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return result
	}
	var revision string
	var dirty bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if revision != "" {
		result += " (" + revision
		if dirty {
			result += "+dirty"
		}
		result += ")"
	}
	return result
}
