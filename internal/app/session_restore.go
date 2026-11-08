package app

import (
	"errors"
	"fmt"
	"github.com/meshcrawler/hrx/internal/session"
)

// restoreSession operates on an already-resolved canonical project directory.
// Native ID capture and project resolution belong to the harness adapters.
func restoreSession(store session.Store, harness, project string, o LaunchOptions) (LaunchOptions, *session.Record, error) {
	if o.Resume == "" && !o.Continue {
		return o, nil, nil
	}
	r, err := store.Resolve(harness, project, o.Resume)
	if err != nil {
		if errors.Is(err, session.ErrNotFound) && o.Model != "" && (!o.YesodMail && !o.YesodBBS || o.YesodAgent != "") && !o.Continue {
			// Legacy exact-ID sessions can still be reconstructed explicitly.
			// A missing latest record cannot be replaced with the harness's
			// global latest session: that could select another project.
			restorePitputSelection(&o, session.Record{})
			return o, nil, nil
		}
		return o, nil, fmt.Errorf("cannot restore session: %w", err)
	}
	o.Resume, o.Continue = r.NativeID, false
	if harness == "claude" {
		if o, err = restoreClaudeConfigDir(o, r.ClaudeConfigDir); err != nil {
			return o, nil, err
		}
	}
	if o.profile == nil && r.Profile != nil {
		copy := *r.Profile
		if copy.Harness != harness || copy.Model != r.Route || copy.CredentialFile == "" {
			return o, nil, errors.New("saved profile metadata is incomplete; choose an explicit --profile")
		}
		o.profile = &copy
		o.provenance = map[string]string{"harness": "session", "model": "session", "reasoning": "session", "credential_file": "session"}
		if o.Model != "" {
			o.provenance["model"] = "cli"
		}
		if o.Reasoning != "" {
			o.provenance["reasoning"] = "cli"
		}
		if o.CredentialFile == "" {
			o.CredentialFile = copy.CredentialFile
		} else {
			o.provenance["credential_file"] = "cli"
		}
	}
	if o.Model == "" {
		o.Model = r.Route
		o.restoredRoute = r.Route
	}
	if o.Reasoning == "" && o.ProfileName == "" {
		o.Reasoning = r.Reasoning
	}
	if !o.YesodMail && !o.YesodBBS {
		o.YesodMail, o.YesodBBS = r.Mail, r.BBS
	}
	if (o.YesodMail || o.YesodBBS) && o.YesodAgent == "" {
		o.YesodAgent = r.Agent
	}
	if (o.YesodMail || o.YesodBBS) && o.YesodAgent == "" {
		return o, nil, errors.New("saved session has no Yesod identity; specify --yesod-agent to opt into communications")
	}
	if o.Plugin == "" {
		o.Plugin = r.Plugin
	}
	if o.Plugin == r.Plugin && o.AgentIdentity == "" {
		o.AgentIdentity = r.AgentIdentity
	}
	o.Pitput = o.Pitput || r.Pitput
	o.PitputCombinedPreview = o.PitputCombinedPreview || r.PitputCombinedPreview
	restorePitputSelection(&o, r)
	if len(o.PitputBoards) == 0 {
		o.PitputBoards = append([]string(nil), r.PitputBoards...)
	}
	if o.PitputDM == "" {
		o.PitputDM = r.PitputDM
	}
	// Permissions come from this invocation's flags/config, never saved history.
	return o, &r, nil
}

func validateRestoredRoute(o LaunchOptions, route string) error {
	if o.restoredRoute != "" && route != o.restoredRoute {
		return errors.New("saved model route is unavailable; choose an explicit --model instead of silently substituting another provider")
	}
	return nil
}

// A default is for a new subscription, never an implicit migration on resume.
func restorePitputSelection(o *LaunchOptions, r session.Record) {
	if !o.pitputSelectionSet && !o.PitputTargeted {
		if r.Pitput {
			o.PitputTargeted = r.PitputTargeted
		} else {
			o.PitputTargeted = o.Pitput && o.PitputDM != "only"
		}
	}
	if !o.PitputAllActivity && len(o.PitputFollowThreads) == 0 {
		o.PitputFollowThreads = append([]string(nil), r.PitputFollowThreads...)
	}
}
