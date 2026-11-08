package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/meshcrawler/hrx/internal/session"
)

func launchProject(harness string, args []string) (string, error) {
	project, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if harness == "codex" {
		for _, token := range nativeArguments(harness, args) {
			if !token.option {
				continue
			}
			i := token.index
			name, value, equal := strings.Cut(args[i], "=")
			if name == "-C" || name == "--cd" {
				if !equal {
					i++
					if i >= len(args) {
						return "", errors.New("missing project directory")
					}
					value = args[i]
				}
				project = value
			} else if strings.HasPrefix(args[i], "-C") {
				project = strings.TrimPrefix(args[i], "-C")
			}
		}
	}
	if harness == "opencode" && nativeCommand(harness, args) != "run" {
		for _, token := range nativeArguments(harness, args) {
			if !token.option {
				project = args[token.index]
				break
			}
		}
	}
	absolute, err := filepath.Abs(project)
	if err != nil {
		return "", errors.New("cannot resolve launch project")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", errors.New("launch project does not exist")
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", errors.New("launch project is not a directory")
	}
	return canonical, nil
}

func resolveLaunchSession(harness string, o LaunchOptions) (LaunchOptions, error) {
	var err error
	o, err = normalizeNativeSession(harness, o)
	if err != nil {
		return o, err
	}
	project, err := launchProject(harness, o.Extra)
	if err != nil {
		return o, err
	}
	o.project = project
	if o.Resume == "" && !o.Continue {
		return o, nil
	}
	store, err := session.Default()
	if err != nil {
		return o, err
	}
	o, _, err = restoreSession(store, harness, project, o)
	return o, err
}

func normalizeNativeSession(harness string, o LaunchOptions) (LaunchOptions, error) {
	if o.Resume != "" || o.Continue {
		return o, nil
	}
	args := append([]string(nil), o.Extra...)
	if harness == "codex" {
		position := codexResumePosition(args)
		if position < 0 {
			return o, nil
		}
		if nativeCommand("codex", args) == "resume" {
			o.resumeInsertion = position
		}
		tail := args[position+1:]
		last, id := -1, -1
		for _, token := range nativeArguments("codex", tail) {
			if token.option && tail[token.index] == "--last" {
				if last >= 0 {
					return o, errors.New("duplicate native --last option")
				}
				last = token.index
			}
			if !token.option && id < 0 {
				id = token.index
			}
		}
		if last >= 0 {
			o.Continue = true
			tail = append(tail[:last], tail[last+1:]...)
		} else if id >= 0 {
			if !nativeSessionID.MatchString(tail[id]) {
				return o, errors.New("invalid native session ID")
			}
			o.Resume = tail[id]
			tail = append(tail[:id], tail[id+1:]...)
		} else {
			return o, errors.New("tracked Codex resume requires a session ID or --last; use hrx --resume or --continue")
		}
		o.Extra = append(args[:position], tail...)
		return o, nil
	}
	removed := map[int]bool{}
	for _, token := range nativeArguments(harness, args) {
		if !token.option {
			continue
		}
		i := token.index
		name, value, equal := strings.Cut(args[i], "=")
		isContinue := name == "--continue" || name == "-c"
		isResume := harness == "claude" && (name == "--resume" || name == "-r") || harness == "opencode" && (name == "--session" || name == "-s")
		if !isContinue && !isResume {
			continue
		}
		if o.Resume != "" || o.Continue {
			return o, errors.New("duplicate native resume/continue options")
		}
		if isContinue {
			if equal {
				return o, errors.New("continue does not take a value")
			}
			o.Continue = true
			removed[i] = true
			continue
		}
		removed[i] = true
		if !equal {
			i++
			if i >= len(args) {
				return o, errors.New("native resume requires an exact session ID")
			}
			value = args[i]
			removed[i] = true
		}
		if !nativeSessionID.MatchString(value) {
			return o, errors.New("invalid native session ID")
		}
		o.Resume = value
	}
	clean := []string{}
	for i, arg := range args {
		if !removed[i] {
			clean = append(clean, arg)
		}
	}
	o.Extra = clean
	return o, nil
}

func sessionHookCommand(executable string) string {
	return "'" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "' _session-start"
}

