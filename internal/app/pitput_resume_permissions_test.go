package app

import (
	"reflect"
	"testing"
)

func TestPitputResumeYoloMovesToServer(t *testing.T) {
	for _, flag := range []string{"--yolo", "--dangerously-bypass-approvals-and-sandbox"} {
		args := []string{flag, "resume", "native", "--", flag}
		want := []string{"app-server", "-c", `approval_policy="never"`, "-c", `sandbox_mode="danger-full-access"`}
		if got := pitputServerArgs(args); !reflect.DeepEqual(got, want) {
			t.Fatalf("server %q", got)
		}
		want = []string{"resume", "native", "--", flag}
		if got := pitputTerminalArgs(args); !reflect.DeepEqual(got, want) {
			t.Fatalf("terminal %q", got)
		}
		fresh := []string{flag, "new task"}
		if got := pitputTerminalArgs(fresh); !reflect.DeepEqual(got, fresh) {
			t.Fatalf("changed fresh launch %q", got)
		}
	}
}
