package backend

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
)

//go:embed assets/release_work.py
var releaseWorkHelper string

type WorkReceipt struct {
	Session   string `json:"session"`
	MessageID string `json:"message_id"`
	Status    string `json:"status"`
}

func (p Proxmox) ReleaseWork(ctx context.Context, id string, spec ContainerSpec, identity, board, prompt string, ready ReadyReceipt) (WorkReceipt, error) {
	var receipt WorkReceipt
	if ready.Session == "" || ready.EventKey == "" || prompt == "" || len(prompt) > 512*1024 {
		return receipt, errors.New("verified readiness and bounded assigned prompt required")
	}
	data, _ := json.Marshal(map[string]string{"deployment": id, "identity": identity, "board": board, "session": ready.Session, "event_key": ready.EventKey, "prompt": prompt})
	result, err := p.Guest(ctx, id, spec, []string{"python3", "-c", releaseWorkHelper}, bytes.NewReader(data))
	if err != nil {
		return receipt, err
	}
	if json.Unmarshal([]byte(result.GuestOutput), &receipt) != nil || receipt.Session != ready.Session || receipt.Status != "released" || receipt.MessageID != fmt.Sprintf("msg_%x", sha256.Sum256([]byte(id+":assigned-work:v1"))) {
		return WorkReceipt{}, errors.New("invalid work release receipt")
	}
	return receipt, nil
}

// ReleaseClaudeWork keeps the task private in the guest and publishes only an
// idempotent release notice through its already-verified Pitput native task bridge.
func (p Proxmox) ReleaseClaudeWork(ctx context.Context, id string, spec ContainerSpec, identity, board, prompt, python, controller string, ready ReadyReceipt) (WorkReceipt, error) {
	return p.releaseNoticeWork(ctx, id, spec, "claude", identity, board, prompt, python, controller, ready)
}

func (p Proxmox) ReleaseCodexWork(ctx context.Context, id string, spec ContainerSpec, identity, board, prompt, python, controller string, ready ReadyReceipt) (WorkReceipt, error) {
	return p.releaseNoticeWork(ctx, id, spec, "codex", identity, board, prompt, python, controller, ready)
}

func (p Proxmox) releaseNoticeWork(ctx context.Context, id string, spec ContainerSpec, harness, identity, board, prompt, python, controller string, ready ReadyReceipt) (WorkReceipt, error) {
	if ready.Session == "" || ready.EventKey == "" || prompt == "" || len(prompt) > 512*1024 {
		return WorkReceipt{}, errors.New("verified readiness and bounded assigned prompt required")
	}
	data, _ := json.Marshal(map[string]string{"operation": "stage-" + harness, "deployment": id, "identity": identity, "board": board, "session": ready.Session, "event_key": ready.EventKey, "prompt": prompt})
	result, err := p.Guest(ctx, id, spec, []string{"python3", "-c", releaseWorkHelper}, bytes.NewReader(data))
	if err != nil {
		return WorkReceipt{}, err
	}
	var receipt WorkReceipt
	if json.Unmarshal([]byte(result.GuestOutput), &receipt) != nil || receipt.Session != ready.Session || receipt.Status != "staged" || receipt.MessageID != fmt.Sprintf("msg_%x", sha256.Sum256([]byte(id+":assigned-work:v1"))) {
		return WorkReceipt{}, errors.New("invalid staged work receipt")
	}
	input, _ := json.Marshal(map[string]string{"action": "work", "deployment": id, "identity": identity, "board": board, "controller": controller, "work_id": receipt.MessageID})
	command := exec.CommandContext(ctx, python, "-c", readinessController)
	command.Stdin = bytes.NewReader(input)
	output := &boundedOutput{limit: 65536}
	command.Stdout = output
	if command.Run() != nil || output.overflow {
		return WorkReceipt{}, errors.New("work notice publication uncertain; retry the same deployment")
	}
	var published struct {
		Published bool   `json:"published"`
		ID        string `json:"work_id"`
	}
	if json.Unmarshal(output.buffer.Bytes(), &published) != nil || !published.Published || published.ID != receipt.MessageID {
		return WorkReceipt{}, errors.New("invalid work publication receipt")
	}
	receipt.Status = "published"
	return receipt, nil
}
