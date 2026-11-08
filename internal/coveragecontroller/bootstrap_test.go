package coveragecontroller

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

const bootstrapStart = `{"thread":{"id":"thread","sessionId":"session"},"model":"fixture","modelProvider":"fixture","cwd":"/fixture","approvalPolicy":"never","approvalsReviewer":"user","sandbox":{"type":"readOnly","networkAccess":false},"serviceTier":null,"reasoningEffort":null,"runtimeWorkspaceRoots":[]}`
const bootstrapRead = `{"thread":{"id":"thread","sessionId":"session","name":"fixture","canAcceptDirectInput":true,"parentThreadId":null,"historyMode":"paginated","turns":[]}}`

func TestBootstrapThreadBoundariesCannotSeal(t *testing.T) {
	for _, method := range []string{"thread/closed", "thread/archived", "thread/deleted", "thread/reverted", "thread/settings/updated"} {
		for _, beforeIdentity := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/before-identity=%t", method, beforeIdentity), func(t *testing.T) {
				b, err := newBootstrap(testConnection, 1, "fixture", 8, 8192, 2048)
				if err != nil {
					t.Fatal(err)
				}
				frames, methods := bootstrapFrames()
				count := 3
				if beforeIdentity {
					count = 1
				}
				for i := 0; i < count; i++ {
					if err := b.accept(frames[i], methods[i]); err != nil {
						t.Fatal(err)
					}
				}
				raw := []byte(fmt.Sprintf(`{"method":%q,"params":{"threadId":"thread"}}`, method))
				if err := b.accept(frame{testConnection, uint64(count + 1), notificationFrame, raw}, ""); err == nil {
					t.Fatal("bootstrap accepted thread boundary")
				}
				if _, err := b.seal(); err == nil || b.native.Identity.ThreadID != "" {
					t.Fatal("invalidated bootstrap retained usable evidence")
				}
			})
		}
	}
}

func bootstrapFrames() ([]frame, []string) {
	return []frame{
		{testConnection, 1, responseFrame, []byte(`{"result":{}}`)},
		{testConnection, 2, notificationFrame, []byte(`{"method":"thread/started","params":{}}`)},
		{testConnection, 3, responseFrame, []byte(`{"result":` + bootstrapStart + `}`)},
		{testConnection, 4, responseFrame, []byte(`{"result":{}}`)},
		{testConnection, 5, responseFrame, []byte(`{"result":` + bootstrapRead + `}`)},
	}, []string{"initialize", "", "thread/start", "thread/name/set", "thread/read"}
}

func TestBootstrapConsumesExactResponseAndNotificationPrefix(t *testing.T) {
	for _, order := range [][]int{{0, 1, 2, 3, 4}, {0, 2, 3, 4, 1}, {4, 3, 2, 1, 0}} {
		b, err := newBootstrap(testConnection, 1, "fixture", 8, 8192, 2048)
		if err != nil {
			t.Fatal(err)
		}
		frames, methods := bootstrapFrames()
		for _, i := range order {
			if err := b.accept(frames[i], methods[i]); err != nil {
				t.Fatal(err)
			}
		}
		r, err := b.seal()
		if err != nil || r.connection != testConnection || r.through != 5 || r.identity.ThreadID != "thread" || r.identity.SessionID != "session" || r.startHash != sha256.Sum256([]byte(bootstrapStart)) || r.readHash != sha256.Sum256([]byte(bootstrapRead)) {
			t.Fatal(r, err)
		}
		if _, err := b.seal(); err == nil {
			t.Fatal("bootstrap receipt reused")
		}
	}
}

