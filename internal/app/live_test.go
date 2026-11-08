package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Opt-in capability probe: never run paid requests in the ordinary test suite.
func TestLiveReasoningProbe(t *testing.T) {
	model := os.Getenv("HRX_PROBE_MODEL")
	if model == "" {
		t.Skip("set HRX_PROBE_MODEL and HRX_PROBE_EFFORTS for an explicit live probe")
	}
	levels := strings.Split(os.Getenv("HRX_PROBE_EFFORTS"), ",")
	if len(levels) == 0 || levels[0] == "" {
		t.Fatal("HRX_PROBE_EFFORTS is required")
	}
	s, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	key, err := credential(context.Background(), s.Provider)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, level := range levels {
		t.Run(level, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"model": model, "input": "Reply with OK only.", "max_output_tokens": 32, "reasoning": map[string]string{"effort": level}, "store": false})
			req, err := http.NewRequest(http.MethodPost, s.Provider.BaseURL+"/responses", bytes.NewReader(body))
			if err != nil {
				t.Fatal("invalid endpoint")
			}
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal("request failed; details suppressed")
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("HTTP %d (body suppressed)", resp.StatusCode)
			}
			var result struct {
				Reasoning struct {
					Effort string `json:"effort"`
				} `json:"reasoning"`
				Usage struct {
					Total int `json:"total_tokens"`
				} `json:"usage"`
			}
			if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
				t.Fatal("invalid response")
			}
			if result.Reasoning.Effort != "" && result.Reasoning.Effort != level {
				t.Fatalf("reasoning effort changed to %q", result.Reasoning.Effort)
			}
			t.Logf("accepted model=%s effort=%s echoed_effort=%q total_tokens=%d", model, level, result.Reasoning.Effort, result.Usage.Total)
		})
	}
}
