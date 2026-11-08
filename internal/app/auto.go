package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/meshcrawler/hrx/internal/catalog"
)

const autoLimit = 32768

// This is deliberately narrower than harness passthrough: inference cannot add
// executable arguments, environment variables, permission overrides or plugins.
type autoProposal struct {
	Harness   string   `json:"harness"`
	Model     string   `json:"model"`
	Reasoning string   `json:"reasoning"`
	Identity  string   `json:"agent_identity"`
	Pitput    bool     `json:"pitput"`
	Boards    []string `json:"pitput_boards"`
	GitLab    bool     `json:"gitlab"`
	Task      string   `json:"task"`
	Question  string   `json:"question"`
}

const autoSchema = `Return ONLY one JSON object with these allowed fields:
{"harness":"codex|claude|opencode", "model":"", "reasoning":"", "agent_identity":"", "pitput":false, "pitput_boards":[], "gitlab":false, "task":"", "question":""}.
Translate launcher intent, not instructions addressed to the launched agent. Keep task text separate. Preserve explicit identities byte for byte. Only set model/reasoning if requested; otherwise leave empty to use local defaults. Never guess a harness, identity, board or task. If ambiguous ask one short clarification in question. Pitput requires identity and at least one board. Do not invent GitLab access. No permission modes, shell commands, environment variables, arbitrary options or unlisted fields. Permissions can only be selected with explicit CLI flags outside --auto. Corrections replace conflicting earlier intent. An explicit_flags object supplies user CLI constraints. Preserve settings explicitly requested in the natural-language text even when they conflict with explicit_flags: local validation will report the conflict. Use explicit_flags only to fill information missing from the text; never silently rewrite a conflicting request.`

func decodeAuto(data []byte) (autoProposal, error) {
	var p autoProposal
	if len(data) > autoLimit {
		return p, errors.New("auto proposal too large")
	}
	// encoding/json otherwise accepts duplicate keys and null scalar values.
	fields := json.NewDecoder(bytes.NewReader(data))
	opening, err := fields.Token()
	if err != nil || opening != json.Delim('{') {
		return p, errors.New("auto proposal must be an object")
	}
	seen := map[string]bool{}
	for fields.More() {
		key, err := fields.Token()
		if err != nil {
			return p, errors.New("invalid auto proposal field")
		}
		name, ok := key.(string)
		if !ok || !slices.Contains([]string{"harness", "model", "reasoning", "agent_identity", "pitput", "pitput_boards", "gitlab", "task", "question"}, name) {
			return p, errors.New("unknown auto proposal field")
		}
		if seen[name] {
			return p, errors.New("duplicate auto proposal field")
		}
		seen[name] = true
		var value json.RawMessage
		if fields.Decode(&value) != nil || bytes.Equal(value, []byte("null")) {
			return p, errors.New("invalid or null auto proposal field")
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, errors.New("invalid auto proposal (expected allowlisted JSON object)")
	}
	var rest any
	if dec.Decode(&rest) != io.EOF {
		return p, errors.New("invalid trailing auto proposal data")
	}
	for _, s := range append([]string{p.Harness, p.Model, p.Reasoning, p.Identity, p.Task, p.Question}, p.Boards...) {
		if strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) >= 0 {
			return p, errors.New("auto proposal contains control characters")
		}
	}
	return p, nil
}

func autoInput(args []string) (string, map[string][]string, error) {
	text := strings.TrimPrefix(args[0], "--auto=")
	start := 1
	if args[0] == "--auto" {
		if len(args) < 2 {
			return "", nil, errors.New("--auto requires a request")
		}
		text = args[1]
		start = 2
	}
	if strings.TrimSpace(text) == "" || len(text) > 8192 {
		return "", nil, errors.New("auto request must contain 1..8192 bytes")
	}
	explicit := map[string][]string{}
	for i := start; i < len(args); i++ {
		name, value, eq := strings.Cut(args[i], "=")
		switch name {
		case "--model", "--reasoning", "--agent-identity", "--pitput-board":
			if !eq {
				i++
				if i == len(args) {
					return "", nil, errors.New("missing explicit auto option value")
				}
				value = args[i]
			}
			if value == "" || strings.HasPrefix(value, "-") {
				return "", nil, errors.New("invalid explicit auto option value")
			}
		case "--pitput", "--gitlab", "--yolo", "--no-yolo", "--dry-run", "--print":
			if eq {
				return "", nil, errors.New("boolean auto option takes no value")
			}
			value = "true"
		default:
			return "", nil, fmt.Errorf("option %q cannot be combined with --auto; use an explicit launch for passthrough, resume or other integrations", name)
		}
		if len(explicit[name]) > 0 && name != "--pitput-board" {
			return "", nil, errors.New("duplicate explicit auto option")
		}
		explicit[name] = append(explicit[name], value)
	}
	if explicit["--yolo"] != nil && explicit["--no-yolo"] != nil {
		return "", nil, errors.New("conflicting permission flags")
	}
	encoded, _ := json.Marshal(explicit)
	if len(encoded) > 8192 {
		return "", nil, errors.New("explicit auto options exceed 8192 bytes")
	}
	return text, explicit, nil
}