func TestBootstrapRefusesGapsForeignEvidenceAndPrematureWork(t *testing.T) {
	for _, mode := range []string{"gap", "foreign", "remote-error", "method-order", "wrong-session", "nonempty", "turn", "server-request", "duplicate", "loss", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			b, err := newBootstrap(testConnection, 1, "fixture", 8, 8192, 2048)
			if err != nil {
				t.Fatal(err)
			}
			frames, methods := bootstrapFrames()
			switch mode {
			case "foreign":
				frames[2].connection.nonce = "other"
			case "remote-error":
				frames[2].raw = []byte(`{"error":{"code":1,"message":"refused"}}`)
			case "method-order":
				methods[2] = "thread/read"
			case "wrong-session":
				frames[4].raw = []byte(strings.ReplaceAll(string(frames[4].raw), `"session"`, `"other"`))
			case "nonempty":
				frames[4].raw = []byte(strings.ReplaceAll(string(frames[4].raw), `"turns":[]`, `"turns":[{}]`))
			case "turn":
				frames[1].raw = []byte(`{"method":"turn/started"}`)
			case "server-request":
				frames[1].kind = serverRequestFrame
			case "overflow":
				b.prefix.maxFrame = 16
			}
			for i, f := range frames {
				if mode == "gap" && i == 1 {
					continue
				}
				if mode == "loss" && i == 2 {
					b.fail(errCoordinatorLost)
				}
				if err = b.accept(f, methods[i]); err != nil {
					break
				}
				if mode == "duplicate" && i == 0 {
					err = b.accept(f, methods[i])
					break
				}
			}
			if _, sealErr := b.seal(); sealErr == nil {
				t.Fatal("invalid bootstrap accepted", mode, err)
			}
			if b.prefix.cause == nil || b.native.Identity.ThreadID != "" {
				t.Fatal("failed bootstrap retained usable evidence")
			}
		})
	}
}

func TestResumeBaselinePreservesExistingNativeTitle(t *testing.T) {
	b, err := newBootstrap(testConnection, 1, "host-label", 8, 8192, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b.step = 1
	b.resumeThread = "thread"
	result := strings.Replace(bootstrapStart, `"sessionId":"session"`, `"sessionId":"session","name":"My existing conversation"`, 1)
	f := frame{testConnection, 1, responseFrame, []byte(`{"result":` + result + `}`)}
	if err := b.reduce(f, "thread/resume"); err != nil {
		t.Fatal(err)
	}
	if b.name != "My existing conversation" {
		t.Fatal("resume replaced the user's title", b.name)
	}
}

// Native TUI reads hook metadata before selecting its thread. This response
// fills the receive prefix but cannot advance thread admission or seal it.
func TestBootstrapMetadataDoesNotAdmitWork(t *testing.T) {
	for _, method := range []string{"hooks/list", "collaborationMode/list", "account/bedrock/checkGovCloudRequirements"} {
		t.Run(method, func(t *testing.T) {
			b, err := newBootstrap(testConnection, 1, "fixture", 8, 8192, 2048)
			if err != nil {
				t.Fatal(err)
			}
			if err := b.accept(frame{testConnection, 1, responseFrame, []byte(`{"result":{}}`)}, "initialize"); err != nil {
				t.Fatal(err)
			}
			if !startupRead(method) {
				t.Fatal("native startup metadata refused")
			}
			if err := b.accept(frame{testConnection, 2, responseFrame, []byte(`{"result":{}}`)}, method); err != nil {
				t.Fatal(err)
			}
			if b.step != 1 || b.prefix.consumed != 2 || b.native.ThreadID != "" {
				t.Fatal("metadata changed admission")
			}
			if _, err := b.seal(); err == nil {
				t.Fatal("metadata admitted work")
			}
		})
	}
}

func TestFreshBootstrapBindsNativeDefaultCollaborationMode(t *testing.T) {
	b, err := newBootstrap(testConnection, 1, "fixture", 8, 8192, 2048)
	if err != nil {
		t.Fatal(err)
	}
	frames, methods := bootstrapFrames()
	for i, f := range frames {
		if err := b.accept(f, methods[i]); err != nil {
			t.Fatal(err)
		}
	}
	if !b.native.SameTurnSetting("collaborationMode", []byte(`{"mode":"default","settings":{"model":"fixture","reasoning_effort":null,"developer_instructions":null}}`)) {
		t.Fatal("fresh native default not admitted")
	}
	if b.native.SameTurnSetting("collaborationMode", []byte(`{"mode":"plan","settings":{"model":"fixture","reasoning_effort":null,"developer_instructions":null}}`)) {
		t.Fatal("mode change admitted")
	}
}
