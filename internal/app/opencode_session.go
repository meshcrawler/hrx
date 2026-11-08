package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"
)

var nativeSessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,255}$`)

type openCodeSession struct {
	ID       string `json:"id"`
	ParentID string `json:"parentID"`
	Time     struct {
		Archived *float64 `json:"archived"`
	} `json:"time"`
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
}

// openCodeNativeSession uses only documented CLI/API operations. Create allocates
// an empty native session; get proves a resume target exists before --session
// can silently create one. Neither operation sends a model prompt.
func openCodeNativeSession(ctx context.Context, launch Launch, project, existing string) (string, error) {
	canonical, err := filepath.EvalSymlinks(project)
	if err != nil || !filepath.IsAbs(canonical) {
		return "", errors.New("invalid OpenCode session project")
	}
	args := []string{"api", "--standalone"}
	if existing == "" {
		model := map[string]string{"providerID": "hrx", "id": launch.Model}
		if launch.Reasoning != "" {
			model["variant"] = launch.Reasoning
		}
		body, err := json.Marshal(map[string]any{"location": map[string]string{"directory": canonical}, "model": model})
		if err != nil {
			return "", errors.New("invalid OpenCode session creation metadata")
		}
		args = append(args, "session.create", "--data", string(body))
	} else {
		if !nativeSessionID.MatchString(existing) {
			return "", errors.New("invalid OpenCode session ID")
		}
		args = append(args, "session.get", "--param", "sessionID="+existing)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "opencode", args...)
	cmd.Dir = canonical
	cmd.Env = launch.Env
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Stdin = nil
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	var output limitedBuffer
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return "", errors.New("OpenCode session API failed; refusing to create a replacement resume session")
	}
	var response struct {
		Data openCodeSession `json:"data"`
	}
	if json.Unmarshal(output.Bytes(), &response) != nil || !nativeSessionID.MatchString(response.Data.ID) {
		return "", errors.New("invalid OpenCode session API response")
	}
	if existing != "" && response.Data.ID != existing {
		return "", errors.New("OpenCode returned a different session")
	}
	actual, err := filepath.EvalSymlinks(response.Data.Location.Directory)
	if err != nil || actual != canonical {
		return "", errors.New("OpenCode session belongs to a different project")
	}
	return response.Data.ID, nil
}
