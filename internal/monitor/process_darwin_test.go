package monitor

import (
	"encoding/binary"
	"strings"
	"testing"
)

func TestDarwinEnvironmentSeparatesPromptFromEnvironment(t *testing.T) {
	b := make([]byte, 4)
	binary.NativeEndian.PutUint32(b, 3)
	b = append(b, []byte("/bin/claude\x00\x00claude\x00-p\x00HRX_AGENT_IDENTITY=spoofed\x00HRX_AGENT_IDENTITY=actual\x00GITLAB_TOKEN=SECRET\x00\x00")...)
	env, err := darwinEnvironment(b)
	if err != nil || strings.Join(env, ",") != "HRX_AGENT_IDENTITY=actual" {
		t.Fatal("argv or secrets interpreted as metadata")
	}
	for _, bad := range [][]byte{nil, {0, 0, 0, 0}, {255, 255, 255, 255}} {
		if _, err := darwinEnvironment(bad); err == nil {
			t.Fatal("invalid process buffer accepted")
		}
	}
}
