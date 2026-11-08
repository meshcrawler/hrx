package app

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"time"
)

// Explicit custom roots work with Darwin's verifier as well as Unix CA files.
// Extend trust only on this client; never mutate the process default transport.
func gatewayHTTPClient(timeout time.Duration) (*http.Client, error) {
	client := &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("redirect refused")
	}}
	path := os.Getenv("HRX_CA_FILE")
	if path == "" {
		return client, nil
	}
	cert, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read HRX_CA_FILE")
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, errors.New("cannot load system certificate roots")
	}
	if !pool.AppendCertsFromPEM(cert) {
		return nil, errors.New("HRX_CA_FILE contains no valid certificates")
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("custom CA requires an HTTP transport")
	}
	transport := base.Clone()
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	transport.TLSClientConfig.RootCAs = pool
	transport.TLSClientConfig.InsecureSkipVerify = false
	client.Transport = transport
	return client, nil
}
