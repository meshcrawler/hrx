package app

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/meshcrawler/hrx/internal/catalog"
)

// The first field is the exact insertion value; descriptions never become argv.
func completeModelDescriptions(out io.Writer, s catalog.Snapshot, harness string) {
	ids := []string{}
	for _, m := range s.Models {
		if _, ok := m.Harnesses[harness]; ok {
			ids = append(ids, m.ID)
		}
	}
	successors := catalog.Successors(ids)
	for _, name := range s.Names(harness) {
		m, err := s.Resolve(name, harness)
		if err != nil {
			continue
		}
		cap := m.Harnesses[harness]
		description := "authorized; declared; " + cap.Verification(time.Now())
		if date := safeTestDate(cap.TestedAt); date != "" && cap.Evidence == "live-tested" {
			description += " " + date
		}
		if newer := successors[m.ID]; newer != "" {
			description += "; newer=" + newer
		}
		// Escape zsh _describe separators without altering the inserted model.
		escape := func(v string) string { return strings.NewReplacer("\\", "\\\\", ":", "\\:").Replace(v) }
		fmt.Fprintf(out, "%s:%s\n", escape(name), escape(description))
	}
}

func completeReasoningDescriptions(out io.Writer, s catalog.Snapshot, harness, name string) {
	m, err := s.Resolve(name, harness)
	if err != nil {
		return
	}
	cap := m.Harnesses[harness]
	for _, level := range cap.Reasoning {
		description := "declared reasoning level"
		if level == cap.DefaultReasoning {
			description += "; default"
		}
		fmt.Fprintf(out, "%s:%s\n", level, description)
	}
}
