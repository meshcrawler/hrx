package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNamespaceAuthorization(t *testing.T) {
	var auth Authorization
	json.Unmarshal([]byte(`{"data":[{"id":"or-team/*"},{"id":"exact"},{"id":"unsafe*"}]}`), &auth)
	for _, route := range []string{"or-team/vendor/model", "exact"} {
		if !Authorized(auth, route) {
			t.Fatal("valid scope rejected", route)
		}
	}
	for _, route := range []string{"or-other/vendor/model", "or-team-evil/model", "unsafe-other", "or-team/*", ""} {
		if Authorized(auth, route) {
			t.Fatal("scope widened", route)
		}
	}
}

func TestHyphenNamespaceAuthorization(t *testing.T) {
	var auth Authorization
	json.Unmarshal([]byte(`{"data":[{"id":"fw-acme-*"},{"id":"fw-fireworks-contoso-*"},{"id":"unsafe*"},{"id":"*"},{"id":"-*"},{"id":"bad*-*"}]}`), &auth)
	for _, route := range []string{"fw-acme-glm-5p3", "fw-fireworks-contoso-kimi-k3"} {
		if !Authorized(auth, route) {
			t.Fatal("valid namespace rejected", route)
		}
	}
	for _, route := range []string{"fw-other-glm-5p3", "fw-acme2-glm-5p3", "fw-acme", "unsafe-model", "anything", "bad-extra-model", "fw-acme-*"} {
		if Authorized(auth, route) {
			t.Fatal("scope widened", route)
		}
	}
	d, _, now := fixture(t)
	d.Models[0].Sources[0].Route = "fw-acme-test"
	s, err := Join(d, auth, "binding", now)
	if err != nil || len(s.Models) != 1 {
		t.Fatal("grant not joined", err)
	}
	d.Models[0].Sources[0].Details.Harnesses = nil
	s, err = Join(d, auth, "binding", now)
	if err != nil || len(s.Models) != 0 || s.Inventory[0].Status("codex") != "compatibility-unverified" {
		t.Fatal("authorization bypassed capability checks", err)
	}
}

func TestInventoryRetainsExcludedModels(t *testing.T) {
	d, a, now := fixture(t)
	for _, name := range []string{"not-authorized", "compatibility-unverified", "no-gateway-route", "unavailable"} {
		m := Identity{ID: name, Available: true, Sources: []Source{{Route: name, Available: true, Provider: "test"}}}
		switch name {
		case "no-gateway-route":
			m.Sources[0].Route = ""
		case "unavailable":
			m.Sources[0].Available = false
		case "compatibility-unverified":
			a.Data = append(a.Data, a.Data[0])
			a.Data[len(a.Data)-1].ID = name
		}
		d.Models = append(d.Models, m)
	}
	s, err := Join(d, a, "binding", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Models) != 1 || len(s.Inventory) != 5 {
		t.Fatal("missing inventory or leaked launchable models")
	}
	for _, e := range s.Inventory {
		want := e.ID
		if want == "test-model" {
			want = "ready"
		}
		if got := e.Status("codex"); got != want {
			t.Fatalf("%s: %s want %s", e.ID, got, want)
		}
	}
	data, _ := json.Marshal(s.Inventory)
	if strings.Contains(string(data), "auth.command") {
		t.Fatal("unexpected auth metadata")
	}
}

func TestWildcardModelsJoinOnlyDiscoveredRoutes(t *testing.T) {
	d, a, now := fixture(t)
	a.Data[0].ID = "provider/*"
	s, err := Join(d, a, "binding", now)
	if err != nil || len(s.Models) != 1 {
		t.Fatal("wildcard discovery failed", err)
	}
	d.Models[0].Sources[0].Route = "another/test-model"
	s, err = Join(d, a, "binding", now)
	if err != nil || len(s.Models) != 0 {
		t.Fatal("wildcard crossed namespace", err)
	}
}