func autoRoute(s catalog.Snapshot, configured string) (string, error) {
	if configured != "" {
		m, e := s.Resolve(configured, "opencode")
		return m.Route, e
	}
	var routes []string
	for _, m := range s.Models {
		if m.ID == "inkling" && m.Harnesses["opencode"].WireAPI == "chat_completions" {
			routes = append(routes, m.Route)
		}
	}
	if len(routes) == 1 {
		return routes[0], nil
	}
	slices.Sort(routes)
	return "", fmt.Errorf("Inkling is unavailable or ambiguous; set [auto].model to one authorized route (no fallback): %s", strings.Join(routes, ", "))
}

type autoGateway struct {
	settings   Settings
	key, route string
	client     *http.Client
}

func newAutoGateway(ctx context.Context) (autoGateway, error) {
	config, _, err := loadUserConfig()
	if err != nil {
		return autoGateway{}, err
	}
	harness := config.Auto.ProviderHarness
	if harness == "" {
		harness = "opencode"
	}
	return openAutoGateway(ctx, harness, config.Auto.Model)
}

func openAutoGateway(ctx context.Context, harness, model string) (autoGateway, error) {
	var g autoGateway
	var err error
	g.settings, err = loadHarnessSettings(harness)
	if err != nil {
		return g, err
	}
	snapshot, key, err := refreshWithCredential(ctx, g.settings)
	if err != nil {
		return g, err
	}
	g.key = key
	g.route, err = autoRoute(snapshot, model)
	if err != nil {
		return g, err
	}
	g.client, err = gatewayHTTPClient(30 * time.Second)
	return g, err
}

func (g autoGateway) infer(ctx context.Context, request string, explicit map[string][]string) (autoProposal, error) {
	var p autoProposal
	payload, _ := json.Marshal(map[string]any{"request": request, "explicit_flags": explicit})
	body, _ := json.Marshal(map[string]any{"model": g.route, "messages": []map[string]string{{"role": "system", "content": autoSchema}, {"role": "user", "content": string(payload)}}, "response_format": map[string]string{"type": "json_object"}, "max_tokens": 2048, "temperature": 0})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.settings.Provider.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return p, errors.New("invalid auto inference endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+g.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "hrx/0.1")
	resp, err := g.client.Do(req)
	if err != nil {
		return p, errors.New("auto inference failed or timed out; no fallback attempted")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return p, fmt.Errorf("auto inference returned HTTP %d (body suppressed); no fallback attempted", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, autoLimit+1))
	if err != nil || len(data) > autoLimit {
		return p, errors.New("auto inference response unreadable or too large")
	}
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &envelope) != nil || len(envelope.Choices) != 1 || envelope.Choices[0].Finish != "stop" {
		return p, errors.New("auto inference returned an incomplete or malformed response")
	}
	return decodeAuto([]byte(envelope.Choices[0].Message.Content))
}

var autoIdentity = regexp.MustCompile(`(?i:\b(?:as|identity(?: is)?))[ \t]+([A-Za-z0-9_.:-]+/[A-Za-z0-9_.:-]+/[A-Za-z0-9_.:-]+)`)

