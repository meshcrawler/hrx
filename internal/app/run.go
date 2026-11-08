package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/meshcrawler/hrx/internal/catalog"
)

const version = "0.1.2"

const Help = `hrx - harness launcher for Codex, Claude Code and OpenCode through one model gateway

  hrx --auto "claude as host/role/agent using pitput" [LAUNCH_OPTIONS] [--print]
  hrx --profile NAME [LAUNCH_OPTIONS] [-- HARNESS_ARGS...]
  hrx run --backend=proxmox --host HOST --profile NAME --agent-identity ID --pitput-board BOARD --prompt-file FILE
  hrx remote inspect|logs|attach|stop|resume|replace-lease|destroy --name NAME
  hrx network --file NETWORK.toml [--plan] [--only ROLE]
  hrx sweep [--host USER@NODE]... [--dashboard URL] [--idle 24h] [--json] [--apply ID]... [--backup-storage local]
  hrx profiles list|show NAME|validate NAME [--all] [--offline] [--json]
  hrx doctor [--harness NAME] [--model NAME] [--yesod-comms] [--json]
  hrx refresh
  hrx dashboard [--listen 127.0.0.1:8789] [--source NAME=URL] [--hub]
  hrx hub serve|enroll|status|revoke|rotate|collect
  hrx matrix --input FILE
  hrx models [--refresh] [--json] [--stale]
  hrx codex [--model NAME] [--reasoning LEVEL] [--dry-run] [-- CODEX_ARGS...]
  hrx claude [--model NAME] [--reasoning LEVEL] [--dry-run] [-- CLAUDE_ARGS...]
  hrx opencode [--model NAME] [--reasoning LEVEL] [--dry-run] [-- OPENCODE_ARGS...]
  hrx models|refresh --harness codex|claude|opencode
  hrx catalog [--compact|--details] [--latest] [--refresh] [--stale] [--json] [--search TEXT] [--harness all|codex|claude|opencode] [--output FILE]
  hrx completion zsh
  hrx prime

Launch options (all three harnesses):
  --profile NAME              Use a validated HRX launch profile
  --credential-file PATH      Override gateway key with private JSON key file
  --gitlab                    Enable GitLab service accounts (broker plugin alias)
  --plugin NAME               Enable gitlab-service-account-broker
  --agent-identity ID          Broker-authorized agent identity (required with plugin)
  --pitput                    Enable Pitput (automatic delivery for Codex)
  --pitput-board BOARD        Enroll/monitor a board; repeat for multiple boards
  --pitput-combined-preview    Opt in to combined Codex board/DM/ask delivery (preview)
  --pitput-targeted           Notify for recipient mentions (new-session default)
  --pitput-all-activity       Explicitly monitor all activity on selected boards
  --pitput-follow-thread PATH Also notify for this topic; implies targeted selection
  --pitput-no-dm              Do not deliver private Pitput direct messages
  --pitput-dm-only            Deliver only direct messages (no boards; DM-only enrollment)
  --yesod-mail | --yesod-bbs | --yesod-comms
  --yesod-agent ADDRESS       Reuse an explicitly chosen registered identity
  --yolo                      Disable approval prompts (OpenCode respects denies)
  --no-yolo                   Override permissions.yolo and keep approval prompts
  --resume SESSION_ID         Resume a specific harness session
  --continue                  Continue the latest harness session

Claude only:
  --claude-config-dir PATH    Use an HRX-managed Claude config that HRX creates (never yours)
  --claude-init PARTS         Initialize first-run answers once: theme,trust,bypass

HRX settings: ~/.config/hrx/config.toml (or HRX_CONFIG / XDG_CONFIG_HOME).
Codex provider definitions: $CODEX_HOME/config.toml (default ~/.codex/config.toml).
Tab completion and models use a local cache; launches refresh and fail closed.
Put hrx options before harness arguments; use -- before Codex subcommands.
Use --latest on models/catalog, or before --model for latest-only Tab suggestions.
Explicit older models remain launchable; omit --latest to show everything.
`

