package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func networkFixture(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "network.toml")
	data := `name="experiment"
tags=["hrx-test"]
[[agents]]
role="a"
host="host"
profile="worker"
identity="host/a/agent"
boards=["test"]
prompt_file="a.md"
[[agents]]
role="b"
host="host"
profile="worker"
identity="host/b/agent"
boards=["test"]
prompt_file="b.md"
`
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestRemoteNetworkBarrierAndTargetedRetry(t *testing.T) {
	for _, failure := range []string{"", "prepare", "recheck", "preflight"} {
		t.Run(failure, func(t *testing.T) {
			path := networkFixture(t)
			var events []string
			calls := map[string]int{}
			prepare := func(ctx context.Context, args []string, out io.Writer, mode remoteRunMode) error {
				name := args[slices.Index(args, "--name")+1]
				role := strings.TrimPrefix(name, "experiment/")
				if slices.Contains(args, "--plan") {
					events = append(events, "plan-"+role)
					if failure == "preflight" && role == "b" {
						return errors.New("invalid artifact")
					}
					_, err := io.WriteString(out, `{"ready":false}`)
					return err
				}
				calls[role]++
				events = append(events, "check-"+role)
				if role == "b" && !mode.ObserveOnly {
					t.Fatal("targeted retry restarted healthy role")
				}
				if role == "a" && calls[role] == 1 && mode.ObserveOnly {
					t.Fatal("selected role was not prepared")
				}
				if (failure == "prepare" && role == "a" && calls[role] == 1) || (failure == "recheck" && role == "b" && calls[role] == 2) {
					return errors.New("not ready")
				}
				mode.DeferWork(func(context.Context) error { events = append(events, "release-"+role); return nil })
				return nil
			}
			var output bytes.Buffer
			err := runRemoteNetworkWith(context.Background(), []string{"--file", path, "--only", "a"}, &output, prepare)
			if failure != "" {
				if err == nil {
					t.Fatal("failure was ignored")
				}
				for _, event := range events {
					if strings.HasPrefix(event, "release-") {
						t.Fatal("work crossed failed barrier")
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			expected := []string{"plan-a", "plan-b", "check-a", "check-b", "check-a", "check-b", "release-a", "release-b"}
			if !slices.Equal(events, expected) {
				t.Fatalf("ordering: %v", events)
			}
		})
	}
}
func TestRemoteNetworkUnknownRoleHasNoEffects(t *testing.T) {
	err := runRemoteNetworkWith(context.Background(), []string{"--file", networkFixture(t), "--only", "missing"}, io.Discard, func(context.Context, []string, io.Writer, remoteRunMode) error {
		t.Fatal("unexpected operation")
		return nil
	})
	if err == nil {
		t.Fatal("unknown role accepted")
	}
}