func autoArguments(p autoProposal, explicit map[string][]string, request string) ([]string, string, error) {
	if p.Question != "" {
		return nil, p.Question, nil
	}
	if !slices.Contains([]string{"codex", "claude", "opencode"}, p.Harness) {
		return nil, "Which harness: codex, claude, or opencode?", nil
	}
	for name, target := range map[string]*string{"--model": &p.Model, "--reasoning": &p.Reasoning, "--agent-identity": &p.Identity} {
		if values := explicit[name]; len(values) > 0 {
			if *target != "" && *target != values[0] {
				return nil, "", fmt.Errorf("auto proposal conflicts with explicit %s", name)
			}
			*target = values[0]
		}
	}
	if explicit["--pitput"] != nil {
		p.Pitput = true
	}
	if explicit["--gitlab"] != nil {
		p.GitLab = true
	}
	if boards := explicit["--pitput-board"]; boards != nil {
		if len(p.Boards) > 0 && !slices.Equal(p.Boards, boards) {
			return nil, "", errors.New("auto proposal conflicts with explicit --pitput-board")
		}
		p.Boards = boards
	}
	// Check explicitly stated launch identities, not unrelated peer mentions in a task.
	identityRequest := request
	if p.Task != "" {
		identityRequest = strings.ReplaceAll(identityRequest, p.Task, "")
	}
	ids := autoIdentity.FindAllStringSubmatch(identityRequest, -1)
	if len(ids) > 0 && p.Identity != ids[len(ids)-1][1] {
		return nil, "", errors.New("auto proposal did not preserve the requested agent identity")
	}
	if (p.Pitput || p.GitLab) && p.Identity == "" {
		return nil, "Which exact agent identity should be used?", nil
	}
	if p.Pitput && len(p.Boards) == 0 {
		return nil, "Which Pitput board should this agent join?", nil
	}
	if p.Identity != "" && !brokerIdentity.MatchString(p.Identity) {
		return nil, "", errors.New("invalid auto agent identity")
	}
	args := []string{p.Harness}
	for _, pair := range [][2]string{{"--model", p.Model}, {"--reasoning", p.Reasoning}, {"--agent-identity", p.Identity}} {
		if pair[1] != "" {
			args = append(args, pair[:]...)
		}
	}
	if p.Pitput {
		args = append(args, "--pitput")
	}
	for _, board := range p.Boards {
		args = append(args, "--pitput-board", board)
	}
	if p.GitLab {
		args = append(args, "--gitlab")
	}
	if explicit["--yolo"] != nil {
		args = append(args, "--yolo")
	} else {
		args = append(args, "--no-yolo")
	}
	if explicit["--dry-run"] != nil {
		args = append(args, "--dry-run")
	}
	if p.Task != "" {
		if strings.HasPrefix(p.Task, "-") || (p.Harness == "opencode" && strings.Contains(p.Task, "\x00")) {
			return nil, "", errors.New("auto task cannot begin with an option")
		}
		// OpenCode's positional argument is a project path; --prompt is its initial task.
		if p.Harness == "opencode" {
			args = append(args, "--", "--prompt", p.Task)
		} else {
			args = append(args, "--", "--", p.Task)
		}
	}
	if _, err := parseLaunchFor(p.Harness, args[1:]); err != nil {
		return nil, "", err
	}
	return args, "", nil
}

func validateAuto(ctx context.Context, args []string) ([]string, error) {
	s, err := loadHarnessSettings(args[0])
	if err != nil {
		return nil, err
	}
	o, err := parseLaunchFor(args[0], args[1:])
	if err != nil {
		return nil, err
	}
	if o.Model == "" && s.DefaultModel == "" {
		return nil, autoQuestion("No default model is configured. Which model or exact gateway route should be used?")
	}
	snapshot, err := Refresh(ctx, s)
	if err != nil {
		return nil, err
	}
	var l Launch
	if args[0] == "codex" {
		l, err = BuildLaunch(s, snapshot, o)
	} else {
		l, err = buildOtherLaunch(s, snapshot, o, "")
	}
	if err != nil {
		return nil, err
	}
	// Pin local defaults in the approved argument array so the preview is explicit.
	if o.Model == "" {
		args = slices.Insert(args, 1, "--model", l.Model)
	}
	if o.Reasoning == "" && l.Reasoning != "" {
		args = slices.Insert(args, 1, "--reasoning", l.Reasoning)
	}
	return args, nil
}

type autoInfer func(context.Context, string, map[string][]string) (autoProposal, error)
type autoValidate func(context.Context, []string) ([]string, error)
type autoQuestion string

func (q autoQuestion) Error() string { return string(q) }

func runAuto(ctx context.Context, args []string, in io.Reader, stdout, out io.Writer, interactive bool, launch func([]string) error) error {
	request, explicit, err := autoInput(args)
	if err != nil {
		return err
	}
	// --print only selects this mode: it is never sent to the parser or launched.
	printOnly := explicit["--print"] != nil
	delete(explicit, "--print")
	if !interactive && !printOnly {
		return errors.New("--auto requires an interactive terminal for y/n/reprompt; use --print or explicit HRX arguments in scripts")
	}
	if printOnly {
		fmt.Fprintln(out, "hrx: Auto parsing sends your request to LiteLLM. Do not include secrets. --print never launches or enrolls an agent.")
		g, err := newAutoGateway(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "hrx: Auto parser model: %s\n", g.route)
		return autoPrint(ctx, request, explicit, stdout, out, g.infer, validateAuto)
	}
	fmt.Fprintln(out, "hrx: Auto parsing sends your request to LiteLLM. Do not include secrets. No agent is launched or enrolled until you approve.")
	g, err := newAutoGateway(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "hrx: Auto parser model: %s\n", g.route)
	return autoLoop(ctx, request, explicit, in, out, g.infer, validateAuto, launch)
}

