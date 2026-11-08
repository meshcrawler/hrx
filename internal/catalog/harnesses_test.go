package catalog

import "testing"

func TestMultiHarnessCapabilities(t *testing.T) {
	d, a, now := fixture(t)
	caps := d.Models[0].Sources[0].Details.Harnesses
	caps["claude"] = Capability{WireAPI: "messages"}
	caps["opencode"] = Capability{WireAPI: "chat_completions"}
	s, err := Join(d, a, "binding", now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Validate("binding", now, false); err != nil {
		t.Fatal(err)
	}
	for _, harness := range []string{"codex", "claude", "opencode"} {
		if len(s.Names(harness)) != 1 {
			t.Fatal("lost harness", harness)
		}
	}
	caps["claude"] = Capability{WireAPI: "responses"}
	if _, err = Join(d, a, "binding", now); err == nil {
		t.Fatal("wrong protocol accepted")
	}
}

func TestFailoverLimitsDoNotConflict(t *testing.T) {
	d, a, now := fixture(t)
	source := d.Models[0].Sources[0]
	source.Details.Context, source.Details.Output = 100000, 20000
	d.Models[0].Sources[0] = source
	source.Details.Context, source.Details.Output = 50000, 10000
	d.Models[0].Sources = append(d.Models[0].Sources, source)
	s, err := Join(d, a, "binding", now)
	if err != nil || len(s.Models) != 1 {
		t.Fatal("failover alias rejected", err)
	}
	if s.Models[0].Context != 50000 || s.Models[0].Output != 10000 {
		t.Fatal("nonconservative failover limits")
	}
}
