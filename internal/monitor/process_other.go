//go:build !darwin && !linux

package monitor

import "errors"

func processEnvironment(pid int) ([]string, error) {
	return nil, errors.New("process discovery unsupported")
}
