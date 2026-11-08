// Package coveragejournal stores historical staging evidence, never current
// coverage authority. It is dormant: no production admission path calls it.
package coveragejournal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	MaxPayload = 64 << 10
	maxFrame   = 128 << 10
	maxJournal = 16 << 20
	maxRecords = 1024
)

var (
	ErrInvalid   = errors.New("invalid coverage stage")
	ErrCorrupt   = errors.New("coverage journal corrupt or truncated")
	ErrOwned     = errors.New("coverage journal already owned")
	ErrPrivate   = errors.New("coverage journal requires private owned regular files")
	ErrClosed    = errors.New("coverage journal closed or indeterminate")
	ErrLimit     = errors.New("coverage journal capacity reached")
	ErrDuplicate = errors.New("coverage stage operation already exists")
)

// Durability classifies storage only. Even Durable must never be interpreted as
// publication, baseline admission or permission to ACK/skip/execute anything.
type Durability uint8

const (
	Absent Durability = iota
	Durable
	Indeterminate
)

// Stage identity is revision-scoped. Payload contains bounded historical
// evidence supplied by the controller, which owns any necessary redaction.
type Stage struct {
	Epoch      string `json:"epoch"`
	Connection string `json:"connection"`
	Generation string `json:"generation"`
	Revision   uint64 `json:"revision"`
	Operation  string `json:"operation"`
	Prefix     uint64 `json:"prefix"`
	Payload    []byte `json:"payload"`
}

type record struct {
	Version  int    `json:"version"`
	Ordinal  uint64 `json:"ordinal"`
	Previous string `json:"previous"`
	Stage    Stage  `json:"stage"`
}

type operation struct {
	epoch, connection, generation string
	revision                      uint64
	id                            string
}

func (s Stage) key() operation {
	return operation{s.Epoch, s.Connection, s.Generation, s.Revision, s.Operation}
}

func validID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}
func valid(s Stage) bool {
	return validID(s.Epoch) && validID(s.Connection) && validID(s.Generation) && validID(s.Operation) && s.Revision != 0 && len(s.Payload) <= MaxPayload
}

// Journal serializes its own filesystem operations, never the transport reader.
// The controller must use a separate writer and continue bounded event draining.
// Close waits for an in-flight write; cancellation cannot interrupt fsync.
type Journal struct {
	mu               sync.Mutex
	file             stageFile
	lock             io.Closer
	stages           []Stage
	seen             map[operation]bool
	last             [32]byte
	size             int64
	closed, poisoned bool
	closeErr         error
}

type stageFile interface {
	WriteAt([]byte, int64) (int, error)
	Sync() error
	Close() error
}

// Open requires an already provisioned private directory on a local filesystem.
// It uses descriptor-relative opens and holds a nonblocking exclusive lock until
// Close. Lock files are never unlinked. It refuses corruption instead of silently
// truncating history. All recovered stages are historical; there is no API for
// recovering a current publication or automatically retrying a stage.
func Open(path string) (*Journal, error) {
	dir, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Geteuid()) {
		return nil, ErrPrivate
	}
	open := func(name string) (*os.File, error) {
		fd, e := unix.Openat(int(dir.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
		if e != nil {
			return nil, e
		}
		f := os.NewFile(uintptr(fd), name)
		i, e := f.Stat()
		if e == nil {
			s, ok := i.Sys().(*syscall.Stat_t)
			if !ok || !i.Mode().IsRegular() || i.Mode().Perm()&0077 != 0 || s.Uid != uint32(os.Geteuid()) || s.Nlink != 1 {
				e = ErrPrivate
			}
		}
		if e != nil {
			f.Close()
			return nil, e
		}
		return f, nil
	}
	lock, err := open("owner.lock")
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, ErrOwned
	}
	f, err := open("stages.log")
	if err != nil {
		lock.Close()
		return nil, err
	}
	fail := func(e error) (*Journal, error) { f.Close(); lock.Close(); return nil, e }
	info, err = f.Stat()
	if err != nil {
		return fail(err)
	}
	if info.Size() > maxJournal {
		return fail(ErrLimit)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxJournal+1))
	if err != nil {
		return fail(err)
	}
	if len(data) > maxJournal {
		return fail(ErrLimit)
	}
	stages, last, err := decode(data)
	if err != nil {
		return fail(err)
	}
	// Sync the created file and its directory entry before accepting any stage.
	if err = f.Sync(); err != nil {
		return fail(err)
	}
	if err = dir.Sync(); err != nil {
		return fail(err)
	}
	j := &Journal{file: f, lock: lock, stages: stages, last: last, size: int64(len(data)), seen: map[operation]bool{}}
	for _, s := range stages {
		j.seen[s.key()] = true
	}
	return j, nil
}

