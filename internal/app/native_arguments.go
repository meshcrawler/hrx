package app

import (
	"slices"
	"strings"
)

type nativeArgument struct {
	index           int
	option, literal bool
}

// Only syntactic arguments are returned: known option values are skipped and
// tokens after -- are literal data. Share this view between resume detection
// and launch guards so prompt text cannot become a routing/lifecycle option.
func nativeArguments(harness string, args []string) []nativeArgument {
	values := map[string][]string{
		"codex":    {"-c", "--config", "-p", "--profile", "-m", "--model", "--image", "-i", "--output-last-message", "-o", "--output-schema", "--cd", "-C", "--sandbox", "-s", "--ask-for-approval", "-a", "--enable", "--disable", "--add-dir", "--color"},
		"claude":   {"--resume", "-r", "--append-system-prompt", "--system-prompt", "--system-prompt-snapshot", "--output-format", "--input-format", "--json-schema", "--max-turns", "--max-budget-usd", "--permission-mode", "--permission-profile", "--session-id", "--add-dir", "--allowedTools", "--disallowedTools", "--tools", "--mcp-config", "--name"},
		"opencode": {"--session", "-s", "--prompt", "--file", "-f", "--title", "--format", "--server", "--log-level", "--port", "--hostname", "--variant", "--model", "-m"},
	}
	var tokens []nativeArgument
	literal := false
	for i := 0; i < len(args); i++ {
		if !literal && args[i] == "--" {
			literal = true
			continue
		}
		option := !literal && strings.HasPrefix(args[i], "-") && args[i] != "-"
		tokens = append(tokens, nativeArgument{i, option, literal})
		name, _, equal := strings.Cut(args[i], "=")
		if option && !equal && slices.Contains(values[harness], name) {
			i++
		}
	}
	return tokens
}

func nativeCommand(harness string, args []string) string {
	if harness == "claude" {
		for _, token := range nativeArguments(harness, args) {
			if token.option && (args[token.index] == "-p" || args[token.index] == "--print") {
				return ""
			}
		}
	}
	for _, token := range nativeArguments(harness, args) {
		if !token.option && !token.literal {
			return args[token.index]
		}
	}
	return ""
}

func codexResumePosition(args []string) int {
	commandSeen := false
	for _, token := range nativeArguments("codex", args) {
		if token.option || token.literal {
			continue
		}
		arg := args[token.index]
		if arg == "resume" {
			return token.index
		}
		if !commandSeen && arg == "exec" {
			commandSeen = true
			continue
		}
		return -1
	}
	return -1
}

func openCodeRunPosition(args []string) int {
	for _, token := range nativeArguments("opencode", args) {
		if token.option || token.literal {
			continue
		}
		if args[token.index] == "run" {
			return token.index
		}
		break
	}
	return -1
}

// Run-specific flags must follow the command, even when root options precede it.
func insertOpenCodeFlags(args []string, flags ...string) []string {
	position := openCodeRunPosition(args) + 1
	result := make([]string, 0, len(args)+len(flags))
	result = append(result, args[:position]...)
	result = append(result, flags...)
	return append(result, args[position:]...)
}

func nativeHasOption(harness string, args []string, name string) bool {
	for _, token := range nativeArguments(harness, args) {
		if token.option && args[token.index] == name {
			return true
		}
	}
	return false
}
