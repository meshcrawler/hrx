package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"github.com/meshcrawler/hrx/internal/monitor"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var brokerIdentity = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,200}$`)
var brokerUsername = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,255}$`)
var brokerSSHHost = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$`)

type BrokerLaunch struct {
	Credential        monitor.Credential `json:"credential"`
	Identity          string             `json:"identity"`
	ProvisionAtLaunch bool               `json:"provision_at_launch"`
}

func prepareBroker(ctx context.Context, o LaunchOptions, launch *Launch) (string, error) {
	if o.Plugin == "" {
		return "", nil
	}
	if !brokerIdentity.MatchString(o.AgentIdentity) {
		return "", errors.New("GitLab requires --agent-identity or a saved session with a broker identity")
	}
	launch.Broker = &BrokerLaunch{Identity: o.AgentIdentity, ProvisionAtLaunch: true}
	if o.DryRun {
		return "", nil
	}
	origin, path, err := brokerConfiguration(o.AgentIdentity)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(origin)
	if err != nil || u == nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("configure broker url in HRX config or HRX_GITLAB_BROKER_URL (HTTPS or loopback HTTP)")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return "", errors.New("broker requires HTTPS or a loopback HTTP address")
	}
	if err := enrollBrokerClient(ctx, strings.TrimRight(u.String(), "/"), o.AgentIdentity, path); err != nil {
		return "", err
	}
	credential, err := requestBrokerCredential(ctx, strings.TrimRight(u.String(), "/"), o.AgentIdentity, path)
	if err != nil {
		return "", err
	}
	launch.Broker.Credential = monitor.Credential{Provider: "gitlab", Host: credential.Host, Username: credential.Username, Profile: credential.Profile, Scopes: credential.Scopes, ExpiresAt: credential.ExpiresAt}
	executable, err := os.Executable()
	if err != nil {
		return "", errors.New("cannot locate HRX credential helper")
	}
	env := launch.Env
	if env == nil {
		env = os.Environ()
	}
	launch.Env = childEnvironment(env, map[string]string{
		envGitlabToken: credential.Token, "GITLAB_HOST": credential.Host,
		"GITLAB_API_HOST": credential.Host, "GLAB_API_PROTOCOL": "https", "API_PROTOCOL": "https", "GLAB_SKIP_TLS_VERIFY": "false", "SKIP_TLS_VERIFY": "false",
		"GLAB_ENABLE_CI_AUTOLOGIN": "false", "GLAB_DEBUG_HTTP": "false", "GLAB_DEBUG": "false",
		envAgentIdentity: o.AgentIdentity,
		// Digest only: lets refresh recognise the launch token without keeping a copy.
		envLaunchTokenSHA256:  launchTokenDigest(credential.Token),
		envLaunchTokenExpires: credential.ExpiresAt,
		"GLAB_NO_PROMPT":      "true",
	}, []string{"GITLAB_URI", "GITLAB_ACCESS_TOKEN", "OAUTH_TOKEN", "GLAB_TOKEN", "CI_JOB_TOKEN", "HRX_GITLAB_BROKER_TOKEN_FILE", "HRX_GITLAB_BROKER_URL"})
	launch.Env, err = gitCredentialEnvironment(launch.Env, executable, credential.Host, credential.Username, credential.SSHHosts)
	if err != nil {
		return "", err
	}
	if launch.Env, err = installGlabShim(launch.Env, executable); err != nil {
		return "", err
	}
	return "\n## GitLab service account\nYour agent identity is " + o.AgentIdentity + ". " +
		"For glab commands use the injected GITLAB_TOKEN and GITLAB_HOST environment; the account is " + credential.Username + " on " + credential.Host + ". " +
		"HRX refreshes this credential for git and glab when the launch token expires; never copy GITLAB_TOKEN into files or other commands. " +
		"Check attribution with `glab api user`. Do not run glab auth login, substitute personal credentials, print tokens, or persist them in files. " +
		"These credentials are for this GitLab host only; do not use them with other hosts or API URLs. " +
		"Git HTTPS authentication is configured noninteractively for this host; configured GitLab SSH remotes are rewritten to HTTPS within this process. " +
		"Use normal git commands without configuring credentials or embedding tokens in URLs. If access fails, report the error rather than switching credentials. " +
		"This account does not expand the user's authorization.\n", nil
}

type brokerCredential struct {
	Identity  string   `json:"identity"`
	Username  string   `json:"username"`
	Token     string   `json:"token"`
	Host      string   `json:"host"`
	ExpiresAt string   `json:"expires_at"`
	UserID    int64    `json:"user_id"`
	SSHHosts  []string `json:"ssh_hosts"`
	Scopes    []string `json:"scopes"`
	Profile   string   `json:"profile"`
}

// requestBrokerCredential issues the identity's current GitLab credential with its
// private client token at path, and validates it before any caller can use it.
func requestBrokerCredential(ctx context.Context, origin, identity, path string) (brokerCredential, error) {
	secret, err := readBrokerSecret(path)
	if err != nil {
		return brokerCredential{}, err
	}
	body, _ := json.Marshal(map[string]string{"identity": identity})
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", origin+"/v1/credentials", bytes.NewReader(body))
	if err != nil {
		return brokerCredential{}, errors.New("invalid broker URL")
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return brokerCredential{}, errors.New("GitLab broker request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return brokerCredential{}, fmt.Errorf("GitLab broker refused credentials (HTTP %d)", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	var credential brokerCredential
	if err != nil || len(raw) > 65536 || json.Unmarshal(raw, &credential) != nil {
		return brokerCredential{}, errors.New("invalid broker response")
	}
	hostURL, hostErr := url.Parse("https://" + credential.Host)
	expiry, expiryErr := time.Parse("2006-01-02", credential.ExpiresAt)
	if credential.Identity != identity || !brokerUsername.MatchString(credential.Username) || credential.UserID <= 0 || credential.Token == "" || strings.ContainsAny(credential.Token, "\x00\r\n") ||
		hostErr != nil || hostURL.Host != credential.Host || hostURL.Hostname() == "" || hostURL.User != nil || hostURL.Path != "" || hostURL.RawQuery != "" || hostURL.Fragment != "" ||
		expiryErr != nil || !expiry.After(time.Now()) {
		return brokerCredential{}, errors.New("invalid broker credential metadata")
	}
	for _, host := range credential.SSHHosts {
		if !brokerSSHHost.MatchString(host) {
			return brokerCredential{}, errors.New("invalid broker SSH host")
		}
	}
	return credential, nil
}

// refreshedBrokerToken is a fresh token for the launch's own identity, host and
// account, or "" when the broker is unavailable or returns anything else. Launch
// tokens expire; long sessions call this from the git helper and the glab shim.
//
// Only the launch token is replaced: a GITLAB_TOKEN the caller set explicitly is
// always used as given, so a deliberately invalid or different token still fails.
func refreshedBrokerToken(l gitlabLaunch) string {
	if !brokerIdentity.MatchString(l.Identity) || l.Host == "" || l.Username == "" || l.LaunchTokenSHA256 == "" ||
		subtle.ConstantTimeCompare([]byte(launchTokenDigest(l.Token)), []byte(l.LaunchTokenSHA256)) != 1 ||
		!launchTokenExpiring(l.LaunchTokenExpires, time.Now()) {
		return ""
	}
	origin, path, err := brokerConfiguration(l.Identity)
	if err != nil {
		return ""
	}
	u, err := url.Parse(origin)
	if err != nil || u == nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return ""
	}
	if ip := net.ParseIP(u.Hostname()); u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	credential, err := requestBrokerCredential(ctx, strings.TrimRight(u.String(), "/"), l.Identity, path)
	if err != nil || credential.Host != l.Host || credential.Username != l.Username {
		return ""
	}
	if run := os.Getenv(monitor.RunEnv); run != "" && os.Getenv("HRX_MONITOR") != "off" {
		if store, err := monitor.Default(); err == nil {
			_ = store.RefreshedCredential(run, credential.Host, credential.Username, credential.ExpiresAt) // Dashboard metadata only.
		}
	}
	return credential.Token
}

// launchTokenExpiring reports whether the launch token is within refreshMargin of
// its expiry date (GitLab expires tokens at the start of that UTC day). Until then
// the broker is not called, so a launch still issues exactly one credential.
func launchTokenExpiring(expires string, now time.Time) bool {
	day, err := time.Parse("2006-01-02", expires)
	return err != nil || !now.Before(day.Add(-refreshMargin))
}

const refreshMargin = 6 * time.Hour

func launchTokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
