package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// LeaseBroker speaks the existing LiteLLM bootstrap broker contract. Authentication
// is resolved on the controller only; neither bootstrap tokens nor broker URLs
// containing credentials are accepted in a guest configuration.
type LeaseBroker struct {
	URL          string
	Authenticate func(context.Context) (string, error)
	Client       *http.Client
}

type LeaseRequest struct {
	RequestID           string   `json:"-"`
	ClientID            string   `json:"client_id"`
	Purpose             string   `json:"purpose"`
	Models              []string `json:"models"`
	CredentialProfile   string   `json:"credential_profile,omitempty"`
	TTLSeconds          int      `json:"ttl_seconds"`
	MaxBudgetUSD        float64  `json:"max_budget_usd"`
	RPMLimit            int      `json:"rpm_limit"`
	TPMLimit            int      `json:"tpm_limit"`
	MaxParallelRequests int      `json:"max_parallel_requests"`
}

type CredentialLease struct {
	ID               string    `json:"lease_id"`
	ExpiresAt        time.Time `json:"expires_at"`
	Model            string    `json:"model"`
	OpenAIBaseURL    string    `json:"openai_base_url"`
	AnthropicBaseURL string    `json:"anthropic_base_url"`
	key              string
	oauth            string
}

func (l CredentialLease) String() string {
	return fmt.Sprintf("credential lease %s expires %s (secrets redacted)", l.ID, l.ExpiresAt.Format(time.RFC3339))
}
func (l CredentialLease) GoString() string { return l.String() }

// PrivateJSON is explicitly secret-bearing. Send only through private stdin/files.
// Normal JSON/log formatting of CredentialLease never contains either credential.
func (l CredentialLease) PrivateJSON() ([]byte, error) {
	if l.key == "" {
		return nil, errors.New("lease has no validated credential")
	}
	return json.Marshal(map[string]any{"lease_id": l.ID, "expires_at": l.ExpiresAt, "key": l.key, "claude_code_oauth_token": l.oauth})
}

var uuidPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var brokerIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
var routePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,239}$`)

func secureURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func (b LeaseBroker) request(ctx context.Context, method, path, id string, payload []byte) ([]byte, error) {
	if !secureURL(b.URL) || b.Authenticate == nil {
		return nil, errors.New("configure an HTTPS credential broker and controller authentication")
	}
	token, err := b.Authenticate(ctx)
	if err != nil || token == "" || strings.ContainsAny(token, "\r\n \t") {
		return nil, errors.New("credential broker authentication unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(b.URL, "/")+path, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("invalid credential broker request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if id != "" {
		req.Header.Set("Idempotency-Key", id)
	}
	client := http.Client{Timeout: 30 * time.Second}
	if b.Client != nil {
		client = *b.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.Timeout == 0 {
		client.Timeout = 30 * time.Second
	}
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("credential broker request failed; retry with the same request ID")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("credential broker returned HTTP %d (body suppressed)", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(data) > 65536 {
		return nil, errors.New("credential broker response incomplete or oversized")
	}
	return data, nil
}

func (b LeaseBroker) Mint(ctx context.Context, r LeaseRequest) (CredentialLease, error) {
	var lease CredentialLease
	if !uuidPattern.MatchString(r.RequestID) || !brokerIdentifierPattern.MatchString(r.ClientID) || !brokerIdentifierPattern.MatchString(r.Purpose) || len(r.Models) != 1 || !routePattern.MatchString(r.Models[0]) || r.TTLSeconds < 60 || r.TTLSeconds > 86400 || r.MaxBudgetUSD <= 0 || math.IsNaN(r.MaxBudgetUSD) || math.IsInf(r.MaxBudgetUSD, 0) || r.RPMLimit < 1 || r.TPMLimit < 1 || r.MaxParallelRequests < 1 {
		return lease, errors.New("invalid bounded single-model credential lease request")
	}
	if r.CredentialProfile != "" && (r.CredentialProfile != "claude-code" || !strings.HasPrefix(r.Models[0], "claude-")) {
		return lease, errors.New("invalid Claude credential profile")
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return lease, err
	}
	data, err := b.request(ctx, http.MethodPost, "/v1/leases", r.RequestID, payload)
	if err != nil {
		return lease, err
	}
	var wire struct {
		ID      string    `json:"lease_id"`
		Key     string    `json:"key"`
		Expires time.Time `json:"expires_at"`
		Models  []string  `json:"models"`
		Limits  struct {
			Budget   float64 `json:"max_budget_usd"`
			RPM      int     `json:"rpm_limit"`
			TPM      int     `json:"tpm_limit"`
			Parallel int     `json:"max_parallel_requests"`
		} `json:"limits"`
		Gateway struct {
			OpenAI    string `json:"openai_base_url"`
			Anthropic string `json:"anthropic_base_url"`
		} `json:"gateway"`
		Credentials struct {
			Profile string `json:"profile"`
			OAuth   string `json:"claude_code_oauth_token"`
		} `json:"credentials"`
	}
	if json.Unmarshal(data, &wire) != nil {
		return lease, errors.New("invalid credential broker response")
	}
	// Preserve a valid ID on rejection so the controller can revoke a bad grant.
	if uuidPattern.MatchString(wire.ID) {
		lease.ID = wire.ID
	}
	now := time.Now()
	if lease.ID == "" || wire.Key == "" || strings.ContainsAny(wire.Key, "\r\n \t") || len(wire.Models) != 1 || wire.Models[0] != r.Models[0] || !wire.Expires.After(now.Add(15*time.Second)) || wire.Expires.After(now.Add(time.Duration(r.TTLSeconds+60)*time.Second)) || !secureURL(wire.Gateway.OpenAI) || !secureURL(wire.Gateway.Anthropic) || wire.Limits.Budget <= 0 || wire.Limits.Budget > r.MaxBudgetUSD || wire.Limits.RPM < 1 || wire.Limits.RPM > r.RPMLimit || wire.Limits.TPM < 1 || wire.Limits.TPM > r.TPMLimit || wire.Limits.Parallel < 1 || wire.Limits.Parallel > r.MaxParallelRequests {
		return lease, errors.New("credential lease does not satisfy requested scope, expiry or limits")
	}
	if r.CredentialProfile == "claude-code" && (wire.Credentials.Profile != "claude-code" || !strings.HasPrefix(wire.Credentials.OAuth, "sk-ant-oat")) {
		return lease, errors.New("Claude lease missing subscription credential")
	}
	lease.ExpiresAt = wire.Expires
	lease.Model = wire.Models[0]
	lease.OpenAIBaseURL = wire.Gateway.OpenAI
	lease.AnthropicBaseURL = wire.Gateway.Anthropic
	lease.key = wire.Key
	if r.CredentialProfile == "claude-code" {
		lease.oauth = wire.Credentials.OAuth
	}
	return lease, nil
}

func (b LeaseBroker) Revoke(ctx context.Context, id string) error {
	if !uuidPattern.MatchString(id) {
		return errors.New("invalid credential lease ID")
	}
	_, err := b.request(ctx, http.MethodDelete, "/v1/leases/"+id, "", nil)
	return err
}
