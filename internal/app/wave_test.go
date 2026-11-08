package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const waveTestBlock = "11111111-1111-1111-1111-111111111111"
const waveTestClient = "22222222-2222-2222-2222-222222222222"

func waveTestEnv() map[string]string {
	return map[string]string{"WAVETERM_BLOCKID": waveTestBlock, "WAVETERM_CLIENTID": waveTestClient, "WAVETERM_JWT": "h." + base64.RawURLEncoding.EncodeToString([]byte(`{"blockid":"`+waveTestBlock+`"}`)) + ".s"}
}
func TestWaveEnvironment(t *testing.T) {
	for _, test := range []struct {
		name, key, value string
		valid            bool
	}{
		{name: "valid", valid: true}, {name: "no block", key: "WAVETERM_BLOCKID"}, {name: "no client", key: "WAVETERM_CLIENTID"}, {name: "malformed", key: "WAVETERM_CLIENTID", value: "this"}, {name: "missing auth", key: "WAVETERM_JWT"}, {name: "mismatch", key: "WAVETERM_BLOCKID", value: waveTestClient}, {name: "ssh", key: "SSH_CONNECTION", value: "x"}, {name: "CI", key: "CI", value: "true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := waveTestEnv()
			if test.key != "" {
				env[test.key] = test.value
			}
			_, _, ok := waveEnvironment(func(k string) string { return env[k] })
			if ok != test.valid {
				t.Fatalf("valid=%v", ok)
			}
		})
	}
}
func TestWaveMetadata(t *testing.T) {
	config := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(config, []byte("[wave.role_labels]\nmayor = \"Mayor\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRX_CONFIG", config)
	for _, test := range []struct {
		name, meta string
		fail       int
		want       bool
	}{
		{"label", `{"view":"term"}`, 0, true}, {"existing title", `{"view":"term","frame:text":"mine"}`, 0, false}, {"prior binding", `{"view":"term","hrx:wave":{}}`, 0, false}, {"other view", `{"view":"web"}`, 0, false}, {"invalid", `no`, 0, false}, {"stale client", `{}`, 1, false}, {"stale block", `{}`, 2, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			var patch map[string]any
			applyWaveLabel(context.Background(), Launch{agentIdentity: "laptop/acme-triage-mayor/agent", Identity: "kimi-k3", Executable: "opencode"}, waveTestBlock, waveTestClient, func(ctx context.Context, input []byte, args ...string) ([]byte, error) {
				calls++
				if calls == test.fail {
					return nil, errors.New("unavailable")
				}
				if calls == 1 {
					return []byte(`{}`), nil
				}
				if calls == 2 {
					return []byte(test.meta), nil
				}
				if args[0] != "setmeta" || args[2] != "block:"+waveTestBlock {
					t.Fatalf("wrong target: %v", args)
				}
				if err := json.Unmarshal(input, &patch); err != nil {
					t.Fatal(err)
				}
				return nil, nil
			})
			if (patch != nil) != test.want {
				t.Fatalf("patch=%v", patch)
			}
			if test.want {
				if len(patch) != 2 || patch["frame:text"] != "Mayor | Kimi K3 | OpenCode" {
					t.Fatalf("unexpected patch=%v", patch)
				}
			}
		})
	}
}
func TestWaveNoContextNeverExecutes(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "called")
	script := "#!/bin/sh\ntouch '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "wsh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("WAVETERM_BLOCKID", "")
	labelWavePane(Launch{})
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("wsh invoked outside Wave")
	}
}
func TestWaveCommandBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := waveCommand(ctx, "/bin/sh", nil, "-c", "sleep 10")
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("unbounded: %v %v", err, time.Since(start))
	}
}
func TestWaveLock(t *testing.T) {
	// Use a unique block so the test never contends with real panes.
	block := filepath.Base(t.TempDir())
	unlock := lockWavePane(block)
	if unlock == nil {
		t.Fatal("first lock failed")
	}
	defer unlock()
	if other := lockWavePane(block); other != nil {
		other()
		t.Fatal("concurrent launch acquired lock")
	}
}
func TestWaveLabelsSanitized(t *testing.T) {
	if got := waveText("mayor\n\x1b\u202eok"); got != "mayorok" {
		t.Fatal(got)
	}
	if len(waveText(strings.Repeat("x", 300))) != 160 {
		t.Fatal("unbounded label")
	}
	config := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(config, []byte("[wave.role_labels]\n\"aws-infra\" = \"AWS Infra\"\n\"dashboard-maker\" = \"Dashboard\"\n\"finai\" = \"FinAI\"\n\"maker\" = \"Maker\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRX_CONFIG", config)
	for role, want := range map[string]string{"acme-triage/aws-infra/agent": "AWS Infra", "laptop/acme-triage-dashboard-maker/agent": "Dashboard", "laptop/acme-triage-finai-agent-builder/agent": "FinAI", "laptop/reviewer/agent": "reviewer"} {
		if waveRole(role) != want {
			t.Fatal(role)
		}
	}
}
