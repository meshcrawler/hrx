package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/meshcrawler/hrx/internal/backend"
	"github.com/meshcrawler/hrx/internal/monitor"
)

// hrx sweep finds what hrx left behind: dashboard rows for agents that are gone,
// collector sources that stopped answering, and HRX-owned containers that are
// stopped or idle. It only prints a plan. Each change needs an explicit --apply ID
// taken from a fresh plan, and a container is always backed up before it is
// destroyed.

type sweepFinding struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Subject    string `json:"subject"`
	Reason     string `json:"reason"`
	Action     string `json:"action"`
	Applicable bool   `json:"applicable"`

	node string // Proxmox node that holds a container finding
	vmid int
}

type sweepResource struct {
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Tags   string `json:"tags"`
	Node   string `json:"node"`
	Type   string `json:"type"`
	Uptime int64  `json:"uptime"`
}

type sweepInputs struct {
	now        time.Time
	idle       time.Duration
	local      []monitor.Record
	dashboard  *monitor.Snapshot
	containers []sweepResource
}

// Tags that put a container outside the sweep entirely.
var sweepProtectedTags = []string{"life-prod", "hrx-infra", "hrx-keep"}

func sweepTags(raw string) map[string]bool {
	tags := map[string]bool{}
	for _, tag := range strings.FieldsFunc(raw, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }) {
		tags[tag] = true
	}
	return tags
}

func hrxOwned(tags map[string]bool) bool { return tags["hrx-managed"] || tags["owner-hrx"] }

func sweepAge(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

// planSweep is pure: it never touches a host, a store or the network.
func planSweep(in sweepInputs) []sweepFinding {
	findings := []sweepFinding{}
	live := func(r monitor.Record) bool { return r.Status != "exited" && in.now.Sub(r.LastSeen) < in.idle }

	// Containers some live agent row still points at are in use.
	inUse := map[int]bool{}
	for _, r := range in.local {
		if r.Remote != nil && r.Remote.VMID > 0 && live(r) {
			inUse[r.Remote.VMID] = true
		}
	}
	if in.dashboard != nil {
		for _, r := range in.dashboard.Agents {
			if r.Remote != nil && r.Remote.VMID > 0 && live(r.Record) {
				inUse[r.Remote.VMID] = true
			}
		}
	}
	running := map[int]bool{}
	for _, c := range in.containers {
		if c.Status == "running" {
			running[c.VMID] = true
		}
	}

	for _, r := range in.local {
		if r.Remote == nil || r.Status == "exited" || in.now.Sub(r.LastSeen) < in.idle {
			continue
		}
		if r.Remote.VMID > 0 && running[r.Remote.VMID] {
			continue // a quiet controller is not a dead agent while its container runs
		}
		stage := r.Remote.Stage
		if stage == "" {
			stage = "registered"
		}
		findings = append(findings, sweepFinding{
			ID: "row:" + r.RunID, Kind: "stale-row", Subject: r.Remote.Name + " on " + r.Host,
			Reason:     fmt.Sprintf("last controller report %s ago (stage %s)", sweepAge(in.now.Sub(r.LastSeen)), stage),
			Action:     "mark the dashboard row exited (the container is not touched)",
			Applicable: true,
		})
	}

	if in.dashboard != nil {
		for _, s := range in.dashboard.Sources {
			if s.ID == "local" || s.Status == "connected" {
				continue
			}
			reason := "unreachable and never answered since the dashboard last started"
			if !s.LastSuccess.IsZero() {
				if in.now.Sub(s.LastSuccess) < in.idle {
					continue
				}
				reason = fmt.Sprintf("unreachable; last answered %s ago", sweepAge(in.now.Sub(s.LastSuccess)))
			}
			findings = append(findings, sweepFinding{
				ID: "source:" + s.ID, Kind: "stale-source", Subject: "dashboard source " + s.ID, Reason: reason,
				Action: "remove its --source flag from the dashboard service, or restore its feed (advice only)",
			})
		}
	}

	for _, c := range in.containers {
		tags := sweepTags(c.Tags)
		if c.Type != "" && c.Type != "lxc" || !hrxOwned(tags) || inUse[c.VMID] {
			continue
		}
		protected := false
		for _, tag := range sweepProtectedTags {
			protected = protected || tags[tag]
		}
		if protected {
			continue
		}
		f := sweepFinding{
			ID: "ct:" + strconv.Itoa(c.VMID), Subject: fmt.Sprintf("CT %d %s on %s [%s]", c.VMID, c.Name, c.Node, c.Tags),
			Action: "back up to the node's storage, then destroy", Applicable: true, node: c.Node, vmid: c.VMID,
		}
		switch c.Status {
		case "stopped":
			f.Kind, f.Reason = "stopped-container", "stopped HRX-owned container with no live agent row"
		case "running":
			up := time.Duration(c.Uptime) * time.Second
			if up < in.idle {
				continue
			}
			f.Kind, f.Reason = "idle-container", fmt.Sprintf("running %s with no live agent row", sweepAge(up))
		default:
			continue
		}
		findings = append(findings, f)
	}
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].ID < findings[j].ID })
	return findings
}

