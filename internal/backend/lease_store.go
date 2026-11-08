package backend

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

var ErrLeaseExpired = errors.New("deployment credential lease expired; stop the owned session before explicit replacement")

type leaseRecord struct {
	Generation  int             `json:"generation,omitempty"`
	Request     LeaseRequest    `json:"request"`
	Schema      int             `json:"schema"`
	Fingerprint string          `json:"request_fingerprint"`
	Lease       CredentialLease `json:"lease"`
	Key         string          `json:"key"`
	OAuth       string          `json:"oauth,omitempty"`
	Revoked     bool            `json:"revoked"`
}

// AcquireLease stores a scoped grant only in controller-owned private state. Its
// request ID derives from the already durable deployment reservation, so a crash
// between broker acceptance and local persistence does not require a new mint.
// Expiry never silently restarts/rekeys a running worker.
func (s StateStore) AcquireLease(ctx context.Context, res Reservation, request LeaseRequest, broker LeaseBroker) (CredentialLease, error) {
	var result CredentialLease
	err := s.withLease(res, func(path string) error {
		saved, readErr := readLeaseRecord(path)
		generation := 0
		if readErr == nil {
			generation = saved.Generation
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		request.RequestID = leaseRequestID(res.DeploymentID, generation)
		payload, err := json.Marshal(request)
		if err != nil {
			return err
		}
		fingerprint := fmt.Sprintf("%x", sha256.Sum256(payload))
		saved, err = readLeaseRecord(path)
		if err == nil {
			if saved.Fingerprint != fingerprint {
				return errors.New("saved credential lease binds a different request")
			}
			if saved.Revoked {
				return errors.New("deployment credential lease was revoked; explicit replacement required")
			}
			if saved.Key != "" {
				if !saved.Lease.ExpiresAt.After(time.Now().Add(15 * time.Second)) {
					return ErrLeaseExpired
				}
				if !uuidPattern.MatchString(saved.Lease.ID) {
					return errors.New("invalid stored credential lease")
				}
				result = saved.Lease
				result.key = saved.Key
				result.oauth = saved.OAuth
				return nil
			}
			if saved.Lease.ID != "" {
				return errors.New("unusable grant retained; revoke it before replacement")
			}
		}

		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = writeLeaseRecord(path, leaseRecord{Generation: generation, Schema: 1, Fingerprint: fingerprint, Request: request}); err != nil {
			return err
		}
		result, err = broker.Mint(ctx, request)
		if err != nil {
			if result.ID != "" {
				if savedErr := writeLeaseRecord(path, leaseRecord{Generation: generation, Schema: 1, Fingerprint: fingerprint, Request: request, Lease: result}); savedErr != nil {
					return savedErr
				}
			}
			return err
		}
		return writeLeaseRecord(path, leaseRecord{Generation: generation, Schema: 1, Fingerprint: fingerprint, Request: request, Lease: result, Key: result.key, OAuth: result.oauth})
	})
	return result, err
}

// RevokeLease retains a credential-free tombstone after confirmed revocation;
// retries cannot accidentally re-mint an already torn-down deployment's grant.
func (s StateStore) RevokeLease(ctx context.Context, res Reservation, broker LeaseBroker) error {
	return s.withLease(res, func(path string) error {
		saved, err := readLeaseRecord(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if saved.Revoked {
			return nil
		}
		if saved.Lease.ID == "" {
			request := saved.Request
			request.RequestID = leaseRequestID(res.DeploymentID, saved.Generation)
			recovered, recoveryErr := broker.Mint(ctx, request)
			if recovered.ID == "" {
				return recoveryErr
			}
			saved.Lease = recovered
			if err = writeLeaseRecord(path, saved); err != nil {
				return err
			}
		}
		if err = broker.Revoke(ctx, saved.Lease.ID); err != nil {
			return err
		}
		saved.Key = ""
		saved.OAuth = ""
		saved.Revoked = true
		return writeLeaseRecord(path, saved)
	})
}

func (s StateStore) withLease(res Reservation, fn func(string) error) error {
	verified, err := s.Reserve(res.Name, res.Host, res.Fingerprint)
	if err != nil {
		return err
	}
	if verified.DeploymentID != res.DeploymentID {
		return errors.New("credential operation does not match durable deployment reservation")
	}
	lock, err := os.OpenFile(filepath.Join(s.Directory, res.DeploymentID+".lease.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("cannot lock deployment credential state")
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn(filepath.Join(s.Directory, res.DeploymentID+".lease.json"))
}

func readLeaseRecord(path string) (leaseRecord, error) {
	var value leaseRecord
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return value, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 65536 {
		return value, errors.New("credential state must be a private regular file")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) {
		return value, errors.New("credential state owner mismatch")
	}
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || value.Schema != 1 || value.Generation < 0 || value.Generation > 1000000 || decoder.Decode(new(any)) != io.EOF {
		return leaseRecord{}, errors.New("invalid credential state; refusing replacement")
	}
	return value, nil
}

func writeLeaseRecord(path string, value leaseRecord) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".lease-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// leaseRequestID keeps the initial protocol stable and gives replacements a
// deterministic, distinct broker idempotency key.
func leaseRequestID(id string, generation int) string {
	if generation > 0 {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s:lease:%d", id, generation)))
		id = fmt.Sprintf("%x", sum[:16])
	}
	return id[:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
}

// ReplaceLease is called only after stopping the owned native process. Explicit
// generation makes retrying an interrupted replacement reuse the same grant.
// The previous grant is confirmed revoked before a new mint intent is recorded.
func (s StateStore) ReplaceLease(ctx context.Context, res Reservation, generation int, broker LeaseBroker) (CredentialLease, error) {
	var result CredentialLease
	if generation < 1 || generation > 1000000 {
		return result, errors.New("lease generation must be between 1 and 1000000")
	}
	err := s.withLease(res, func(path string) error {
		saved, err := readLeaseRecord(path)
		if err != nil {
			return err
		}
		if generation < saved.Generation || generation > saved.Generation+1 {
			return errors.New("replacement must use the current or next lease generation")
		}
		if generation == saved.Generation && saved.Revoked {
			return errors.New("requested lease generation was revoked")
		}
		if generation > saved.Generation {
			if !saved.Revoked {
				if saved.Lease.ID == "" {
					request := saved.Request
					request.RequestID = leaseRequestID(res.DeploymentID, saved.Generation)
					recovered, recoveryErr := broker.Mint(ctx, request)
					if recovered.ID == "" {
						return recoveryErr
					}
					saved.Lease = recovered
					if err = writeLeaseRecord(path, saved); err != nil {
						return err
					}
				}
				if err = broker.Revoke(ctx, saved.Lease.ID); err != nil {
					return err
				}
				saved.Key = ""
				saved.OAuth = ""
				saved.Revoked = true
				if err = writeLeaseRecord(path, saved); err != nil {
					return err
				}
			}
			request := saved.Request
			request.RequestID = leaseRequestID(res.DeploymentID, generation)
			payload, err := json.Marshal(request)
			if err != nil {
				return err
			}
			saved = leaseRecord{Schema: 1, Generation: generation, Request: request, Fingerprint: fmt.Sprintf("%x", sha256.Sum256(payload))}
			if err = writeLeaseRecord(path, saved); err != nil {
				return err
			}
		}
		if saved.Key != "" {
			if !saved.Lease.ExpiresAt.After(time.Now().Add(30 * time.Second)) {
				return ErrLeaseExpired
			}
			result = saved.Lease
			result.key = saved.Key
			result.oauth = saved.OAuth
			return nil
		}
		if saved.Lease.ID != "" {
			return errors.New("unusable replacement retained; revoke before next generation")
		}
		request := saved.Request
		request.RequestID = leaseRequestID(res.DeploymentID, generation)
		payload, marshalErr := json.Marshal(request)
		if marshalErr != nil || saved.Fingerprint != fmt.Sprintf("%x", sha256.Sum256(payload)) {
			return errors.New("replacement request binding mismatch")
		}
		result, err = broker.Mint(ctx, request)
		if result.ID != "" {
			saved.Lease = result
			if err == nil {
				saved.Key = result.key
				saved.OAuth = result.oauth
			}
			if saveErr := writeLeaseRecord(path, saved); saveErr != nil {
				return saveErr
			}
		}
		return err
	})
	return result, err
}
