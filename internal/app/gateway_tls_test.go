package app

import (
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGatewayCustomCA(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	t.Setenv("HRX_CA_FILE", "")
	plain, err := gatewayHTTPClient(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := plain.Get(server.URL); err == nil {
		resp.Body.Close()
		t.Fatal("private certificate trusted without explicit CA")
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRX_CA_FILE", path)
	client, err := gatewayHTTPClient(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	transport := client.Transport.(*http.Transport)
	if transport == http.DefaultTransport || transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("custom trust mutated shared transport or disabled verification")
	}
	client.CloseIdleConnections()
	transport.TLSClientConfig.ServerName = "wrong-host.invalid"
	if response, err := client.Get(server.URL); err == nil {
		response.Body.Close()
		t.Fatal("custom CA bypassed hostname verification")
	}
}

func TestGatewayCustomCAInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	for _, test := range []struct{ name, data, want string }{
		{"missing", "", "cannot read HRX_CA_FILE"},
		{"malformed", "not a certificate", "no valid certificates"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.data != "" {
				if err := os.WriteFile(path, []byte(test.data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HRX_CA_FILE", path)
			client, err := gatewayHTTPClient(time.Second)
			if client != nil || err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected closed failure %q, got client=%v err=%v", test.want, client, err)
			}
		})
	}
}