func encode(s Stage, ordinal uint64, previous [32]byte) ([]byte, [32]byte, error) {
	if !valid(s) {
		return nil, [32]byte{}, ErrInvalid
	}
	b, err := json.Marshal(record{1, ordinal, hex.EncodeToString(previous[:]), s})
	if err != nil || len(b) > maxFrame {
		return nil, [32]byte{}, ErrInvalid
	}
	hash := sha256.Sum256(b)
	frame := make([]byte, 4+len(b)+len(hash))
	binary.BigEndian.PutUint32(frame, uint32(len(b)))
	copy(frame[4:], b)
	copy(frame[4+len(b):], hash[:])
	return frame, hash, nil
}

func decode(data []byte) ([]Stage, [32]byte, error) {
	var stages []Stage
	var last [32]byte
	seen := map[operation]bool{}
	for len(data) > 0 {
		if len(stages) >= maxRecords || len(data) < 4 {
			return nil, last, ErrCorrupt
		}
		n := int(binary.BigEndian.Uint32(data))
		if n <= 0 || n > maxFrame || len(data) < 4+n+32 {
			return nil, last, ErrCorrupt
		}
		body := data[4 : 4+n]
		digest := sha256.Sum256(body)
		if !bytes.Equal(digest[:], data[4+n:4+n+32]) {
			return nil, last, ErrCorrupt
		}
		var r record
		d := json.NewDecoder(bytes.NewReader(body))
		d.DisallowUnknownFields()
		if d.Decode(&r) != nil || r.Version != 1 || r.Ordinal != uint64(len(stages)+1) || r.Previous != hex.EncodeToString(last[:]) || !valid(r.Stage) || seen[r.Stage.key()] {
			return nil, last, ErrCorrupt
		}
		// Require the exact canonical representation produced by this version.
		canonical, _ := json.Marshal(r)
		if !bytes.Equal(canonical, body) {
			return nil, last, ErrCorrupt
		}
		stages = append(stages, r.Stage)
		seen[r.Stage.key()] = true
		last = digest
		data = data[4+n+32:]
	}
	return stages, last, nil
}

// Append returns Absent only when this call attempted no write; an existing
// duplicate may still be present in history. A write/sync
// error or cancellation after write initiation poisons this owner and returns
// Indeterminate. A late disk completion cannot be retried on this instance.
// The caller must not mutate s.Payload concurrently with this call.
func (j *Journal) Append(ctx context.Context, s Stage) (Durability, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.poisoned {
		return Absent, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return Absent, err
	}
	if !valid(s) {
		return Absent, ErrInvalid
	}
	if j.seen[s.key()] {
		return Absent, ErrDuplicate
	}
	frame, hash, err := encode(s, uint64(len(j.stages)+1), j.last)
	if err != nil {
		return Absent, err
	}
	if len(j.stages) >= maxRecords || j.size+int64(len(frame)) > maxJournal {
		return Absent, ErrLimit
	}
	s.Payload = bytes.Clone(s.Payload)
	if err = ctx.Err(); err != nil {
		return Absent, err
	}
	n, err := j.file.WriteAt(frame, j.size)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = j.file.Sync()
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		j.poisoned = true
		return Indeterminate, err
	}
	j.size += int64(len(frame))
	j.last = hash
	j.stages = append(j.stages, s)
	j.seen[s.key()] = true
	return Durable, nil
}

// History returns independent copies, never current coverage authority.
func (j *Journal) History() []Stage {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := append([]Stage(nil), j.stages...)
	for i := range out {
		out[i].Payload = bytes.Clone(out[i].Payload)
	}
	return out
}

func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return j.closeErr
	}
	j.closed = true
	// Never retry an uncertain descriptor close or turn it into later success.
	j.closeErr = errors.Join(j.file.Close(), j.lock.Close())
	return j.closeErr
}
