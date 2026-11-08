package catalog

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const MaxJSONBytes = 16 << 20

func Read(path string) (Snapshot, error) {
	var s Snapshot
	f, err := os.Open(path)
	if err != nil {
		return s, errors.New("no readable cache; run hrx refresh")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxJSONBytes+1))
	if err != nil || len(data) > MaxJSONBytes {
		return s, errors.New("cache is unreadable or too large")
	}
	if err = json.Unmarshal(data, &s); err != nil {
		return s, errors.New("invalid cache JSON; run hrx refresh")
	}
	return s, nil
}

func Write(path string, s Snapshot) error {
	return WriteJSON(path, s)
}

// WriteJSON atomically publishes a private generated artifact beside its cache.
func WriteJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".hrx-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
