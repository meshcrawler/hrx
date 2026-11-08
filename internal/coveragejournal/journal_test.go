package coveragejournal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func sample() Stage {
	return Stage{Epoch: "epoch", Connection: "connection", Generation: "generation", Revision: 1, Operation: "operation", Prefix: 7, Payload: []byte("historical evidence")}
}

func TestCodecRejectsEveryTornFrameAndCorruption(t *testing.T) {
	f, _, err := encode(sample(), 1, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(f); i++ {
		if _, _, err := decode(f[:i]); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("truncation %d: %v", i, err)
		}
	}
	for i := range f {
		bad := bytes.Clone(f)
		bad[i] ^= 1
		if _, _, err := decode(bad); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("corruption %d: %v", i, err)
		}
	}
}

func TestCodecRejectsReorderDuplicateAndCrossJournalSplice(t *testing.T) {
	s := sample()
	a, hash, _ := encode(s, 1, [32]byte{})
	s.Operation = "second"
	b, _, _ := encode(s, 2, hash)
	if out, _, err := decode(append(bytes.Clone(a), b...)); err != nil || len(out) != 2 {
		t.Fatal(out, err)
	}
	dup, _, _ := encode(sample(), 2, hash)
	wrong, _, _ := encode(s, 2, [32]byte{})
	for _, data := range [][]byte{b, append(bytes.Clone(a), a...), append(bytes.Clone(a), dup...), append(bytes.Clone(a), wrong...)} {
		if _, _, err := decode(data); !errors.Is(err, ErrCorrupt) {
			t.Fatal(err)
		}
	}
}

type memoryFile struct {
	data              []byte
	writeErr, syncErr error
	short             bool
	syncHook          func()
	closeErr          error
	closeCalls        int
}

func (f *memoryFile) WriteAt(b []byte, offset int64) (int, error) {
	if offset != int64(len(f.data)) {
		return 0, errors.New("unexpected offset")
	}
	n := len(b)
	if f.short {
		n /= 2
	}
	f.data = append(f.data, b[:n]...)
	return n, f.writeErr
}
func (f *memoryFile) Sync() error {
	if f.syncHook != nil {
		f.syncHook()
	}
	return f.syncErr
}
func (f *memoryFile) Close() error {
	f.closeCalls++
	return f.closeErr
}
func inMemory(f *memoryFile) *Journal { return &Journal{file: f, lock: f, seen: map[operation]bool{}} }

func TestMemoryCloseFailureRemainsVisibleWithoutRetry(t *testing.T) {
	fileErr, lockErr := errors.New("file close failed"), errors.New("lock close failed")
	f, lock := &memoryFile{closeErr: fileErr}, &memoryFile{closeErr: lockErr}
	j := inMemory(f)
	j.lock = lock
	for i := 0; i < 2; i++ {
		err := j.Close()
		if !errors.Is(err, fileErr) || !errors.Is(err, lockErr) {
			t.Fatalf("close %d lost cleanup evidence: %v", i, err)
		}
	}
	if f.closeCalls != 1 || lock.closeCalls != 1 {
		t.Fatal("uncertain descriptor close was retried")
	}
	if d, err := j.Append(context.Background(), sample()); d != Absent || !errors.Is(err, ErrClosed) || len(f.data) != 0 {
		t.Fatal("failed cleanup reopened journal authority", d, err)
	}
}

func TestMemoryIndeterminateWritesPoisonOwner(t *testing.T) {
	for _, kind := range []string{"write", "short", "sync", "canceled-after-sync"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := &memoryFile{}
			switch kind {
			case "write":
				f.writeErr = io.ErrUnexpectedEOF
			case "short":
				f.short = true
			case "sync":
				f.syncErr = io.ErrClosedPipe
			default:
				f.syncHook = cancel
			}
			j := inMemory(f)
			d, err := j.Append(ctx, sample())
			if d != Indeterminate || err == nil || len(j.History()) != 0 {
				t.Fatal(d, err)
			}
			before := len(f.data)
			d, err = j.Append(context.Background(), sample())
			if d != Absent || !errors.Is(err, ErrClosed) || len(f.data) != before {
				t.Fatal(d, err)
			}
		})
	}
}

