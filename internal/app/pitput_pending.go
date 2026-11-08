package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/meshcrawler/hrx/internal/coveragecontroller"
)

var errPitputPending = errors.New("exact authenticated Pitput pending verification unavailable")

func pitputPendingArgs(root, identity string, request coveragecontroller.OwnedPendingRequest) ([]string, error) {
	if !filepath.IsAbs(root) || !pitputThreadID.MatchString(request.ThreadID) ||
		!regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(request.RuntimeBinding) ||
		!regexp.MustCompile(`^delivery-[0-9a-f]{64}$`).MatchString(request.DeliveryID) || identity == "" {
		return nil, errPitputPending
	}
	// Only this runtime's host-owned config is selectable; no caller config path
	// or credential enters the callback or command. Python revalidates mapping.
	config := filepath.Join(root, request.ThreadID, "@combined", "bridge.toml")
	return []string{"bridge", "verify-pending", "--config", config, "--agent-id", identity,
		"--thread-id", request.ThreadID, "--runtime-binding", request.RuntimeBinding,
		"--delivery-id", request.DeliveryID}, nil
}

func verifyPitputPendingResponse(raw []byte, request coveragecontroller.OwnedPendingRequest) error {
	if len(raw) > 1024 {
		return errPitputPending
	}
	var result struct {
		Protocol string `json:"protocol"`
		Delivery string `json:"delivery_id"`
		Pending  bool   `json:"pending"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || result.Protocol != "combined-attention.v1" ||
		result.Delivery != request.DeliveryID || !result.Pending {
		return errPitputPending
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errPitputPending
	}
	return nil
}

func (r *pitputRuntime) pendingVerifier(root string, env []string, project string) func(context.Context, coveragecontroller.OwnedPendingRequest) error {
	if !r.combined {
		return nil
	}
	return func(ctx context.Context, request coveragecontroller.OwnedPendingRequest) error {
		args, err := pitputPendingArgs(root, r.identity, request)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, r.cli, args...)
		command.Env, command.Dir = env, project
		var output limitedBuffer
		command.Stdout, command.Stderr = &output, io.Discard
		if command.Run() != nil {
			return errPitputPending
		}
		return verifyPitputPendingResponse(output.Bytes(), request)
	}
}
