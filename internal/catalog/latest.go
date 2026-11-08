package catalog

import (
	"regexp"
	"strconv"
	"strings"
)

var generationPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^(gpt-)([0-9]+(?:\.[0-9]+)*)(-[a-z][a-z0-9-]*)?$`),
	regexp.MustCompile(`^(claude-(?:opus|sonnet|haiku|fable)-)([0-9]+(?:[.-][0-9]+)*)(-[a-z][a-z0-9-]*)?$`),
	regexp.MustCompile(`^((?:glm-|kimi-k|deepseek-v|minimax-m))([0-9]+(?:[.p][0-9]+)*)(-[a-z][a-z0-9-]*)?$`),
}
var datedModel = regexp.MustCompile(`(?:-[0-9]{8}|-[0-9]{4}-[0-9]{2}-[0-9]{2})(?:$|-)`)

func generation(id string) (string, []int) {
	// Dated snapshots, previews and distinct suffixes are not interchangeable.
	if datedModel.MatchString(id) {
		return "", nil
	}
	for _, pattern := range generationPatterns {
		match := pattern.FindStringSubmatch(id)
		if match == nil {
			continue
		}
		parts := strings.FieldsFunc(match[2], func(r rune) bool { return r == '.' || r == 'p' || r == '-' })
		version := make([]int, len(parts))
		for i, part := range parts {
			n, err := strconv.Atoi(part)
			if err != nil {
				return "", nil
			}
			version[i] = n
		}
		return match[1] + "{version}" + match[3], version
	}
	return "", nil
}

func newer(a, b []int) bool {
	for i := 0; i < max(len(a), len(b)); i++ {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// Superseded uses versioned names as a conservative display heuristic, not an
// upstream deprecation claim. Unknown naming schemes remain visible.
func Superseded(ids []string) map[string]bool {
	best := map[string][]int{}
	for _, id := range ids {
		family, v := generation(id)
		if family != "" && (best[family] == nil || newer(v, best[family])) {
			best[family] = v
		}
	}
	result := map[string]bool{}
	for _, id := range ids {
		family, v := generation(id)
		if family != "" && newer(best[family], v) {
			result[id] = true
		}
	}
	return result
}

// Successors explains the same-family name heuristic, not provider deprecation.
func Successors(ids []string) map[string]string {
	type candidate struct {
		id      string
		version []int
	}
	best := map[string]candidate{}
	for _, id := range ids {
		family, v := generation(id)
		old := best[family]
		if family != "" && (old.id == "" || newer(v, old.version) || (!newer(old.version, v) && id < old.id)) {
			best[family] = candidate{id, v}
		}
	}
	result := map[string]string{}
	for _, id := range ids {
		family, v := generation(id)
		if choice := best[family]; family != "" && newer(choice.version, v) {
			result[id] = choice.id
		}
	}
	return result
}

func (s Snapshot) Latest(harness string) Snapshot {
	ids := []string{}
	for _, m := range s.Models {
		if _, ok := m.Harnesses[harness]; ok {
			ids = append(ids, m.ID)
		}
	}
	hidden := Superseded(ids)
	models := make([]Model, 0, len(s.Models))
	for _, m := range s.Models {
		if !hidden[m.ID] {
			models = append(models, m)
		}
	}
	s.Models = models
	entries := make([]Entry, 0, len(s.Inventory))
	for _, e := range s.Inventory {
		if !hidden[e.ID] {
			entries = append(entries, e)
		}
	}
	s.Inventory = entries
	return s
}
