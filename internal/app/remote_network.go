package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/pelletier/go-toml/v2"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

type remoteNetwork struct {
	Name   string               `toml:"name"`
	Tags   []string             `toml:"tags"`
	Agents []remoteNetworkAgent `toml:"agents"`
}
type remoteNetworkAgent struct {
	FollowThreads []string `toml:"follow_threads"`
	Role          string   `toml:"role"`
	Host          string   `toml:"host"`
	Profile       string   `toml:"profile"`
	Identity      string   `toml:"identity"`
	Boards        []string `toml:"boards"`
	PromptFile    string   `toml:"prompt_file"`
	Tags          []string `toml:"tags"`
	Required      *bool    `toml:"required"`
	Cores         int      `toml:"cores"`
	MemoryMiB     int      `toml:"memory_mib"`
	DiskGiB       int      `toml:"disk_gib"`
}

func (a remoteNetworkAgent) required() bool { return a.Required == nil || *a.Required }
func (n remoteNetwork) arguments(a remoteNetworkAgent, dir string) []string {
	prompt := a.PromptFile
	if !filepath.IsAbs(prompt) {
		prompt = filepath.Join(dir, prompt)
	}
	args := []string{"--host", a.Host, "--profile", a.Profile, "--name", n.Name + "/" + a.Role, "--agent-identity", a.Identity, "--prompt-file", prompt}
	for _, thread := range a.FollowThreads {
		args = append(args, "--pitput-follow-thread", thread)
	}
	for _, board := range a.Boards {
		args = append(args, "--pitput-board", board)
	}
	for _, tag := range append(append([]string{}, n.Tags...), a.Tags...) {
		args = append(args, "--tag", tag)
	}
	for _, resource := range []struct {
		name  string
		value int
	}{{"cores", a.Cores}, {"memory-mib", a.MemoryMiB}, {"disk-gib", a.DiskGiB}} {
		if resource.value != 0 {
			args = append(args, "--"+resource.name, strconv.Itoa(resource.value))
		}
	}
	return args
}

type remotePrepare func(context.Context, []string, io.Writer, remoteRunMode) error

func runRemoteNetwork(ctx context.Context, args []string, out io.Writer) error {
	return runRemoteNetworkWith(ctx, args, out, runProxmoxMode)
}
func runRemoteNetworkWith(ctx context.Context, args []string, out io.Writer, prepare remotePrepare) error {
	flags := flag.NewFlagSet("hrx network", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	file := flags.String("file", "", "network TOML")
	plan := flags.Bool("plan", false, "validate all roles without effects")
	var only repeatedFlag
	flags.Var(&only, "only", "role to start/retry; repeat for multiple roles")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *file == "" || flags.NArg() != 0 {
		return errors.New("usage: hrx network --file NETWORK.toml [--plan] [--only ROLE]")
	}
	path, err := filepath.Abs(*file)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 256*1024 {
		return errors.New("network file unavailable or oversized")
	}
	var network remoteNetwork
	decoder := toml.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&network) != nil || !boardFlagPattern.MatchString(network.Name) || len(network.Name) > 100 || len(network.Agents) < 1 || len(network.Agents) > 16 {
		return errors.New("invalid network name, roles, or schema (1–16 agents required)")
	}
	roles, identities := map[string]bool{}, map[string]bool{}
	for _, a := range network.Agents {
		if !boardFlagPattern.MatchString(a.Role) || len(a.Role) > 100 || roles[a.Role] || identities[a.Identity] || !pitputAgentName.MatchString(a.Identity) || a.Host == "" || a.Profile == "" || a.PromptFile == "" || len(a.Boards) == 0 {
			return errors.New("network requires unique roles/identities and explicit host, profile, boards, prompt_file")
		}
		roles[a.Role] = true
		identities[a.Identity] = true
	}
	selected := map[string]bool{}
	for _, role := range only {
		if !roles[role] {
			return fmt.Errorf("unknown network role %q", role)
		}
		selected[role] = true
	}
	// Validate every role and artifact before allocating any container.
	plans := map[string]json.RawMessage{}
	for _, a := range network.Agents {
		var buffer bytes.Buffer
		argv := append(network.arguments(a, filepath.Dir(path)), "--plan")
		if err := prepare(ctx, argv, &buffer, remoteRunMode{}); err != nil {
			return fmt.Errorf("role %s preflight: %w", a.Role, err)
		}
		if !json.Valid(buffer.Bytes()) {
			return errors.New("invalid role plan")
		}
		plans[a.Role] = append([]byte(nil), buffer.Bytes()...)
	}
	if *plan {
		return json.NewEncoder(out).Encode(map[string]any{"network": network.Name, "roles": plans, "ready": false})
	}
	ready := map[string]bool{}
	var failures []error
	for _, a := range network.Agents {
		fmt.Fprintf(out, "network=%s role=%s stage=preparing\n", network.Name, a.Role)
		mode := remoteRunMode{ObserveOnly: len(selected) > 0 && !selected[a.Role], DeferWork: func(func(context.Context) error) {}}
		err := prepare(ctx, network.arguments(a, filepath.Dir(path)), out, mode)
		if err != nil {
			fmt.Fprintf(out, "role=%s stage=failed error=%s\n", a.Role, err)
			if a.required() {
				failures = append(failures, fmt.Errorf("role %s: %w", a.Role, err))
			}
			continue
		}
		ready[a.Role] = true
	}
	if len(failures) > 0 {
		return fmt.Errorf("network barrier not met; no assigned work released: %w", errors.Join(failures...))
	}
	// Revalidate required participants after preparation without bootstrapping or
	// restarting healthy agents. Capture closures only from these current checks.
	releases := map[string]func(context.Context) error{}
	for _, a := range network.Agents {
		if !ready[a.Role] {
			continue
		}
		var release func(context.Context) error
		err := prepare(ctx, network.arguments(a, filepath.Dir(path)), out, remoteRunMode{ObserveOnly: true, DeferWork: func(f func(context.Context) error) { release = f }})
		if err != nil || release == nil {
			if err == nil {
				err = errors.New("prepared role did not supply a release operation")
			}
			fmt.Fprintf(out, "role=%s stage=recheck-failed error=%s\n", a.Role, err)
			if a.required() {
				failures = append(failures, fmt.Errorf("role %s recheck: %w", a.Role, err))
			}
			continue
		}
		releases[a.Role] = release
	}
	if len(failures) > 0 {
		return fmt.Errorf("network readiness changed; no assigned work released: %w", errors.Join(failures...))
	}
	fmt.Fprintf(out, "network=%s stage=barrier-ready\n", network.Name)
	for _, a := range network.Agents {
		if release := releases[a.Role]; release != nil {
			if err := release(ctx); err != nil {
				failures = append(failures, fmt.Errorf("role %s release: %w", a.Role, err))
			}
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("network release incomplete; retry uses stable work IDs: %w", errors.Join(failures...))
	}
	return nil
}