func Run(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) == 1 && args[0] == "_codex-observe" {
		return runRemoteCodexObserve(ctx, os.Stdin, out)
	}
	if len(args) > 0 && args[0] == "network" {
		return runRemoteNetwork(ctx, args[1:], out)
	}
	if len(args) > 0 && args[0] == "sweep" {
		return runSweep(ctx, args[1:], out)
	}
	if len(args) > 0 && args[0] == "remote" {
		return runRemoteLifecycle(ctx, args[1:], out)
	}
	if len(args) > 0 && args[0] == "run" {
		return runLocated(ctx, args[1:], out, errOut)
	}
	if len(args) == 1 && args[0] == "_remote-observation" {
		return runRemoteObservation(os.Stdin)
	}
	if len(args) > 0 && args[0] == "_pitput-admit" {
		return runPitputAdmit(ctx, args[1:], out)
	}
	if len(args) > 0 && args[0] == "profiles" {
		return runProfiles(ctx, args[1:], out)
	}
	var profileErr error
	args, profileErr = expandProfileCommand(args)
	if profileErr != nil {
		return profileErr
	}
	if len(args) > 0 && args[0] == "hub" {
		return runHub(ctx, args[1:], out)
	}
	if len(args) > 0 && (args[0] == "--auto" || strings.HasPrefix(args[0], "--auto=")) {
		var group int32
		interactive := terminalGroup(os.Stdin.Fd(), syscall.TIOCGPGRP, &group) == nil
		return runAuto(ctx, args, os.Stdin, out, errOut, interactive, func(argv []string) error {
			return Run(ctx, argv, out, errOut)
		})
	}
	if len(args) > 0 && args[0] == "dashboard" {
		return runDashboard(ctx, args[1:], out)
	}
	if len(args) == 2 && args[0] == "_activity" {
		recordActivity(args[1], "")
		return nil
	}
	if len(args) > 0 && args[0] == "_git-credential" {
		return gitCredential(args[1:], os.Stdin, out)
	}
	if len(args) > 0 && args[0] == "_glab" {
		return runGlabShim(args[1:])
	}
	if len(args) == 1 && args[0] == "_matrix-child" {
		if err := runMatrixChild(ctx); err != nil {
			return errors.New("offline fixture failed")
		}
		return nil
	}
	if len(args) > 0 && args[0] == "matrix" {
		return runMatrix(ctx, args[1:], out)
	}
	if len(args) == 1 && args[0] == "_session-start" {
		return runSessionStartHook(os.Stdin)
	}
	if len(args) == 1 && args[0] == "_claude-pitput-recover" {
		return runClaudeHandoffRecovery(os.Stdin, out)
	}
	if len(args) > 0 && args[0] == "doctor" {
		return runDoctor(ctx, args[1:], out)
	}
	if len(args) > 0 && args[0] == "catalog" {
		return runCatalog(ctx, args[1:], out)
	}
	latest := false
	if len(args) > 0 && (args[0] == "models" || args[0] == "_complete") {
		clean := make([]string, 0, len(args))
		for _, arg := range args {
			if arg == "--latest" {
				latest = true
			} else {
				clean = append(clean, arg)
			}
		}
		args = clean
	}
	if handled, err := runOtherHarness(ctx, args, out, errOut, latest); handled {
		return err
	}
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		_, err := fmt.Fprint(out, Help)
		return err
	}
	if args[0] == "--version" {
		fmt.Fprintln(out, Version())
		return nil
	}
	if args[0] == "prime" {
		if len(args) != 1 {
			return errors.New("usage: hrx prime")
		}
		_, err := fmt.Fprint(out, PrimeGuide)
		return err
	}
	if args[0] == "completion" {
		if len(args) != 2 || args[1] != "zsh" {
			return errors.New("usage: hrx completion zsh")
		}
		_, err := fmt.Fprint(out, ZshCompletion)
		return err
	}
	if args[0] == "codex" && len(args) == 2 && args[1] == "--help" {
		fmt.Fprint(out, Help)
		return nil
	}
	if args[0] != "refresh" && args[0] != "models" && args[0] != "codex" && args[0] != "_complete" {
		return errors.New("unknown command; run hrx --help")
	}
	var s Settings
	var opts LaunchOptions
	var err error
	if args[0] == "codex" {
		opts, err = prepareLaunchOptions("codex", args[1:])
		if err != nil {
			return err
		}
		s, err = LoadSettings(opts.CredentialFile)
	} else {
		s, err = LoadSettings()
	}
	if err != nil {
		if args[0] == "_complete" {
			return nil
		}
		return err
	}
	if args[0] == "_complete" {
		// Never refresh or execute an auth helper from this path.
		snapshot, err := catalog.Read(s.CachePath)
		if err != nil || snapshot.Validate(s.Binding, time.Now(), false) != nil {
			return nil
		}
		if latest {
			snapshot = snapshot.Latest("codex")
		}
		if len(args) == 2 && args[1] == "models" {
			for _, name := range snapshot.Names("codex") {
				fmt.Fprintln(out, name)
			}
		}
		if len(args) == 2 && args[1] == "model-descriptions" {
			completeModelDescriptions(out, snapshot, "codex")
		}
		if len(args) == 3 && (args[1] == "reasoning" || args[1] == "reasoning-descriptions") {
			name := args[2]
			if name == "" {
				name = s.DefaultModel
			}
			if args[1] == "reasoning-descriptions" {
				completeReasoningDescriptions(out, snapshot, "codex", name)
				return nil
			}
			m, err := snapshot.Resolve(name, "codex")
			if err == nil {
				for _, level := range m.Harnesses["codex"].Reasoning {
					fmt.Fprintln(out, level)
				}
			}
		}
		return nil
	}
	if args[0] == "codex" {
		snapshot, key, err := refreshWithCredential(ctx, s)
		if err != nil {
			return fmt.Errorf("launch validation failed: %w", err)
		}
		if err := validateProfileRoute(s, snapshot, opts); err != nil {
			return err
		}
		launch, err := BuildLaunch(s, snapshot, opts)
		if err != nil {
			return err
		}
		generatedPath, skipped, err := GenerateCodexCatalog(s, snapshot)
		if err != nil {
			return err
		}
		s.GeneratedCatalogPath = generatedPath
		for _, route := range skipped {
			fmt.Fprintf(errOut, "hrx: codex catalog skipped %s: gateway context limit unknown; refresh the gateway catalog\n", route)
		}
		launch, err = BuildLaunch(s, snapshot, opts)
		if err != nil {
			return err
		}
		attachProfile(opts, &launch)
		if opts.CredentialFile != "" {
			launch.Env = childEnvironment(os.Environ(), map[string]string{"HRX_PROFILE_GATEWAY_KEY": key}, nil)
		}
		if launch.Profile != nil {
			fmt.Fprintf(errOut, "hrx: profile=%s route=%s\n", launch.Profile.Name, launch.Model)
		}
		if err := prepareYesod(ctx, s, opts, &launch, errOut); err != nil {
			return err
		}
		if err := prepareSessionCapture(ctx, opts, &launch, errOut); err != nil {
			return err
		}
		if opts.DryRun {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(launch)
		}
		fmt.Fprintf(errOut, "hrx: %s -> %s; reasoning=%s; provider=%s\n", launch.Identity, launch.Model, launch.Reasoning, launch.Provider)
		return execHarness(launch)
	}
	refresh := args[0] == "refresh"
	asJSON, stale := false, false
	for _, a := range args[1:] {
		if args[0] == "refresh" {
			return errors.New("usage: hrx refresh")
		}
		switch a {
		case "--refresh":
			refresh = true
		case "--json":
			asJSON = true
		case "--stale":
			stale = true
		default:
			return fmt.Errorf("unknown models option %q", a)
		}
	}
	var snapshot catalog.Snapshot
	if refresh {
		snapshot, err = Refresh(ctx, s)
	} else {
		snapshot, err = catalog.Read(s.CachePath)
	}
	if err != nil {
		return err
	}
	if err = snapshot.Validate(s.Binding, time.Now(), stale); err != nil {
		return err
	}
	if latest {
		snapshot = snapshot.Latest("codex")
	}
	if stale {
		fmt.Fprintln(errOut, "hrx: --stale permits cached authorization; launch always revalidates")
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(snapshot)
	}
	fmt.Fprintf(out, "Authorization checked %s; gateway catalog %s (not a live inference test)\n", snapshot.RefreshedAt.Format(time.RFC3339), snapshot.GeneratedAt.Format(time.RFC3339))
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "MODEL\tROUTE\tREASONING")
	for _, name := range snapshot.Names("codex") {
		m, _ := snapshot.Resolve(name, "codex")
		fmt.Fprintf(w, "%s\t%s\t%s\n", name, m.Route, strings.Join(m.Harnesses["codex"].Reasoning, ", "))
	}
	if len(snapshot.Models) == 0 {
		fmt.Fprintln(errOut, "hrx: no authorized routes with declared Codex capabilities")
	}
	return w.Flush()
}

