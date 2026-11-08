package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"github.com/meshcrawler/hrx/internal/backend"
	"github.com/meshcrawler/hrx/internal/monitor"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// Lifecycle operations use the immutable saved specification, even when current
// host defaults change. The backend rechecks ownership before every operation.
func runRemoteLifecycle(ctx context.Context, args []string, out io.Writer) (resultErr error) {
	if len(args) == 0 {
		return errors.New("usage: hrx remote inspect|logs|attach|stop|resume|replace-lease|destroy --name NAME")
	}
	operation := args[0]
	switch operation {
	case "inspect", "logs", "attach", "stop", "resume", "replace-lease", "destroy":
	default:
		return errors.New("supported remote operations: inspect, logs, attach, stop, resume, replace-lease, destroy")
	}
	flags := flag.NewFlagSet("remote "+operation, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	generation := flags.Int("generation", 0, "explicit replacement lease generation")
	lines := flags.Int("lines", 200, "native pane history lines (logs only)")
	name := flags.String("name", "", "saved deployment name")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *name == "" || flags.NArg() != 0 {
		return errors.New("remote operation requires --name NAME and no positional arguments")
	}
	config, dir, err := loadUserConfig()
	if err != nil {
		return err
	}
	stateDirectory, err := profilePath(config.Proxmox.StateDirectory, dir)
	if err != nil {
		return errors.New("configure a private deployment state_directory")
	}
	store := backend.StateStore{Directory: stateDirectory}
	res, err := store.Lookup(*name)
	if err != nil {
		return err
	}
	p := backend.Proxmox{Host: res.Host, Transport: backend.SSH{}}
	observations, observationErr := monitor.Default()
	var remoteRecord *monitor.Record
	if observationErr == nil {
		remoteRecord, observationErr = observations.RemoteRecord(res.DeploymentID)
	}
	stage := map[string]string{"stop": "stopping", "resume": "resuming", "replace-lease": "replacing-lease", "destroy": "destroying"}[operation]
	report := func(value string, failed bool) {
		if observationErr == nil && remoteRecord != nil {
			if err := observations.UpdateRemote(res.DeploymentID, value, failed, 0, ""); err != nil {
				fmt.Fprintln(os.Stderr, "hrx: could not persist controller progress; inspect remote state directly")
			}
		}
	}
	if stage != "" {
		report(stage, false)
		defer func() {
			if resultErr != nil {
				report(stage, true)
			}
		}()
	}

	if operation == "replace-lease" {
		if *generation < 1 {
			return errors.New("replace-lease requires --generation N (first replacement is 1; retry with the same number)")
		}
		auth := config.Proxmox.BrokerAuthCommand
		if len(auth) == 0 || config.Proxmox.BrokerURL == "" {
			return errors.New("credential replacement requires controller broker configuration")
		}
		// The stop operation refuses destroyed/absent deployments before minting.
		if _, err = p.Reconcile(ctx, "stop", res.DeploymentID, *res.Container); err != nil {
			return err
		}
		broker := backend.LeaseBroker{URL: config.Proxmox.BrokerURL, Authenticate: func(call context.Context) (string, error) {
			data, e := exec.CommandContext(call, auth[0], auth[1:]...).Output()
			if e != nil {
				return "", errors.New("controller broker authentication failed")
			}
			return strings.TrimSpace(string(data)), nil
		}}
		lease, replaceErr := store.ReplaceLease(ctx, res, *generation, broker)
		if replaceErr != nil {
			return replaceErr
		}
		if _, err = p.Reconcile(ctx, "start", res.DeploymentID, *res.Container); err != nil {
			return err
		}
		if err = p.InstallLease(ctx, res.DeploymentID, *res.Container, lease); err != nil {
			return err
		}
		report("credential-installed", false)
		return json.NewEncoder(out).Encode(map[string]any{"deployment": res.DeploymentID, "generation": *generation, "credential_installed": true, "ready": false, "expires_at": lease.ExpiresAt})
	}
	if operation == "attach" {
		var group int32
		if terminalGroup(os.Stdin.Fd(), syscall.TIOCGPGRP, &group) != nil {
			return errors.New("remote attach requires an interactive terminal")
		}
		return p.Attach(ctx, res.DeploymentID, *res.Container, os.Stdin, out, os.Stderr)
	}
	if operation == "logs" {
		logs, logErr := p.Logs(ctx, res.DeploymentID, *res.Container, *lines)
		if logErr != nil {
			return logErr
		}
		return json.NewEncoder(out).Encode(logs)
	}
	if operation == "resume" {
		if _, err = p.Reconcile(ctx, "start", res.DeploymentID, *res.Container); err != nil {
			return err
		}
		started, resumeErr := p.ResumeAgent(ctx, res.DeploymentID, *res.Container)
		if resumeErr != nil {
			return resumeErr
		}
		report("starting", false)
		return json.NewEncoder(out).Encode(started)
	}
	if operation == "destroy" {
		auth := config.Proxmox.BrokerAuthCommand
		if len(auth) == 0 || config.Proxmox.BrokerURL == "" {
			return errors.New("destroy requires controller broker configuration for credential revocation")
		}
		// Stop before revocation; retain the stopped container if revocation fails.
		current, observeErr := p.Reconcile(ctx, "inspect", res.DeploymentID, *res.Container)
		if observeErr != nil {
			return observeErr
		}
		if current.Phase != "destroyed" && current.Phase != "destroying" {
			if _, err = p.Reconcile(ctx, "stop", res.DeploymentID, *res.Container); err != nil {
				return err
			}
		}
		broker := backend.LeaseBroker{URL: config.Proxmox.BrokerURL, Authenticate: func(call context.Context) (string, error) {
			data, e := exec.CommandContext(call, auth[0], auth[1:]...).Output()
			if e != nil {
				return "", errors.New("controller broker authentication failed")
			}
			return strings.TrimSpace(string(data)), nil
		}}
		if err = store.RevokeLease(ctx, res, broker); err != nil {
			return err
		}
	}
	result, err := p.Reconcile(ctx, operation, res.DeploymentID, *res.Container)
	if err != nil {
		return err
	}
	if operation == "stop" {
		report("stopped", false)
	}
	if operation == "destroy" {
		report("destroyed", false)
	}
	if operation == "inspect" && remoteRecord != nil {
		return json.NewEncoder(out).Encode(struct {
			backend.Deployment
			ControllerObservation *monitor.Record `json:"controller_observation"`
		}{result, remoteRecord})
	}
	return json.NewEncoder(out).Encode(result)
}
