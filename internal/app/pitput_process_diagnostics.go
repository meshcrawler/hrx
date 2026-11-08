package app

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// Retain only a bounded tail in memory. Never publish its contents: native
// stderr can contain credentials, prompts, paths, or upstream error bodies.
type nativeDiagnosticTail struct {
	mu   sync.Mutex
	data []byte
}

func (d *nativeDiagnosticTail) Write(p []byte) (int, error) {
	n := len(p)
	d.mu.Lock()
	defer d.mu.Unlock()
	const limit = 8192
	if len(p) >= limit {
		d.data = append(d.data[:0], p[len(p)-limit:]...)
	} else {
		if excess := len(d.data) + len(p) - limit; excess > 0 {
			d.data = append(d.data[:0], d.data[excess:]...)
		}
		d.data = append(d.data, p...)
	}
	return n, nil
}
func (d *nativeDiagnosticTail) classification() string {
	if d == nil {
		return "unavailable"
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	text := strings.ToLower(string(d.data))
	for _, row := range [][2]string{{"panicked at", "native-panic"}, {"out of memory", "memory-error"}, {"failed to deserialize", "decode-error"}, {"error loading config", "config-error"}, {"permission denied", "permission-error"}, {"no such file or directory", "missing-file"}, {"database is locked", "database-lock"}} {
		if strings.Contains(text, row[0]) {
			return row[1]
		}
	}
	if len(d.data) == 0 {
		return "empty"
	}
	return "unclassified"
}
func (p *pitputProcess) diagnosticSummary() string {
	state := "not-observed-exited"
	select {
	case err := <-p.done:
		p.done <- err
		if err == nil {
			state = "exited-0"
		} else if exit, ok := err.(*exec.ExitError); ok {
			state = fmt.Sprintf("exited-%d", exit.ExitCode())
		} else {
			state = "exit-error"
		}
	default:
	}
	return "native_process=" + state + " stderr_class=" + p.diagnostic.classification()
}
