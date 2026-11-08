package app

import (
	"errors"
	"strings"
)

func validatePitputSelection(o LaunchOptions, harness string) error {
	if o.PitputCombinedPreview && (!o.Pitput || (harness != "codex" && harness != "opencode" && harness != "claude") || o.PitputDM != "" || len(o.PitputBoards) != 1) {
		return errors.New("--pitput-combined-preview requires Codex, OpenCode or Claude, --pitput and one --pitput-board with DM consumption")
	}
	if o.PitputAllActivity && (!o.Pitput || o.PitputTargeted || len(o.PitputFollowThreads) != 0 || o.PitputDM == "only") {
		return errors.New("--pitput-all-activity requires --pitput and cannot be combined with targeted selection or DM-only")
	}
	if !o.PitputTargeted && len(o.PitputFollowThreads) == 0 {
		return nil
	}
	if !o.Pitput || !o.PitputTargeted {
		return errors.New("targeted Pitput selection requires --pitput --pitput-targeted")
	}
	if harness != "codex" && harness != "opencode" && harness != "claude" {
		return errors.New("targeted Pitput selection requires Codex, Claude or OpenCode")
	}
	if o.PitputDM == "only" {
		return errors.New("targeted board selection cannot be combined with --pitput-dm-only")
	}
	if len(o.PitputFollowThreads) > 128 {
		return errors.New("at most 128 followed Pitput topics are supported")
	}
	for _, topic := range o.PitputFollowThreads {
		parts := strings.Split(topic, "/")
		if len(parts) != 4 || parts[0] != "" || parts[1] != "b" ||
			!pitputBoardName.MatchString(parts[2]) || !pitputBoardName.MatchString(parts[3]) {
			return errors.New("followed Pitput topic must be /b/BOARD/TOPIC")
		}
	}
	return nil
}

func (r *pitputRuntime) boardAgent(board, id string) bridgeAgent {
	agent := bridgeAgent{AgentID: r.identity, Board: board, ThreadID: id}
	if r.targeted {
		agent.Targeted = true
		for _, topic := range r.followed {
			if strings.HasPrefix(topic, "/b/"+board+"/") {
				agent.FollowedThreads = append(agent.FollowedThreads, topic)
			}
		}
	}
	return agent
}
