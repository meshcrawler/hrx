package backend

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"strconv"
)

//go:embed assets/agent_logs.py
var agentLogsHelper string

type AgentLogs struct {
	Deployment string `json:"deployment"`
	Source     string `json:"source"`
	Text       string `json:"text"`
	Truncated  bool   `json:"truncated"`
	Lines      int    `json:"lines_requested"`
}

func (p Proxmox) Logs(ctx context.Context, id string, spec ContainerSpec, lines int) (AgentLogs, error) {
	if lines < 1 || lines > 2000 {
		return AgentLogs{}, errors.New("log lines must be between 1 and 2000")
	}
	result, err := p.Guest(ctx, id, spec, []string{"python3", "-c", agentLogsHelper, id, strconv.Itoa(lines)}, nil)
	if err != nil {
		return AgentLogs{}, err
	}
	var logs AgentLogs
	if json.Unmarshal([]byte(result.GuestOutput), &logs) != nil || logs.Deployment != id || logs.Lines != lines || len(logs.Text) > 4*65536 || (logs.Source != "pane" && logs.Source != "previous-pane") {
		return AgentLogs{}, errors.New("invalid native log observation")
	}
	return logs, nil
}
