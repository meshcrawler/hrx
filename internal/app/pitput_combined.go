package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// This helper addresses only the existing private owned ingress. It never
// launches a native process, reads its stream or substitutes another target.
func runPitputAdmit(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("_pitput-admit", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	endpoint := flags.String("endpoint", "", "")
	binding := flags.String("runtime-binding", "", "")
	thread := flags.String("thread", "", "")
	delivery := flags.String("delivery-id", "", "")
	message := flags.String("message", "", "")
	attempt := flags.Uint64("attempt", 0, "")
	observeOnly := flags.Bool("observe-only", false, "")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *attempt == 0 || *attempt > uint64(1<<63-1) || !strings.HasPrefix(*endpoint, "unix:///") ||
		!pitputThreadID.MatchString(*thread) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(*binding) ||
		!regexp.MustCompile(`^delivery-[0-9a-f]{64}$`).MatchString(*delivery) || len(*message) == 0 || len(*message) > 16384 {
		return errors.New("invalid owned attention request")
	}
	socket := strings.TrimPrefix(*endpoint, "unix://")
	parent, err := os.Lstat(filepath.Dir(socket))
	if err != nil || !parent.IsDir() || parent.Mode().Perm() != 0700 {
		return errors.New("owned attention endpoint unavailable")
	}
	owner, ok := parent.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return errors.New("owned attention endpoint unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := connectPitputSocket(ctx, socket)
	if err != nil {
		return errors.New("owned attention endpoint unavailable")
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	request := ownedAdmissionRequest{Protocol: "hrx-owned-attention.v1", Runtime: *binding, Thread: *thread, Delivery: *delivery, Text: *message, Attempt: *attempt}
	raw, err := ownedAdmissionExchangeMode(request, func(method string, params any) (json.RawMessage, error) {
		if conn.WriteJSON(map[string]any{"id": *delivery, "method": method, "params": params}) != nil {
			return nil, errors.New("owned attention outcome uncertain")
		}
		var response struct {
			ID     string          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if conn.ReadJSON(&response) != nil || response.ID != *delivery || len(response.Error) != 0 {
			return nil, errors.New("owned attention outcome uncertain")
		}
		return response.Result, nil
	}, *observeOnly)
	if len(raw) != 0 {
		if _, writeErr := out.Write(append(raw, '\n')); writeErr != nil {
			return writeErr
		}
	}
	return err
}

func (r *pitputRuntime) combinedConfig(root, binary, endpoint, id, binding string) ([]string, error) {
	if !r.dm || len(r.boards) != 1 {
		return nil, errors.New("combined attention preview requires one board and DM consumption")
	}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(user, ".codex")
	}
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		return nil, err
	}
	executable := r.admissionBinary
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return nil, err
		}
	}
	dir := filepath.Join(root, id, "@combined")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	state, err := r.bridgeState(root, "@combined")
	if err != nil {
		return nil, err
	}
	agent := r.boardAgent(r.boards[0], id)
	agent.Combined = true
	path, err := writeBridgeConfig(dir, bridgeConfig{CodexBinary: binary, CodexHome: home,
		StateDirectory: state, RetrySeconds: 2,
		OwnedAdmissionBinary: executable, OwnedAdmissionEndpoint: endpoint,
		OwnedAdmissionBinding: binding, Agents: []bridgeAgent{agent}})
	if err != nil {
		return nil, err
	}
	return []string{path}, nil
}

// Native backends retain their existing ingress and lifecycle fields. Only the
// source selection and durable worker state are shared with the combined path.
func (r *pitputRuntime) nativeCombinedConfig(root, stateRoot, id string, config bridgeConfig) ([]string, error) {
	if !r.dm || len(r.boards) != 1 {
		return nil, errors.New("combined attention preview requires one board and DM consumption")
	}
	dir := filepath.Join(root, id, "@combined")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	state, err := r.bridgeState(stateRoot, "@combined")
	if err != nil {
		return nil, err
	}
	agent := r.boardAgent(r.boards[0], id)
	agent.Combined = true
	config.StateDirectory, config.Agents = state, []bridgeAgent{agent}
	path, err := writeBridgeConfig(dir, config)
	if err != nil {
		return nil, err
	}
	return []string{path}, nil
}
