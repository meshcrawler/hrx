package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/meshcrawler/hrx/internal/catalog"
)

const doctorHelp = `hrx doctor [--harness all|codex|claude|opencode] [--model NAME]
           [--reasoning LEVEL] [--yesod-mail|--yesod-bbs|--yesod-comms]
           [--json] [--timeout SECONDS]
Read-only checks; no inference, registration, cache writes or credential changes.
Missing harnesses are optional with --harness all (default).
Exit codes: 0 = no failed checks (warnings possible), 1 = failed checks, 2 = usage.
Timeout: 1..60 seconds total (default 45); subprocesses and HTTP also bounded.
`

type exitError struct {
	code    int
	message string
}

func (e *exitError) Error() string { return e.message }
func ExitCode(err error) int {
	var e *exitError
	if errors.As(err, &e) {
		return e.code
	}
	if err != nil {
		return 1
	}
	return 0
}

type doctorOptions struct {
	harness, model, reasoning string
	json, yesod               bool
	timeout                   time.Duration
}

type DoctorCheck struct {
	Harness string `json:"harness,omitempty"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	State   string `json:"state"`
	Detail  string `json:"detail"`
	Remedy  string `json:"remedy,omitempty"`
}
type DoctorReport struct {
	SchemaVersion int           `json:"schema_version"`
	CheckedAt     time.Time     `json:"checked_at"`
	OK            bool          `json:"ok"`
	Checks        []DoctorCheck `json:"checks"`
}

type doctorDeps struct {
	load       func(string) (Settings, error)
	lookup     func(string) (string, error)
	run        func(context.Context, string, ...string) ([]byte, error)
	credential func(context.Context, Provider) (string, error)
	client     *http.Client
}

func doctorCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var output limitedBuffer
	cmd.Stdout, cmd.Stderr, cmd.WaitDelay = &output, io.Discard, time.Second
	err := cmd.Run()
	return output.Bytes(), err
}

func parseDoctor(args []string) (doctorOptions, error) {
	o := doctorOptions{harness: "all", timeout: 45 * time.Second}
	bad := func() (doctorOptions, error) {
		return o, &exitError{2, "invalid doctor options; run hrx doctor --help"}
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			o.json = true
		case "--yesod-mail", "--yesod-bbs", "--yesod-comms":
			o.yesod = true
		case "--harness", "--model", "--reasoning", "--timeout":
			flag := args[i]
			i++
			if i == len(args) || args[i] == "" || strings.HasPrefix(args[i], "--") {
				return bad()
			}
			switch flag {
			case "--harness":
				o.harness = args[i]
			case "--model":
				o.model = args[i]
			case "--reasoning":
				o.reasoning = args[i]
			case "--timeout":
				n, err := strconv.Atoi(args[i])
				if err != nil || n < 1 || n > 60 {
					return bad()
				}
				o.timeout = time.Duration(n) * time.Second
			}
		default:
			return bad()
		}
	}
	if !slices.Contains([]string{"all", "codex", "claude", "opencode"}, o.harness) || (o.harness == "all" && (o.model != "" || o.reasoning != "")) {
		return bad()
	}
	return o, nil
}

func runDoctor(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 1 && args[0] == "--help" {
		_, err := fmt.Fprint(out, doctorHelp)
		return err
	}
	o, err := parseDoctor(args)
	if err != nil {
		return err
	}
	client, err := gatewayHTTPClient(5 * time.Second)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	d := doctorDeps{load: func(h string) (Settings, error) { return loadHarnessSettings(h) }, lookup: exec.LookPath, run: doctorCommand, credential: credential, client: client}
	report := diagnose(ctx, o, d)
	if o.json {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(out, "hrx doctor: read-only; no inference performed")
		for _, c := range report.Checks {
			fmt.Fprintf(out, "%-4s %-9s %-14s %s: %s\n", strings.ToUpper(c.Status), c.Harness, c.Name, c.State, c.Detail)
			if c.Remedy != "" {
				fmt.Fprintln(out, "     Fix:", c.Remedy)
			}
		}
	}
	if !report.OK {
		return &exitError{1, "doctor found failed checks"}
	}
	return nil
}

// Only fixed descriptions and parsed semantic versions are emitted. Never relay
// subprocess diagnostics, HTTP bodies, URLs, credential handles or config errors.
func diagnose(ctx context.Context, o doctorOptions, d doctorDeps) DoctorReport {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	r := DoctorReport{SchemaVersion: 1, CheckedAt: time.Now().UTC(), OK: true, Checks: []DoctorCheck{}}
	add := func(h, name, status, state, detail, remedy string) {
		r.Checks = append(r.Checks, DoctorCheck{h, name, status, state, detail, remedy})
		if status == "fail" {
			r.OK = false
		}
	}
	harnesses := []string{"codex", "claude", "opencode"}
	if o.harness != "all" {
		harnesses = []string{o.harness}
	}
	found := 0
	for _, h := range harnesses {
		path, err := d.lookup(h)
		if err != nil {
			status := "warn"
			if o.harness != "all" {
				status = "fail"
			}
			add(h, "executable", status, "unavailable", "Harness executable not on PATH.", "Install the harness or correct PATH.")
			continue
		}
		found++
		add(h, "executable", "ok", "available", "Harness executable found.", "")
		data, err := d.run(ctx, path, "--version")
		version := ""
		if err == nil {
			version = regexp.MustCompile(`(?m)^(?:(?:codex-cli|codex|claude|opencode) )?v?([0-9]+\.[0-9]+\.[0-9]+)(?:\s|$)`).FindString(string(data))
		}
		if version == "" {
			add(h, "version", "warn", "unknown", "Version unavailable or unrecognized.", "Check the installed harness version manually.")
		} else {
			number := regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+`).FindString(version)
			add(h, "version", "ok", "known", number, "")
		}
		s, err := d.load(h)
		if err != nil {
			remedy := "Check CODEX_HOME/config.toml or HRX_CONFIG and the credential handle; see README configuration."
			if h == "claude" {
				remedy = "Add [claude.provider] (base_url and one credential handle) to the HRX configuration; see README configuration."
			}
			add(h, "configuration", "fail", "invalid", "Cannot load a supported gateway configuration.", remedy)
			continue
		}
		add(h, "configuration", "ok", "valid", "Gateway configuration parsed.", "")
		if h == "claude" {
			if err := checkClaudeRoutingSettings(""); err != nil {
				add(h, "routing", "fail", "conflict", "Claude settings conflict with isolated gateway routing.", "Remove routing/auth overrides or use a separate CLAUDE_CONFIG_DIR.")
			}
		}
		cached, err := catalog.Read(s.CachePath)
		if err != nil {
			add(h, "cache", "warn", "unavailable", "No readable catalog cache.", "Run hrx refresh --harness "+h+" to enable completion.")
		} else if err = cached.Validate(s.Binding, time.Now(), true); err != nil {
			add(h, "cache", "warn", "invalid", "Cache schema, binding or capabilities invalid.", "Run hrx refresh --harness "+h)
		} else if !catalog.Fresh(cached.RefreshedAt, time.Now()) || !catalog.Fresh(cached.GeneratedAt, time.Now()) {
			add(h, "cache", "warn", "stale", "Completion cache is stale; launch will revalidate.", "Run hrx refresh --harness "+h)
		} else {
			add(h, "cache", "ok", "fresh", "Local catalog and authorization timestamps are fresh.", "")
		}
		var dashboard catalog.Dashboard
		catalogErr := getJSON(ctx, d.client, s.CatalogURL, "", &dashboard)
		if catalogErr != nil {
			add(h, "gateway", "fail", "unavailable", "Cannot read gateway catalog (network, TLS, HTTP or JSON error).", "Check gateway connectivity/TLS and dashboard health.")
		} else {
			add(h, "gateway", "ok", "reachable", "Gateway catalog endpoint reachable.", "")
		}
		if s.Provider.Auth.Command != "" {
			if _, err := d.lookup(s.Provider.Auth.Command); err != nil {
				add(h, "credential", "fail", "unavailable", "Credential helper not on PATH.", "Install/configure the credential helper; no key rotation is needed.")
				continue
			}
		}
		key, err := d.credential(ctx, s.Provider)
		if err != nil {
			add(h, "credential", "fail", "unavailable", "Credential lookup failed; output suppressed.", "Check credential environment/file or helper authentication (including SSH).")
			continue
		}
		add(h, "credential", "ok", "available", "Credential resolved in memory; value suppressed.", "")
		var auth catalog.Authorization
		if err := getJSON(ctx, d.client, s.Provider.BaseURL+"/models", key, &auth); err != nil {
			state, detail, remedy := "unavailable", "Cannot check gateway key authorization.", "Check gateway connectivity and service health."
			var status *gatewayHTTPError
			if errors.As(err, &status) {
				switch status.Code {
				case 401, 403:
					state, detail, remedy = "not-authorized", "Gateway rejected this key (HTTP 401/403).", "Check the existing key expiry, budget and authorization; do not rotate automatically."
				case 429:
					state, detail, remedy = "rate-limited", "Gateway rate-limited authorization check.", "Wait and retry; this does not establish model incompatibility."
				}
			}
			add(h, "authorization", "fail", state, detail, remedy)
			continue
		}
		if auth.Data == nil {
			add(h, "authorization", "fail", "unknown", "Gateway returned no recognizable model list.", "Check the gateway /v1/models response schema.")
			continue
		}
		add(h, "authorization", "ok", "authorized", "Existing key accepted; route grants read (not an inference test).", "")
		if catalogErr != nil {
			continue
		}
		if dashboard.Models == nil {
			add(h, "catalog", "fail", "invalid", "Shared catalog has no recognizable models array.", "Check dashboard schema and discovery output.")
			continue
		}
		snap, err := catalog.Join(dashboard, auth, s.Binding, time.Now())
		if err != nil {
			add(h, "catalog", "fail", "invalid", "Shared catalog is stale, unhealthy, conflicting or has unsupported capabilities/schema.", "Check dashboard discovery and shared harness policy, then refresh.")
			continue
		}
		add(h, "catalog", "ok", "valid", "Fresh shared catalog joined with this key's authorization.", "")
		if h == "codex" {
			if _, skipped, err := codexCatalogContent(s, snap); err != nil {
				add(h, "templates", "fail", "invalid", "Codex launch templates are unreadable, invalid or missing an authorized model.", "Check model_catalog_json and refresh the installed Codex model templates.")
			} else if len(skipped) > 0 {
				add(h, "templates", "warn", "incomplete", fmt.Sprintf("Codex catalog skipped %d authorized route(s) with unknown context capacity: %s.", len(skipped), strings.Join(skipped, ", ")), "Publish context limits on the gateway catalog, then refresh.")
			} else {
				add(h, "templates", "ok", "valid", "All authorized Codex routes have readable launch templates; no generated files written.", "")
			}
		}
		name := o.model
		if name == "" {
			name = s.DefaultModel
		}
		if name == "" {
			add(h, "model", "warn", "unknown", fmt.Sprintf("%d authorized and declared-compatible choices; no default selected.", len(snap.Names(h))), "Choose --model using hrx models --harness "+h)
			continue
		}
		m, err := snap.Resolve(name, h)
		if err != nil {
			state := "unknown"
			for _, e := range snap.Inventory {
				if e.Route == name || e.ID == name {
					state = e.Status(h)
					if state == "ready" {
						state = "ambiguous"
						break
					}
				}
			}
			add(h, "model", "fail", state, "Selected/default model cannot be resolved for this harness.", "Use hrx catalog --details --harness "+h+" and select an exact authorized compatible route.")
			continue
		}
		cap := m.Harnesses[h]
		add(h, "model", "ok", "declared-compatible", "Selected/default route is authorized with declared harness capabilities.", "")
		if o.reasoning != "" && !slices.Contains(cap.Reasoning, o.reasoning) {
			add(h, "reasoning", "fail", "unsupported", "Requested reasoning level is not advertised for this route/harness.", "Use --reasoning completion to choose a supported level.")
		}
		state := cap.Verification(time.Now())
		status := "warn"
		detail := "No recent dated end-to-end harness test is recorded; authorization is not verification."
		if state == "verified" {
			status = "ok"
			detail = "Shared policy records a live test at " + safeTestDate(cap.TestedAt) + "; provider uptime is not guaranteed."
		}
		add(h, "verification", status, state, detail, "")
	}
	if found == 0 {
		add("", "harnesses", "fail", "unavailable", "No requested harness is installed.", "Install at least one supported harness.")
	}
	if o.yesod {
		if _, err := d.lookup("python3"); err != nil {
			add("", "yesod-helper", "fail", "unavailable", "Python 3 required by the hrx identity-selection helper is missing.", "Install Python 3 or correct PATH.")
		}
		if path, err := d.lookup("yesod"); err != nil {
			add("", "yesod", "fail", "unavailable", "Yesod CLI not found.", "Install Yesod before using communications.")
		} else {
			leasesAvailable := true
			for _, operation := range []string{"claim", "renew", "release"} {
				help, err := d.run(ctx, path, "mail", operation, "--help")
				if err != nil || !strings.Contains(string(help), "--token-stdin") || !strings.Contains(string(help), "--as") {
					leasesAvailable = false
				}
			}
			if leasesAvailable {
				add("", "yesod-leases", "ok", "supported", "Claim, renew and release CLI contracts available; lease operations not exercised.", "")
			} else {
				add("", "yesod-leases", "fail", "unsupported", "Required identity-lease CLI commands unavailable or unrecognized.", "Install a Yesod release with mail claim, renew and release supporting --token-stdin before using communications.")
			}
			data, err := d.run(ctx, path, "mail", "roster", "--json")
			var rows []map[string]json.RawMessage
			if err != nil || json.Unmarshal(data, &rows) != nil || rows == nil {
				add("", "yesod", "fail", "unavailable", "Cannot read Yesod registry; diagnostics suppressed.", "Check yesod mail roster --json and registry connectivity.")
			} else {
				add("", "yesod", "ok", "reachable", "Mail registry readable; no identity registered or heartbeat sent.", "")
			}
		}
	}
	if ctx.Err() != nil {
		add("", "deadline", "fail", "timeout", "Doctor reached its total deadline.", "Fix slow checks or retry with --timeout up to 60 seconds.")
	}
	return r
}