func TestMemoryPrewriteRefusalAndHistoricalCopies(t *testing.T) {
	f := &memoryFile{}
	j := inMemory(f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if d, err := j.Append(ctx, sample()); d != Absent || !errors.Is(err, context.Canceled) || len(f.data) != 0 {
		t.Fatal(d, err)
	}
	s := sample()
	if d, err := j.Append(context.Background(), s); d != Durable || err != nil {
		t.Fatal(d, err)
	}
	s.Payload[0] = 'X'
	h := j.History()
	h[0].Payload[0] = 'Y'
	if string(j.History()[0].Payload) != "historical evidence" {
		t.Fatal("mutable history")
	}
	before := len(f.data)
	if d, err := j.Append(context.Background(), sample()); d != Absent || !errors.Is(err, ErrDuplicate) || len(f.data) != before {
		t.Fatal(d, err)
	}
	s = sample()
	s.Revision++
	if d, err := j.Append(context.Background(), s); d != Durable || err != nil {
		t.Fatal(d, err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append(context.Background(), sample()); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestMemoryBoundsRefuseBeforeWrite(t *testing.T) {
	for _, kind := range []string{"payload", "identity", "revision", "bytes", "records"} {
		t.Run(kind, func(t *testing.T) {
			f := &memoryFile{}
			j, s := inMemory(f), sample()
			want := ErrInvalid
			switch kind {
			case "payload":
				s.Payload = make([]byte, MaxPayload+1)
			case "identity":
				s.Connection = "../other"
			case "revision":
				s.Revision = 0
			case "bytes":
				j.size = maxJournal
				want = ErrLimit
			case "records":
				j.stages = make([]Stage, maxRecords)
				want = ErrLimit
			}
			if d, err := j.Append(context.Background(), s); d != Absent || !errors.Is(err, want) || len(f.data) != 0 {
				t.Fatal(d, err)
			}
		})
	}
}

// Filesystem/process tests run on GitLab, never as local monitoring fixtures.
func TestDiskReopenAndExclusiveOwnership(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	j, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Open(dir); !errors.Is(err, ErrOwned) {
		if other != nil {
			other.Close()
		}
		t.Fatal(err)
	}
	if d, err := j.Append(context.Background(), sample()); d != Durable || err != nil {
		t.Fatal(d, err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if h := j.History(); len(h) != 1 || h[0].Prefix != 7 {
		t.Fatal(h)
	}
	if d, err := j.Append(context.Background(), sample()); d != Absent || !errors.Is(err, ErrDuplicate) {
		t.Fatal(d, err)
	}
}

func TestDiskRefusesTornHistoryWithoutRepair(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	f, _, _ := encode(sample(), 1, [32]byte{})
	path := filepath.Join(dir, "stages.log")
	for _, data := range [][]byte{f[:len(f)-1], append(bytes.Clone(f), 0)} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if j, err := Open(dir); !errors.Is(err, ErrCorrupt) {
			if j != nil {
				j.Close()
			}
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatal("history mutated", err)
		}
	}
}

func TestDiskRejectsUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "public"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			os.Chmod(dir, 0700)
			target := filepath.Join(dir, "stages.log")
			other := filepath.Join(dir, "other")
			if err := os.WriteFile(other, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(other, target)
			case "hardlink":
				err = os.Link(other, target)
			case "public":
				err = os.WriteFile(target, nil, 0644)
			}
			if err != nil {
				t.Fatal(err)
			}
			if j, err := Open(dir); err == nil {
				j.Close()
				t.Fatal("unsafe file accepted")
			}
			got, err := os.ReadFile(other)
			if err != nil || string(got) != "untouched" {
				t.Fatal("target modified", err)
			}
		})
	}
}

func TestDiskCrashAfterDurableStage(t *testing.T) {
	if dir := os.Getenv("HRX_JOURNAL_CRASH_DIR"); dir != "" {
		j, err := Open(dir)
		if err != nil {
			os.Exit(10)
		}
		if d, err := j.Append(context.Background(), sample()); d != Durable || err != nil {
			os.Exit(11)
		}
		os.Exit(23) // Deliberately bypass Close and all defers.
	}
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	cmd := exec.Command(os.Args[0], "-test.run=^TestDiskCrashAfterDurableStage$")
	cmd.Env = append(os.Environ(), "HRX_JOURNAL_CRASH_DIR="+dir)
	if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != 23 {
		t.Fatal("child failed", err)
	}
	j, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if len(j.History()) != 1 {
		t.Fatal("durable stage missing")
	}
}
