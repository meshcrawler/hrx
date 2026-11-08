package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixture(id string) Record {
	return Record{NativeID: id, Harness: "codex", Identity: "model", Route: "provider/model", Project: "/project", Reasoning: "high", Mail: true, BBS: true, Agent: "host/hrx/name"}
}

func TestSaveAndStartRollsBackFreshAndResumedSessions(t *testing.T) {
	for _, existing := range []bool{false, true} {
		s := Store{Dir: filepath.Join(t.TempDir(), "sessions")}
		if existing {
			if err := s.Save(fixture("target")); err != nil {
				t.Fatal(err)
			}
		}
		before, beforeErr := s.Resolve("codex", "/project", "target")
		changed := fixture("target")
		changed.Route = "new/route"
		startErr := errors.New("fixture startup failed")
		if err := s.SaveAndStart(changed, func() error { return startErr }); !errors.Is(err, startErr) {
			t.Fatal("startup failure lost", err)
		}
		after, afterErr := s.Resolve("codex", "/project", "target")
		if !reflect.DeepEqual(before, after) || !errors.Is(afterErr, beforeErr) {
			t.Fatal("failed startup changed session metadata")
		}
		if err := s.SaveAndStart(changed, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		after, err := s.Resolve("codex", "/project", "target")
		if err != nil || after.Route != "new/route" {
			t.Fatal("successful startup not indexed", err)
		}
	}
}

func TestSaveAndStartSerializesRollbackWithConcurrentWriters(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "sessions")}
	entered, finish := make(chan struct{}), make(chan struct{})
	started := make(chan error, 1)
	go func() {
		started <- s.SaveAndStart(fixture("failed"), func() error {
			close(entered)
			<-finish
			return errors.New("startup failed")
		})
	}()
	<-entered
	written := make(chan error, 1)
	go func() { written <- s.Save(fixture("concurrent")) }()
	read := make(chan error, 1)
	go func() { _, err := s.Resolve("codex", "/project", "failed"); read <- err }()
	select {
	case <-written:
		close(finish)
		t.Fatal("writer bypassed launch transaction")
	case <-read:
		close(finish)
		t.Fatal("reader observed uncommitted launch")
	case <-time.After(30 * time.Millisecond):
	}
	close(finish)
	if <-started == nil || <-written != nil || !errors.Is(<-read, ErrNotFound) {
		t.Fatal("transaction outcome incorrect")
	}
	if _, err := s.Resolve("codex", "/project", "concurrent"); err != nil {
		t.Fatal("rollback lost concurrent writer", err)
	}
}

func TestSaveAndStartRefusesInvalidIndexBeforeStarting(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	path := filepath.Join(s.Dir, "index.json")
	if err := os.WriteFile(path, []byte("invalid index"), 0600); err != nil {
		t.Fatal(err)
	}
	started := false
	err := s.SaveAndStart(fixture("target"), func() error { started = true; return nil })
	data, readErr := os.ReadFile(path)
	if err == nil || started || readErr != nil || string(data) != "invalid index" {
		t.Fatal("invalid index launched a process or was overwritten")
	}
}

func TestSaveResolveAndProjectIsolation(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "sessions")}
	if _, err := s.Resolve("codex", "/project", ""); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Fatal("missing read created index directory")
	}
	for _, id := range []string{"first", "second"} {
		if err := s.Save(fixture(id)); err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.Resolve("codex", "/project", "")
	if err != nil || r.NativeID != "second" {
		t.Fatalf("latest: %+v %v", r, err)
	}
	r, err = s.Resolve("codex", "/project", "first")
	if err != nil || r.Agent != "host/hrx/name" || r.Reasoning != "high" {
		t.Fatalf("exact: %+v %v", r, err)
	}
	created := r.CreatedAt
	r.Route = "new/model"
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	r, err = s.Resolve("codex", "/project", "")
	if err != nil || r.NativeID != "first" || !r.CreatedAt.Equal(created) {
		t.Fatalf("updated: %+v %v", r, err)
	}
	for _, query := range [][2]string{{"claude", "/project"}, {"codex", "/elsewhere"}} {
		if _, err := s.Resolve(query[0], query[1], "first"); !errors.Is(err, ErrNotFound) {
			t.Fatal("cross-project/harness leak", err)
		}
	}
	r.Project = "/elsewhere"
	if err := s.Save(r); err == nil {
		t.Fatal("session reassigned to another project")
	}
	for _, name := range []string{"index.json", "index.lock"} {
		info, err := os.Stat(filepath.Join(s.Dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("nonprivate index file", name, err)
		}
	}
}

func TestConcurrentWritersKeepEverySession(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "sessions")}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Save(fixture(fmt.Sprintf("session-%d", i))); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	for i := range 20 {
		if _, err := s.Resolve("codex", "/project", fmt.Sprintf("session-%d", i)); err != nil {
			t.Fatal("lost writer", i, err)
		}
	}
}

func TestBadIndexIsNeverOverwritten(t *testing.T) {
	for _, content := range []string{"{", `{"version":2,"sessions":[]}`, `{"version":1,"sessions":[],"token":"secret"}`} {
		s := Store{Dir: t.TempDir()}
		path := filepath.Join(s.Dir, "index.json")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(fixture("new")); err == nil {
			t.Fatal("corrupt index accepted")
		}
		got, _ := os.ReadFile(path)
		if string(got) != content {
			t.Fatal("corrupt index overwritten")
		}
	}
}

func TestIndexRejectsSymlinksAndPublicFiles(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.Dir, "index.lock")); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(fixture("new")); err == nil {
		t.Fatal("followed lock symlink")
	}
	s = Store{Dir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(s.Dir, "index.json"), []byte(`{"version":1,"sessions":[]}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(fixture("new")); err == nil {
		t.Fatal("accepted public metadata")
	}
}

func TestMetadataValidationAndSecretFreeSchema(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "sessions")}
	for _, id := range []string{"", "../escape", "bad\nvalue", strings.Repeat("a", 257)} {
		if err := s.Save(fixture(id)); err == nil {
			t.Fatal("invalid native ID accepted")
		}
	}
	r := fixture("valid")
	r.Agent = ""
	if err := s.Save(r); err == nil {
		t.Fatal("communications without identity accepted")
	}
	typeOf := reflect.TypeOf(Record{})
	for i := range typeOf.NumField() {
		name := strings.ToLower(typeOf.Field(i).Name)
		for _, forbidden := range []string{"token", "key", "transcript", "prompt", "yolo", "environment"} {
			if strings.Contains(name, forbidden) {
				t.Fatalf("unsafe metadata field %s", name)
			}
		}
	}
}
