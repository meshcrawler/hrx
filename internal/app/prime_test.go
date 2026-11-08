package app

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
)

func TestPrimePrintsGuide(t *testing.T) {
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"prime"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != PrimeGuide {
		t.Fatal("prime output differs from the guide")
	}
	if err := Run(context.Background(), []string{"prime", "extra"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Fatal("extra arguments accepted")
	}
}

func TestPrimeIsDiscoverable(t *testing.T) {
	if !strings.Contains(Help, "  hrx prime\n") {
		t.Fatal("help omits prime")
	}
	if !strings.Contains(ZshCompletion, `"prime:`) {
		t.Fatal("completion omits prime")
	}
}

// Every hrx flag the guide names must still exist in the help text.
func TestPrimeFlagsExistInHelp(t *testing.T) {
	documentedElsewhere := map[string]bool{"--generation": true, "--help": true, "--version": true}
	for _, flag := range regexp.MustCompile(`--[a-z][a-z-]*`).FindAllString(PrimeGuide, -1) {
		if !documentedElsewhere[flag] && !strings.Contains(Help, flag) {
			t.Errorf("guide names %s, which hrx --help does not", flag)
		}
	}
	for _, command := range []string{"run", "network", "remote", "profiles", "doctor", "catalog", "dashboard", "hub"} {
		if !strings.Contains(Help, "hrx "+command) || !strings.Contains(PrimeGuide, "hrx "+command) {
			t.Errorf("hrx %s missing from help or guide", command)
		}
	}
}
