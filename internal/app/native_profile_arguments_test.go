package app

import "testing"

func TestCodexProfileAndModelValuesAreNotCommands(t *testing.T) {
	for _, flag := range []string{"--profile", "-p", "--model", "-m"} {
		args := []string{flag, "resume", "resume", "native-id"}
		if got := codexResumePosition(args); got != 2 {
			t.Fatal(flag, "resume position", got)
		}
		if got := nativeCommand("codex", args); got != "resume" {
			t.Fatal(flag, "command", got)
		}
		if got := codexResumePosition([]string{flag, "resume", "prompt"}); got != -1 {
			t.Fatal(flag, "option value became resume")
		}
		if got := codexResumePosition([]string{flag + "=fixture", "resume", "native-id"}); got != 1 {
			t.Fatal(flag, "equals option broke resume", got)
		}
	}
	if got := codexResumePosition([]string{"--", "--profile", "resume"}); got != -1 {
		t.Fatal("literal prompt became command")
	}
	if got := codexResumePosition([]string{"-c", `developer_instructions="resume"`, "--profile", "work", "resume", "native-id"}); got != 4 {
		t.Fatal("config/profile prefixes broke resume", got)
	}
}