func execHarness(launch Launch) error {
	env := launch.Env
	if env == nil {
		env = os.Environ()
	}
	launch.Env = childEnvironment(env, nil, []string{resumeReceiptEnv})
	showResume := interactiveResumeHint(launch)
	var receipt string
	var cleanup func()
	if showResume {
		receipt, cleanup = prepareResumeReceipt(&launch)
	}
	trackLaunch(&launch)
	defer finishLaunch(launch)
	var stopHub func()
	if launch.hubClient != nil {
		var err error
		stopHub, err = startHubOwnership(&launch, os.Stderr)
		if err != nil {
			if cleanup != nil {
				cleanup()
			}
			if launch.lease != nil {
				_ = launch.lease.release()
			}
			return err
		}
	}
	if launch.Executable == "opencode" && launch.pitputRuntime == nil {
		dir, err := os.MkdirTemp("", "hrx-opencode-guard-")
		if err == nil {
			defer os.RemoveAll(dir)
			dir, err = filepath.EvalSymlinks(dir)
			if err == nil {
				err = installOpenCodeActionGuard(&launch, dir)
			}
		}
		if err != nil {
			if stopHub != nil {
				stopHub()
			}
			if cleanup != nil {
				cleanup()
			}
			if launch.lease != nil {
				_ = launch.lease.release()
			}
			return err
		}
	}
	var stopPitput func()
	if launch.pitputRuntime != nil {
		var err error
		stopPitput, err = startPitputRuntime(&launch, receipt, os.Stderr)
		if err != nil {
			if stopHub != nil {
				stopHub()
			}
			if cleanup != nil {
				cleanup()
			}
			if launch.lease != nil {
				_ = launch.lease.release()
			}
			return err
		}
	}
	labelWavePane(launch)
	if launch.lease != nil || showResume || stopPitput != nil || launch.monitorRun != "" {
		var lease presenceLease
		if launch.lease != nil {
			lease = launch.lease
		}
		status, err := superviseHarness(launch, lease)
		if stopPitput != nil {
			stopPitput()
		}
		finishLaunch(launch)
		if stopHub != nil {
			stopHub()
		}
		if showResume && err == nil {
			printResumeHint(os.Stderr, launch, receipt)
		}
		if cleanup != nil {
			cleanup()
		}
		if err != nil {
			return err
		}
		if status.Signaled() {
			sig := status.Signal()
			signal.Reset(sig)
			_ = syscall.Kill(os.Getpid(), sig)
			// Normally unreachable; retain a conventional exit code if the
			// invoking shell had this signal ignored before hrx started.
			time.Sleep(time.Second)
			os.Exit(128 + int(sig))
		}
		os.Exit(status.ExitStatus())
	}
	path, err := exec.LookPath(launch.Executable)
	if err != nil {
		return errors.New("harness executable not found on PATH")
	}
	env = launch.Env
	if env == nil {
		env = os.Environ()
	}
	return launch.startWithSession(func() error {
		return syscall.Exec(path, append([]string{path}, launch.Args...), env)
	})
}
