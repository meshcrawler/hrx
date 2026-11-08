// hrxfixture runs the production command dispatcher with a private local CA.
// macOS's native certificate verifier does not use SSL_CERT_FILE. Keep this
// transport setup out of the installed launcher and forbid non-loopback IO.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/meshcrawler/hrx/internal/app"
)

func main() {
	cert, err := os.ReadFile(os.Getenv("HRX_INTEGRATION_CA"))
	pool := x509.NewCertPool()
	if err != nil || !pool.AppendCertsFromPEM(cert) {
		fmt.Fprintln(os.Stderr, "integration CA required")
		os.Exit(1)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() {
			return nil, errors.New("integration transport requires loopback IP")
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
	}
	http.DefaultTransport = transport
	if err := app.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "hrx:", err)
		os.Exit(app.ExitCode(err))
	}
}
