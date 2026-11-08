package coveragecontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/meshcrawler/hrx/internal/codexlaunch"
)

func bootstrapResponder(ctx context.Context, wire *controllerWire, mode string) error {
	return bootstrapResponderParams(ctx, wire, mode, nil)
}
func bootstrapResponderParams(ctx context.Context, wire *controllerWire, mode string, expected json.RawMessage) error {
	methods := []string{"initialize", "initialized", "thread/start", "thread/name/set", "thread/read"}
	results := []string{`{}`, ``, bootstrapStart, `{}`, bootstrapRead}
	if mode == "resume" {
		methods[2] = "thread/resume"
		results[4] = strings.Replace(bootstrapRead, `"turns":[]`, `"status":{"type":"idle"},"turns":[{"id":"old-turn","status":"completed"}]`, 1)
	}

	if mode == "profile" {
		methods = []string{"initialize", "initialized", "config/read", "thread/start", "thread/name/set", "thread/read"}
		results = []string{`{}`, ``, `{"config":{"developer_instructions":"base"},"origins":{"developer_instructions":{"name":{"type":"user"}}}}`, bootstrapStart, `{}`, bootstrapRead}
	}
	for i, method := range methods {
		var request struct {
			ID     string          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		select {
		case raw := <-wire.out:
			if json.Unmarshal(raw, &request) != nil || request.Method != method {
				return fmt.Errorf("unexpected bootstrap method, wanted %s", method)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		if (method == "thread/start" || method == "thread/resume") && expected != nil && !bytes.Equal(request.Params, expected) {
			return fmt.Errorf("native startup fields changed on wire")
		}
		if method == "config/read" && string(request.Params) != `{"cwd":"/fixture"}` {
			return fmt.Errorf("config read used wrong cwd")
		}
		if method == "initialized" {
			if request.ID != "" {
				return fmt.Errorf("initialized is not a notification")
			}
			continue
		}
		if method == "thread/name/set" || method == "thread/read" {
			var params struct {
				Thread  string `json:"threadId"`
				Name    string `json:"name"`
				Include bool   `json:"includeTurns"`
			}
			if json.Unmarshal(request.Params, &params) != nil || params.Thread != "thread" || method == "thread/name/set" && params.Name != "fixture" || method == "thread/read" && !params.Include {
				return fmt.Errorf("startup RPC lost exact baseline parameters")
			}
		}
		body := map[string]any{"id": request.ID, "result": json.RawMessage(results[i])}
		if method == "thread/start" {
			notification := `{"method":"thread/started","params":{}}`
			if mode == "premature-work" {
				notification = `{"method":"turn/started","params":{}}`
			}
			wire.in <- []byte(notification)
			if mode == "remote-error" {
				body = map[string]any{"id": request.ID, "error": map[string]any{"code": -1, "message": "refused"}}
			}
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		wire.in <- raw
		if method == "thread/start" && mode != "" && mode != "profile" {
			return nil
		}
	}
	return nil
}

func fixtureStart() codexlaunch.ThreadStart {
	return codexlaunch.ThreadStart{Cwd: "/fixture", HistoryMode: "paginated"}
}

func collectFixtureBootstrap(t *testing.T, g *codexGate, wire *controllerWire) *bootstrap {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		err := bootstrapResponder(ctx, wire, "")
		if err != nil {
			wire.Close()
		}
		done <- err
	}()
	b, err := collectBootstrap(ctx, g, fixtureStart(), "fixture", 8, 8192, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBootstrapRPCCollectsActualCallAndNextPrefix(t *testing.T) {
	g, wire := actualGate(t)
	b := collectFixtureBootstrap(t, g, wire)
	r, err := b.seal()
	if err != nil || r.through != 5 || r.revision != g.binding.Revision || r.connection.nonce != g.binding.Nonce {
		t.Fatal(r, err)
	}
	if _, ok := g.client.Current(); ok {
		t.Fatal("bootstrap collection published authority")
	}
}

func TestBootstrapRPCFailureRevokesBeforeReturning(t *testing.T) {
	for _, mode := range []string{"remote-error", "premature-work", "unbounded", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			g, wire := actualGate(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var run context.Context = ctx
			done := make(chan error, 1)
			if mode == "unbounded" {
				run = context.Background()
			} else if mode == "canceled" {
				cancel()
			} else {
				go func() {
					err := bootstrapResponder(ctx, wire, mode)
					if err != nil {
						wire.Close()
					}
					done <- err
				}()
			}
			if b, err := collectBootstrap(run, g, fixtureStart(), "fixture", 8, 8192, 2048); err == nil || b != nil {
				t.Fatal("failed bootstrap returned evidence", err)
			}
			if _, err := g.client.Publish(g.binding, 0, [32]byte{}); err == nil {
				t.Fatal("failed bootstrap retained publication permission")
			}
			if mode == "unbounded" || mode == "canceled" {
				select {
				case <-wire.out:
					t.Fatal("invalid budget sent native traffic")
				default:
				}
			} else if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBootstrapRPCPreservesCompleteRequestOnWire(t *testing.T) {
	g, wire := actualGate(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw := json.RawMessage(`{"cwd":"/fixture","historyMode":"paginated","permissions":"named-profile","developerInstructions":"native <terminal> instructions","config":{"features":{"terminal_visualization":true}}}`)
	expected, err := preservedStart(raw, 2048)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		err := bootstrapResponderParams(ctx, wire, "", expected)
		if err != nil {
			wire.Close()
		}
		done <- err
	}()
	b, err := collectBootstrapRequest(ctx, g, raw, "fixture", 8, 8192, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if b.requestHash != sha256.Sum256(expected) {
		t.Fatal("journal digest does not bind sent request")
	}
	if _, err := b.seal(); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.client.Current(); ok {
		t.Fatal("request preservation alone admitted work")
	}
}