func attachSessionHook(launch *Launch, command string, record session.Record) error {
	metadata, err := json.Marshal(record)
	if err != nil {
		return errors.New("cannot encode session launch metadata")
	}
	env := launch.Env
	if env == nil {
		env = os.Environ()
	}
	launch.Env = childEnvironment(env, map[string]string{sessionMetadataEnv: string(metadata)}, nil)
	switch launch.Executable {
	case "codex":
		hook := fmt.Sprintf("hooks.SessionStart=[{matcher=\"^(startup|resume|clear)$\",hooks=[{type=\"command\",command=%q,timeout=10}]}]", command)
		launch.Args = append([]string{"-c", hook}, launch.Args...)
		if !slices.Contains(launch.Args, "--no-daemon") {
			launch.Args = append([]string{"--no-daemon"}, launch.Args...)
		}
	case "claude":
		config := map[string]any{"hooks": map[string]any{"SessionStart": []any{map[string]any{"matcher": "startup|resume|clear", "hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 10}}}}}}
		for hook, event := range map[string]string{"UserPromptSubmit": "user-message", "Stop": "assistant-turn", "PostToolUse": "tool-use"} {
			if !strings.HasSuffix(command, "_session-start") {
				continue
			}
			activityCommand := strings.TrimSuffix(command, "_session-start") + "_activity " + event
			config["hooks"].(map[string]any)[hook] = []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": activityCommand, "timeout": 3}}}}
		}
		if launch.Pitput && strings.HasSuffix(command, "_session-start") {
			hooks := config["hooks"].(map[string]any)
			recover := strings.TrimSuffix(command, "_session-start") + "_claude-pitput-recover"
			hooks["UserPromptSubmit"] = append(hooks["UserPromptSubmit"].([]any),
				map[string]any{"hooks": []any{map[string]any{"type": "command", "command": recover, "timeout": 10}}})
		}
		data, _ := json.Marshal(config)
		launch.Args = append([]string{"--settings", string(data)}, launch.Args...)
		if launch.Yesod != nil || launch.Pitput || launch.Broker != nil {
			launch.Args = append([]string{"--system-prompt-snapshot", "off"}, launch.Args...)
		}
	default:
		return errors.New("unsupported session hook harness")
	}
	return nil
}

func prepareSessionCapture(ctx context.Context, o LaunchOptions, launch *Launch, out io.Writer) (err error) {
	if o.DryRun {
		return nil
	}
	defer func() {
		if err != nil && launch.lease != nil {
			_ = launch.lease.release()
			launch.lease = nil
		}
	}()
	record := session.Record{NativeID: o.Resume, Harness: launch.Executable, Identity: launch.Identity, Route: launch.Model, Reasoning: launch.Reasoning, Project: o.project}
	record.Profile = launch.Profile
	launch.resumeProject = o.project
	record.Pitput, record.Plugin, record.AgentIdentity = o.Pitput, o.Plugin, o.AgentIdentity
	record.PitputBoards, record.PitputDM = o.PitputBoards, o.PitputDM
	record.PitputCombinedPreview = o.PitputCombinedPreview
	record.PitputTargeted = o.PitputTargeted
	record.PitputFollowThreads = append([]string(nil), o.PitputFollowThreads...)
	record.ClaudeConfigDir = o.ClaudeConfigDir
	if launch.pitputRuntime != nil {
		record.AgentIdentity = launch.pitputRuntime.identity
		record.PitputBoards = launch.pitputRuntime.boards
		if launch.Executable == "claude" {
			record.NativeID, err = bindClaudeTaskSession(launch, o.Resume)
			if err != nil {
				return err
			}
		}
	}
	if launch.Yesod != nil {
		record.Mail, record.BBS, record.Agent = launch.Yesod.Mail, launch.Yesod.BBS, launch.Yesod.Agent
	}
	if launch.pitputRuntime != nil {
		launch.pitputRuntime.record = record
		if launch.Executable == "codex" {
			// The coverage owner indexes the verified native identity after Ready.
			// A second SessionStart hook is redundant and can block unattended
			// terminal startup on native hook trust before admission begins.
			return nil
		}
	}
	if launch.Executable == "opencode" {
		id, err := openCodeNativeSession(ctx, *launch, o.project, o.Resume)
		if err != nil {
			return err
		}
		record.NativeID = id
		if launch.pitputRuntime != nil {
			launch.pitputRuntime.record = record
		}
		launch.pendingSession = &record
		if o.Resume == "" {
			launch.Args = insertOpenCodeFlags(launch.Args, "--session", id)
		}
		if !nativeHasOption("opencode", launch.Args, "--standalone") {
			launch.Args = insertOpenCodeFlags(launch.Args, "--standalone")
		}
		fmt.Fprintf(out, "hrx: session=%s; private metadata will be indexed at process start\n", id)
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return errors.New("cannot locate hrx session hook executable")
	}
	if err := attachSessionHook(launch, sessionHookCommand(executable), record); err != nil {
		return err
	}
	if launch.Executable == "codex" && launch.pitputRuntime == nil {
		fmt.Fprintln(out, "hrx: session capture requires trusting this hrx SessionStart hook in /hooks; untrusted hooks are skipped by Codex")
	}
	return nil
}

func (launch Launch) startWithSession(start func() error) error {
	if launch.pendingSession == nil {
		return start()
	}
	store, err := session.Default()
	if err != nil {
		return errors.New("cannot locate private hrx session index")
	}
	return store.SaveAndStart(*launch.pendingSession, start)
}