// autoPrint writes the parser's best single proposal to stdout as one quoted
// command line. It asks nothing: a needed clarification is reported on out and
// returned as an error rather than guessed.
func autoPrint(ctx context.Context, request string, explicit map[string][]string, stdout, out io.Writer, infer autoInfer, validate autoValidate) error {
	p, err := infer(ctx, request, explicit)
	if err != nil {
		return err
	}
	argv, question, err := autoArguments(p, explicit, request)
	if err != nil {
		return err
	}
	if question == "" {
		argv, err = validate(ctx, argv)
		var asked autoQuestion
		if errors.As(err, &asked) {
			question, err = string(asked), nil
		}
		if err != nil {
			return err
		}
	}
	if question != "" {
		fmt.Fprintf(out, "hrx: Needs clarification: %s\n", question)
		return errors.New("auto proposal needs clarification; add the missing detail to the request")
	}
	fmt.Fprint(stdout, "hrx")
	for _, a := range argv {
		fmt.Fprintf(stdout, " '%s'", strings.ReplaceAll(a, "'", "'\"'\"'"))
	}
	fmt.Fprintln(stdout)
	return nil
}

func autoLoop(ctx context.Context, request string, explicit map[string][]string, in io.Reader, out io.Writer, infer autoInfer, validate autoValidate, launch func([]string) error) error {
	reader := bufio.NewReaderSize(in, 8193)
	read := func() (string, error) {
		data, err := reader.ReadSlice('\n')
		line := string(data)
		if len(line) > 8192 {
			return "", errors.New("auto reply too long")
		}
		if err != nil {
			return "", errors.New("auto cancelled (input closed)")
		}
		return strings.TrimSpace(line), nil
	}
	for round := 0; round < 8; round++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		p, err := infer(ctx, request, explicit)
		if err != nil {
			return err
		}
		argv, question, err := autoArguments(p, explicit, request)
		if err != nil {
			return err
		}
		if question != "" {
			fmt.Fprintf(out, "hrx: %s (enter n to cancel)\n> ", question)
			answer, err := read()
			if err != nil {
				return err
			}
			if answer == "n" {
				fmt.Fprintln(out, "hrx: Cancelled.")
				return nil
			}
			request += "\nClarification: " + answer
			if len(request) > autoLimit {
				return errors.New("auto conversation too long")
			}
			continue
		}
		argv, err = validate(ctx, argv)
		if err != nil {
			var question autoQuestion
			if errors.As(err, &question) {
				fmt.Fprintf(out, "hrx: %s (enter n to cancel)\n> ", question)
				answer, readErr := read()
				if readErr != nil {
					return readErr
				}
				if answer == "n" {
					fmt.Fprintln(out, "hrx: Cancelled.")
					return nil
				}
				request += "\nClarification: " + answer
				if len(request) > autoLimit {
					return errors.New("auto conversation too long")
				}
				continue
			}
			return err
		}
		fmt.Fprintln(out, "hrx: Proposed launch (quoted arguments, never evaluated as shell):")
		fmt.Fprint(out, "  hrx")
		for _, a := range argv {
			fmt.Fprintf(out, " '%s'", strings.ReplaceAll(a, "'", "'\"'\"'"))
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, "hrx: Harness, model, identity and integrations are launcher settings; text after -- is the agent task.")
		if explicit["--yolo"] != nil {
			fmt.Fprintln(out, "hrx: WARNING: explicit --yolo disables approval prompts.")
		} else {
			fmt.Fprintln(out, "hrx: --auto keeps permission prompts enabled (--no-yolo).")
		}
		for {
			fmt.Fprint(out, "Launch? [y/n/reprompt] ")
			answer, err := read()
			if err != nil {
				return err
			}
			switch strings.ToLower(answer) {
			case "y", "yes":
				if err := ctx.Err(); err != nil {
					return err
				}
				return launch(argv)
			case "n", "no":
				fmt.Fprintln(out, "hrx: Cancelled.")
				return nil
			case "r", "reprompt":
				fmt.Fprint(out, "Correction: ")
				correction, err := read()
				if err != nil {
					return err
				}
				request += "\nCorrection: " + correction
				if len(request) > autoLimit {
					return errors.New("auto conversation too long")
				}
			default:
				continue
			}
			break
		}
	}
	return errors.New("auto reached eight proposals/clarifications; start again with a more specific request")
}
