package backend

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Reservation persists the controller's idempotency key before any remote effect.
// It deliberately contains no credentials, prompts or native transcript content.
type Reservation struct {
	Container    *ContainerSpec `json:"container,omitempty"`
	Schema       int            `json:"schema"`
	Name         string         `json:"name"`
	Host         string         `json:"host"`
	Fingerprint  string         `json:"fingerprint"`
	DeploymentID string         `json:"deployment_id"`
	CreatedAt    time.Time      `json:"created_at"`
}

type StateStore struct{ Directory string }

// Reserve returns the same deployment ID after controller restarts or concurrent
// invocations. A changed host or desired launch must use an explicit new name.
func (s StateStore) Reserve(name, host, fingerprint string) (Reservation, error) {
	return s.reserve(name, host, fingerprint, nil)
}

func (s StateStore) ReserveConfigured(name, host, fingerprint string, spec ContainerSpec) (Reservation, error) {
	if err := spec.Validate(); err != nil {
		return Reservation{}, err
	}
	if spec.LaunchDigest != fingerprint {
		return Reservation{}, errors.New("container launch digest mismatch")
	}
	return s.reserve(name, host, fingerprint, &spec)
}

func (s StateStore) reserve(name, host, fingerprint string, spec *ContainerSpec) (Reservation, error) {
	var result Reservation
	digest, err := hex.DecodeString(fingerprint)
	if name == "" || len(name) > 240 || strings.ContainsAny(name, "\x00\r\n") || !hostPattern.MatchString(host) || err != nil || len(digest) != 32 || strings.ToLower(fingerprint) != fingerprint {
		return result, errors.New("invalid deployment reservation")
	}
	if !filepath.IsAbs(s.Directory) {
		return result, errors.New("deployment state directory must be absolute")
	}
	if err = os.MkdirAll(s.Directory, 0700); err != nil {
		return result, err
	}
	info, err := os.Lstat(s.Directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return result, errors.New("deployment state directory must be private and not a symlink")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return result, errors.New("deployment state directory must be owned by the controller")
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
	lock, err := os.OpenFile(filepath.Join(s.Directory, key+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return result, errors.New("cannot acquire deployment reservation lock")
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return result, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	path := filepath.Join(s.Directory, key+".json")
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err == nil {
		defer f.Close()
		info, err = f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 || info.Mode().Perm()&0077 != 0 {
			return result, errors.New("invalid deployment reservation file")
		}
		fileStat, owned := info.Sys().(*syscall.Stat_t)
		if !owned || fileStat.Uid != uint32(os.Geteuid()) {
			return Reservation{}, errors.New("reservation file is not owned by the controller")
		}
		decoder := json.NewDecoder(f)
		decoder.DisallowUnknownFields()
		if decoder.Decode(&result) != nil || result.Schema != 1 || result.Name != name || result.CreatedAt.IsZero() {
			return Reservation{}, errors.New("invalid deployment reservation; refusing replacement")
		}
		if decoder.Decode(new(any)) != io.EOF {
			return Reservation{}, errors.New("trailing data in deployment reservation")
		}
		id, e := hex.DecodeString(result.DeploymentID)
		if e != nil || len(id) != 16 {
			return Reservation{}, errors.New("invalid saved deployment ID")
		}
		if result.Host != host || result.Fingerprint != fingerprint {
			return Reservation{}, errors.New("deployment name already binds a different host or desired configuration")
		}
		if spec != nil {
			a, _ := json.Marshal(spec)
			b, _ := json.Marshal(result.Container)
			if string(a) != string(b) {
				return Reservation{}, errors.New("saved container specification differs or is missing")
			}
		}
		return result, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return result, errors.New("cannot read deployment reservation; refusing replacement")
	}
	id := make([]byte, 16)
	if _, err = rand.Read(id); err != nil {
		return result, err
	}
	result = Reservation{Container: spec, Schema: 1, Name: name, Host: host, Fingerprint: fingerprint, DeploymentID: hex.EncodeToString(id), CreatedAt: time.Now().UTC()}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return Reservation{}, err
	}
	temp, err := os.CreateTemp(s.Directory, ".reservation-")
	if err != nil {
		return Reservation{}, err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(append(data, '\n')); err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return Reservation{}, err
	}
	if err = os.Rename(temp.Name(), path); err != nil {
		return Reservation{}, err
	}
	directory, err := os.Open(s.Directory)
	if err != nil {
		return Reservation{}, err
	}
	defer directory.Close()
	if err = directory.Sync(); err != nil {
		return Reservation{}, err
	}
	return result, nil
}

// Lookup observes an existing reservation without allocating a new identity.
func (s StateStore) Lookup(name string) (Reservation, error) {
	if !filepath.IsAbs(s.Directory) || name == "" || len(name) > 240 || strings.ContainsAny(name, "\x00\r\n") {
		return Reservation{}, errors.New("invalid deployment lookup")
	}
	info, err := os.Lstat(s.Directory)
	if err != nil {
		return Reservation{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return Reservation{}, errors.New("invalid deployment state directory")
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
	f, err := os.OpenFile(filepath.Join(s.Directory, key+".json"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Reservation{}, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return Reservation{}, err
	}
	stat, ok = info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Size() > 16384 || info.Mode().Perm()&0077 != 0 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return Reservation{}, errors.New("invalid reservation file")
	}
	var res Reservation
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&res) != nil || decoder.Decode(new(any)) != io.EOF || res.Schema != 1 || res.Name != name || res.CreatedAt.IsZero() {
		return Reservation{}, errors.New("invalid saved reservation")
	}
	id, e := hex.DecodeString(res.DeploymentID)
	digest, de := hex.DecodeString(res.Fingerprint)
	if e != nil || len(id) != 16 || strings.ToLower(res.DeploymentID) != res.DeploymentID || de != nil || len(digest) != 32 || strings.ToLower(res.Fingerprint) != res.Fingerprint || !hostPattern.MatchString(res.Host) {
		return Reservation{}, errors.New("invalid saved deployment binding")
	}
	if res.Container == nil || res.Container.Validate() != nil || res.Container.LaunchDigest != res.Fingerprint {
		return Reservation{}, errors.New("reservation lacks a valid saved container specification")
	}
	return res, nil
}
