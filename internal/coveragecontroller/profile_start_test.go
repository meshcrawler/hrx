package coveragecontroller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/codexlaunch"
)

func TestProfileStartUsesFreshNativeOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, result, want string
		fail               bool
	}{
		{"profile wins", `{"config":{"developer_instructions":"base"},"origins":{"developer_instructions":{"name":{"type":"user"}}}}`, "profile\n\nterminal", false},
		{"project wins", `{"config":{"developer_instructions":"project"},"origins":{"developer_instructions":{"name":{"type":"project"}}}}`, "terminal", false},
		{"unknown origin", `{"config":{"developer_instructions":"unknown"},"origins":{"developer_instructions":{"name":{"type":"futureLayer"}}}}`, "", true},
		{"missing origin", `{"config":{"developer_instructions":"base"},"origins":{}}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, w := actualGate(t)
			store := fakeStore()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			h, err := newGateway(ctx, g, store, "fixture", time.Second, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { store.unblock(); h.close(context.Background()) }()
			h.profileDeveloperInstructions = codexlaunch.Optional[string]{Present: true, Value: "profile"}
			served := make(chan error, 1)
			go func() {
				for _, method := range []string{"initialize", "initialized", "config/read"} {
					var req struct {
						ID     string
						Method string
						Params json.RawMessage
					}
					select {
					case raw := <-w.out:
						if err := json.Unmarshal(raw, &req); err != nil {
							served <- err
							return
						}
					case <-ctx.Done():
						served <- ctx.Err()
						return
					}
					if req.Method != method {
						served <- errBootstrap
						return
					}
					if method == "initialized" {
						continue
					}
					result := `{}`
					if method == "config/read" {
						if string(req.Params) != `{"cwd":"/fixture"}` {
							served <- errBootstrap
							return
						}
						result = tc.result
					}
					reply, _ := json.Marshal(map[string]any{"id": req.ID, "result": json.RawMessage(result)})
					w.in <- reply
				}
				served <- nil
			}()
			gatewayHandle(t, h, ctx, `{"id":1,"method":"initialize","params":{}}`)
			gatewayHandle(t, h, ctx, `{"method":"initialized"}`)
			raw, err := h.initializedProfileStart(ctx, json.RawMessage(`{"cwd":"/fixture","historyMode":"paginated","developerInstructions":"terminal"}`))
			if (err != nil) != tc.fail {
				t.Fatal("precedence result", err)
			}
			if !tc.fail {
				var got struct {
					Instructions string `json:"developerInstructions"`
				}
				if json.Unmarshal(raw, &got) != nil || got.Instructions != tc.want {
					t.Fatal("wrong initialized request", string(raw))
				}
			}
			if err := <-served; err != nil {
				t.Fatal(err)
			}
			if h.b.prefix.consumed != 2 {
				t.Fatal("config response not consumed", h.b.prefix.consumed)
			}
			select {
			case raw := <-w.out:
				t.Fatal("resolver released work", string(raw))
			default:
			}
		})
	}
}
