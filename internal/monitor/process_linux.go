package monitor

import (
	"fmt"
	"io"
	"os"
)

func processEnvironment(pid int) ([]string, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4*1024*1024))
	if err != nil {
		return nil, err
	}
	return stringsFromEnvironment(b), nil
}