type sweepHosts []string

func (h *sweepHosts) String() string { return strings.Join(*h, ",") }
func (h *sweepHosts) Set(v string) error {
	*h = append(*h, v)
	return nil
}

type sweepRuntime struct {
	transport backend.Transport
	client    *http.Client
	store     func() (monitor.Store, error)
	now       func() time.Time
	errOut    io.Writer
}

func runSweep(ctx context.Context, args []string, out io.Writer) error {
	return sweepRuntime{
		transport: backend.SSH{}, client: &http.Client{Timeout: 10 * time.Second}, store: monitor.Default,
		now: time.Now, errOut: os.Stderr,
	}.run(ctx, args, out)
}

const sweepUsage = "usage: hrx sweep [--host USER@NODE]... [--dashboard URL] [--idle 24h] [--json] [--apply ID]... [--backup-storage local]"

func (rt sweepRuntime) run(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("hrx sweep", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var hosts, apply sweepHosts
	flags.Var(&hosts, "host", "Proxmox node to list guests from, as an SSH host (repeatable)")
	flags.Var(&apply, "apply", "finding ID from the current plan to act on (repeatable)")
	dashboard := flags.String("dashboard", "", "pooled dashboard URL to read sources and agent rows from")
	idle := flags.Duration("idle", 24*time.Hour, "how long without activity counts as abandoned")
	asJSON := flags.Bool("json", false, "print the plan as JSON")
	storage := flags.String("backup-storage", "local", "Proxmox storage for backups taken before a destroy")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New(sweepUsage)
	}
	if *idle < time.Hour {
		return errors.New("--idle must be at least 1h")
	}
	if !sweepStorage(*storage) {
		return errors.New("invalid --backup-storage name")
	}
	in := sweepInputs{now: rt.now().UTC(), idle: *idle}
	store, err := rt.store()
	if err != nil {
		return err
	}
	if in.local, err = store.RemoteRecords(); err != nil {
		return fmt.Errorf("read remote dashboard rows: %w", err)
	}
	if *dashboard != "" {
		snap, err := rt.fetchDashboard(ctx, *dashboard)
		if err != nil {
			return err
		}
		in.dashboard = &snap
	}
	login := map[string]string{} // node name -> SSH host that reaches it
	seen := map[int]bool{}
	for _, host := range hosts {
		resources, err := rt.listGuests(ctx, host)
		if err != nil {
			return fmt.Errorf("list guests on %s: %w", host, err)
		}
		for _, r := range resources {
			if !seen[r.VMID] {
				seen[r.VMID] = true
				in.containers = append(in.containers, r)
			}
		}
		if err := rt.mapNodes(ctx, host, login); err != nil {
			return fmt.Errorf("read cluster nodes on %s: %w", host, err)
		}
	}
	findings := planSweep(in)

	if len(apply) == 0 {
		return printSweepPlan(out, findings, *asJSON, hosts, *dashboard)
	}
	byID := map[string]sweepFinding{}
	for _, f := range findings {
		byID[f.ID] = f
	}
	// Check every ID first, so a typo never leaves a half-applied sweep.
	for _, id := range apply {
		f, ok := byID[id]
		if !ok {
			return fmt.Errorf("%s is not in the current plan; run hrx sweep again and copy an ID from it", id)
		}
		if !f.Applicable {
			return fmt.Errorf("%s is advice only; %s", id, f.Action)
		}
	}
	type outcome struct {
		ID     string `json:"id"`
		Result string `json:"result"`
	}
	results := []outcome{}
	var failed error
	for _, id := range apply {
		f := byID[id]
		var err error
		switch {
		case strings.HasPrefix(id, "row:"):
			err = store.RetireRemote(strings.TrimPrefix(id, "row:"))
		case strings.HasPrefix(id, "ct:"):
			err = rt.destroyContainer(ctx, login[f.node], f.vmid, *storage)
		}
		result := "done"
		if err != nil {
			result, failed = "failed: "+err.Error(), errors.New("some sweep actions failed; nothing after a failed backup was destroyed")
		}
		results = append(results, outcome{id, result})
		if !*asJSON {
			fmt.Fprintf(out, "%s: %s\n", id, result)
		}
	}
	if *asJSON {
		if err := json.NewEncoder(out).Encode(map[string]any{"applied": results}); err != nil {
			return err
		}
	}
	return failed
}

