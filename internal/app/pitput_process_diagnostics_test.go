package app

import (
	"strings"
	"testing"
)

func TestNativeProcessDiagnosticIsBoundedAndNeverPublishesStderr(t *testing.T) {
	d := &nativeDiagnosticTail{}
	input := strings.Repeat("secret-data", 2000) + " panicked at private-path"
	if n, err := d.Write([]byte(input)); n != len(input) || err != nil {
		t.Fatal(n, err)
	}
	if len(d.data) > 8192 || d.classification() != "native-panic" {
		t.Fatal("invalid bounded classification")
	}
	p := &pitputProcess{diagnostic: d, done: make(chan error, 1)}
	summary := p.diagnosticSummary()
	if strings.Contains(summary, "secret") || strings.Contains(summary, "private") {
		t.Fatal("raw diagnostic leaked")
	}
	p.done <- nil
	if !strings.Contains(p.diagnosticSummary(), "exited-0") {
		t.Fatal("missing exit status")
	}
	if <-p.done != nil {
		t.Fatal("consumed process outcome")
	}
}
