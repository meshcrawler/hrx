package monitor

import (
	"bytes"
	"encoding/binary"
	"errors"

	"golang.org/x/sys/unix"
)

// Read the OS environment separately from argv: prompt text must never be
// mistaken for an HRX environment marker.
func processEnvironment(pid int) ([]string, error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	return darwinEnvironment(b)
}

func darwinEnvironment(b []byte) ([]string, error) {
	if len(b) < 4 || len(b) > 4*1024*1024 {
		return nil, errors.New("invalid process arguments")
	}
	argc := int(binary.NativeEndian.Uint32(b[:4]))
	b = b[4:]
	i := bytes.IndexByte(b, 0)
	if i < 0 || argc < 1 || argc > 65536 {
		return nil, errors.New("invalid process arguments")
	}
	b = bytes.TrimLeft(b[i+1:], "\x00")
	for n := 0; n < argc; n++ {
		i = bytes.IndexByte(b, 0)
		if i < 0 {
			return nil, errors.New("invalid process arguments")
		}
		b = b[i+1:]
	}
	return stringsFromEnvironment(b), nil
}