func printSweepPlan(out io.Writer, findings []sweepFinding, asJSON bool, hosts []string, dashboard string) error {
	if asJSON {
		return json.NewEncoder(out).Encode(map[string]any{"plan_only": true, "findings": findings})
	}
	if len(findings) == 0 {
		_, err := fmt.Fprintln(out, "hrx sweep: nothing to clean up (plan only; nothing changed)")
		return err
	}
	fmt.Fprintf(out, "hrx sweep: %d finding(s) (plan only; nothing changed)\n\n", len(findings))
	for _, f := range findings {
		fmt.Fprintf(out, "  %s  %s\n      %s\n      -> %s\n", f.ID, f.Subject, f.Reason, f.Action)
	}
	command := "hrx sweep"
	for _, h := range hosts {
		command += " --host " + h
	}
	if dashboard != "" {
		command += " --dashboard " + dashboard
	}
	_, err := fmt.Fprintf(out, "\nTo act, rerun with the same options plus --apply ID for each item:\n  %s --apply ID\n", command)
	return err
}

func (rt sweepRuntime) fetchDashboard(ctx context.Context, raw string) (monitor.Snapshot, error) {
	var snap monitor.Snapshot
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return snap, errors.New("--dashboard must be an https URL, or http on loopback")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback())) {
		return snap, errors.New("--dashboard must be an https URL, or http on loopback")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/agents"
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return snap, err
	}
	resp, err := rt.client.Do(req)
	if err != nil {
		return snap, errors.New("dashboard did not answer")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if err != nil || resp.StatusCode != 200 || len(body) > 8<<20 || json.Unmarshal(body, &snap) != nil || snap.Version != 1 {
		return snap, errors.New("dashboard returned no valid agent snapshot")
	}
	return snap, nil
}

func (rt sweepRuntime) listGuests(ctx context.Context, host string) ([]sweepResource, error) {
	raw, err := rt.transport.Run(ctx, host, []string{"pvesh", "get", "/cluster/resources", "--type", "vm", "--output-format", "json"}, nil)
	if err != nil {
		return nil, err
	}
	var resources []sweepResource
	if json.Unmarshal(raw, &resources) != nil {
		return nil, errors.New("invalid Proxmox resource listing")
	}
	return resources, nil
}

// mapNodes records how to reach each cluster node: the --host itself for its own
// node, otherwise the node's cluster address with the same SSH user. Short node
// names often do not resolve from the operator's machine.
func (rt sweepRuntime) mapNodes(ctx context.Context, host string, login map[string]string) error {
	raw, err := rt.transport.Run(ctx, host, []string{"pvesh", "get", "/cluster/status", "--output-format", "json"}, nil)
	if err != nil {
		return err
	}
	var nodes []struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		IP    string `json:"ip"`
		Local int    `json:"local"`
	}
	if json.Unmarshal(raw, &nodes) != nil {
		return errors.New("invalid Proxmox cluster status")
	}
	user := ""
	if at := strings.LastIndex(host, "@"); at >= 0 {
		user = host[:at+1]
	}
	for _, n := range nodes {
		if n.Type != "node" || n.Name == "" {
			continue
		}
		if n.Local == 1 {
			login[n.Name] = host // the host we already reach is authoritative for its node
			continue
		}
		if _, ok := login[n.Name]; ok {
			continue
		}
		if net.ParseIP(n.IP) != nil {
			login[n.Name] = user + n.IP
		} else {
			login[n.Name] = user + n.Name
		}
	}
	return nil
}

func sweepStorage(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// destroyContainer re-reads the guest on its own node, stops it, backs it up and
// only then destroys it. A failed or unverified backup leaves it in place.
func (rt sweepRuntime) destroyContainer(ctx context.Context, host string, vmid int, storage string) error {
	if host == "" {
		return errors.New("no SSH host reaches this container's node")
	}
	id := strconv.Itoa(vmid)
	check := func() (sweepResource, error) {
		resources, err := rt.listGuests(ctx, host)
		if err != nil {
			return sweepResource{}, err
		}
		for _, r := range resources {
			if r.VMID == vmid {
				tags := sweepTags(r.Tags)
				for _, tag := range sweepProtectedTags {
					if tags[tag] {
						return r, fmt.Errorf("container is now tagged %s; left alone", tag)
					}
				}
				if !hrxOwned(tags) {
					return r, errors.New("container is no longer tagged as HRX-owned; left alone")
				}
				return r, nil
			}
		}
		return sweepResource{}, errors.New("container no longer exists")
	}
	current, err := check()
	if err != nil {
		return err
	}
	if current.Status == "running" {
		if _, err := rt.transport.Run(ctx, host, []string{"pct", "shutdown", id, "--timeout", "60", "--forceStop", "1"}, nil); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
	}
	if _, err := rt.transport.Run(ctx, host, []string{"vzdump", id, "--storage", storage, "--mode", "stop", "--compress", "zstd", "--tmpdir", "/var/tmp"}, nil); err != nil {
		return fmt.Errorf("backup failed, container kept: %w", err)
	}
	if _, err := check(); err != nil {
		return err
	}
	if _, err := rt.transport.Run(ctx, host, []string{"pct", "destroy", id, "--purge", "1"}, nil); err != nil {
		return fmt.Errorf("destroy (backup was taken): %w", err)
	}
	return nil
}
