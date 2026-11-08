package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestYesodSelectionBoundsOutputAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		timeout      time.Duration
	}{
		{"oversized", "i=0; while [ $i -lt 5000 ]; do printf 'private-sentinel-value'; i=$((i+1)); done", 3 * time.Second},
		{"cancelled", "exec /bin/sleep 30", 50 * time.Millisecond},
		{"failure", "printf 'private-sentinel-value' >&2; exit 1", 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "python3"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()
			started := time.Now()
			agent, err := registerYesod(ctx, "codex", "fixture-route", "")
			if err == nil || agent != "" || strings.Contains(err.Error(), "private-sentinel") {
				t.Fatal("selection failure accepted or exposed diagnostics")
			}
			if time.Since(started) > tc.timeout+2*time.Second {
				t.Fatal("selection did not honor time bounds")
			}
		})
	}
}
