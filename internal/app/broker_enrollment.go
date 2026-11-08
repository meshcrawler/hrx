package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func readBrokerSecret(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", errors.New("cannot read private broker credential")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return "", errors.New("broker credential must be a private regular file (0600)")
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	secret := strings.TrimSpace(string(b))
	if err != nil || len(secret) < 32 || len(secret) > 4096 || strings.ContainsAny(secret, "\r\n") {
		return "", errors.New("invalid private broker credential")
	}
	return secret, nil
}

func enrollBrokerClient(ctx context.Context, origin, identity, path string) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return errors.New("cannot inspect broker credential")
	}
	if os.Getenv("HRX_GITLAB_BROKER_TOKEN_FILE") != "" {
		return errors.New("explicit broker credential file does not exist")
	}
	config, dir, err := loadUserConfig()
	if err != nil {
		return err
	}
	hostPath := config.Plugins.Broker.EnrollmentTokenFile
	if hostPath == "" {
		return errors.New("agent is not enrolled; configure enrollment_token_file for automatic enrollment")
	}
	if strings.HasPrefix(hostPath, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		hostPath = filepath.Join(home, hostPath[2:])
	}
	if !filepath.IsAbs(hostPath) {
		hostPath = filepath.Join(dir, hostPath)
	}
	hostSecret, err := readBrokerSecret(hostPath)
	if err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return errors.New("cannot create private agent credential directory")
	}
	// Recheck each directory after creation, including concurrent enrollment.
	if _, resolved, err := brokerConfiguration(identity); err != nil || resolved != path {
		return errors.New("invalid agent credential directory")
	}
	lock, err := os.OpenFile(filepath.Join(parent, ".enroll.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("cannot lock agent enrollment")
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return errors.New("cannot lock agent enrollment")
		}
		select {
		case <-ctx.Done():
			return errors.New("agent enrollment lock timed out")
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	pending := path + ".pending"
	if _, err := os.Lstat(pending); os.IsNotExist(err) {
		var random [48]byte
		if _, err := rand.Read(random[:]); err != nil {
			return errors.New("cannot generate agent credential")
		}
		f, err := os.OpenFile(pending, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return errors.New("cannot save pending agent credential")
		}
		_, writeErr := f.WriteString(base64.RawURLEncoding.EncodeToString(random[:]) + "\n")
		syncErr := f.Sync()
		closeErr := f.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			return errors.New("cannot save pending agent credential")
		}
	}
	secret, err := readBrokerSecret(pending)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"identity": identity, "client_token": secret})
	req, err := http.NewRequestWithContext(ctx, "POST", origin+"/v1/enroll", bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid enrollment URL")
	}
	req.Header.Set("Authorization", "Bearer "+hostSecret)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("broker enrollment unavailable; retry the same launch")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("broker enrollment refused; check host namespace policy")
	}
	var result struct {
		Identity string `json:"identity"`
		Enrolled bool   `json:"enrolled"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result) != nil || result.Identity != identity || !result.Enrolled {
		return errors.New("invalid broker enrollment response")
	}
	if err := os.Rename(pending, path); err != nil {
		return errors.New("cannot activate agent credential; retry the same launch")
	}
	return nil
}
