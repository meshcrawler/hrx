package backend

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

type transportFunc func(context.Context, string, []string, io.Reader) ([]byte, error)

func (f transportFunc) Run(c context.Context, h string, a []string, r io.Reader) ([]byte, error) {
	return f(c, h, a, r)
}

func TestProxmoxRejectsWrongBinding(t *testing.T) {
	for _, response := range []string{`garbage`, `{"ok":true,"deployment":{"schema":1,"deployment":"other"}}`, `{"ok":true,"deployment":{"schema":1,"deployment":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","vmid":100,"node":"host","digest":"wrong","marker":"hrx:v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:wrong"}}`} {
		backend := Proxmox{Host: "host", Transport: transportFunc(func(_ context.Context, host string, args []string, r io.Reader) ([]byte, error) {
			if host != "host" || len(args) != 3 || args[0] != "python3" || args[2] != proxmoxHelper {
				t.Fatal("wrong host helper")
			}
			var request map[string]any
			if json.NewDecoder(r).Decode(&request) != nil || request["operation"] != "ensure" {
				t.Fatal("invalid request")
			}
			return []byte(response), nil
		})}
		if _, err := backend.Reconcile(context.Background(), "ensure", strings.Repeat("a", 32), ContainerSpec{}); err == nil {
			t.Fatal("accepted unbound response")
		}
	}
}

func TestProxmoxRefusalIsActionable(t *testing.T) {
	p := Proxmox{Transport: transportFunc(func(context.Context, string, []string, io.Reader) ([]byte, error) {
		return []byte(`{"ok":false,"error":"container ownership or isolation mismatch; refusing mutation"}`), nil
	})}
	if _, err := p.Reconcile(context.Background(), "destroy", strings.Repeat("a", 32), ContainerSpec{}); err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("lost refusal: %v", err)
	}
}
