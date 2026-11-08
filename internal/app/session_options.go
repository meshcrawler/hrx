package app

import (
	"errors"
	"slices"
	"strings"
)

func nativeResume(harness string, args []string) bool {
	if harness == "codex" {
		return codexResumePosition(args) >= 0
	}
	for _, token := range nativeArguments(harness, args) {
		if !token.option {
			continue
		}
		arg := args[token.index]
		name, _, _ := strings.Cut(arg, "=")
		if name == "--continue" || name == "-c" || harness == "claude" && (name == "--resume" || name == "-r") || harness == "opencode" && (name == "--session" || name == "-s") {
			return true
		}
	}
	return false
}

func validateSessionOptions(harness string, o LaunchOptions) error {
	if o.YesodMail || o.YesodBBS || o.Pitput || o.Plugin != "" {
		for _, token := range nativeArguments(harness, o.Extra) {
			if !token.option {
				continue
			}
			arg := o.Extra[token.index]
			name, _, _ := strings.Cut(arg, "=")
			if harness == "claude" && name == "--system-prompt-snapshot" {
				return errors.New("hrx owns the Claude system-prompt snapshot setting for Yesod sessions to prevent restoring an obsolete identity")
			}
			if slices.Contains([]string{"--bg", "--background", "--tmux", "--remote-control", "--remote-control-session-name-prefix", "--environment", "--teleport", "--fork-session", "--fork", "--from-pr"}, name) {
				return errors.New("Yesod communications require a foreground, non-forked session owned by hrx; detached or alternate-session options are unsupported")
			}
		}
		if len(o.Extra) > 0 {
			command := nativeCommand(harness, o.Extra)
			if (harness == "codex" && command == "fork") ||
				(harness == "claude" && slices.Contains([]string{"attach", "agents", "respawn", "logs", "stop", "kill", "rm", "gateway"}, command)) {
				return errors.New("Yesod communications cannot attach to, fork, or manage an independently owned harness process")
			}
		}
	}
	if harness == "opencode" && (o.YesodMail || o.YesodBBS || o.Pitput || o.Plugin != "") {
		for _, token := range nativeArguments(harness, o.Extra) {
			if !token.option {
				continue
			}
			arg := o.Extra[token.index]
			name, _, _ := strings.Cut(arg, "=")
			if name == "--server" {
				return errors.New("Yesod requires a standalone OpenCode process; --server would lose the launch identity")
			}
		}
	}
	if o.Resume != "" && o.Continue {
		return errors.New("choose --resume SESSION_ID or --continue, not both")
	}
	if (o.Resume != "" || o.Continue) && nativeResume(harness, o.Extra) {
		return errors.New("do not combine hrx resume options with native resume arguments")
	}
	if o.YesodAgent != "" && !o.YesodMail && !o.YesodBBS {
		return errors.New("--yesod-agent requires --yesod-mail, --yesod-bbs, or --yesod-comms")
	}
	if o.YesodAgent != "" && !yesodAddress.MatchString(o.YesodAgent) {
		return errors.New("invalid Yesod address; expected host/role/qualifier")
	}
	if o.Yolo {
		for _, token := range nativeArguments(harness, o.Extra) {
			if !token.option {
				continue
			}
			a := o.Extra[token.index]
			name, _, _ := strings.Cut(a, "=")
			if (harness == "codex" && name == "-s") || slices.Contains([]string{"--sandbox", "--ask-for-approval", "-a", "--permission-mode", "--permission-profile", "--approve-for-me"}, name) {
				return errors.New("--yolo conflicts with explicit sandbox/approval options")
			}
		}
	}
	return nil
}

func sessionArguments(harness string, o LaunchOptions) ([]string, error) {
	if err := validateSessionOptions(harness, o); err != nil {
		return nil, err
	}
	if (o.Resume != "" || o.Continue) && (o.YesodMail || o.YesodBBS) && o.YesodAgent == "" {
		return nil, errors.New("resuming with Yesod requires saved metadata or an explicit --yesod-agent")
	}
	extra := append([]string{}, o.Extra...)
	var flags []string
	if o.Yolo {
		flags = append(flags, map[string]string{"codex": "--yolo", "claude": "--dangerously-skip-permissions", "opencode": "--auto"}[harness])
	}
	if o.Resume != "" || o.Continue {
		switch harness {
		case "codex":
			resume := []string{"resume"}
			if o.Continue {
				resume = append(resume, "--last")
			} else {
				resume = append(resume, o.Resume)
			}
			insert := o.resumeInsertion
			for _, token := range nativeArguments("codex", extra) {
				if token.option || token.literal {
					continue
				}
				if extra[token.index] == "exec" {
					insert = token.index + 1
				}
				break
			}
			if insert < 0 || insert > len(extra) {
				return nil, errors.New("invalid native resume position")
			}
			prefix := append([]string{}, extra[:insert]...)
			extra = append(append(prefix, resume...), extra[insert:]...)
		case "claude":
			if o.Continue {
				flags = append(flags, "--continue")
			} else {
				flags = append(flags, "--resume", o.Resume)
			}
		case "opencode":
			if o.Continue {
				flags = append(flags, "--continue")
			} else {
				flags = append(flags, "--session", o.Resume)
			}
		}
	}
	// OpenCode's run flags belong to the subcommand, not the root parser.
	if harness == "opencode" {
		return insertOpenCodeFlags(extra, flags...), nil
	}
	return append(flags, extra...), nil
}
