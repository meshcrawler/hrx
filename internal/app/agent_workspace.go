package app

import "strings"

// agentWorkspacePrompt returns the opt-in workspace convention configured by
// [agents] workspace_root, or nothing when it is unset. Shared across harnesses
// and communication adapters; it guides the agent without changing the launch
// directory or relocating a resumed worktree.
func agentWorkspacePrompt() string {
	config, _, err := loadUserConfig()
	if err != nil {
		return ""
	}
	root := strings.TrimRight(strings.TrimSpace(config.Agents.WorkspaceRoot), "/")
	if root == "" {
		return ""
	}
	return "\n## HRX agent workspace\n" +
		"Use your assigned agent directory as your workspace home. If none was assigned, use " + root + "/<agent-name>/, where <agent-name> is your identity's role (the middle component of HOST/ROLE/QUALIFIER). " +
		"If that directory belongs to another identity, choose a separate identity-specific directory under " + root + "; never share another agent's workspace. " +
		"Keep Git worktrees under <agent-directory>/worktrees/<repo>/<task>/, temporary files under <agent-directory>/scratch/, and logs under <agent-directory>/logs/. " +
		"Do not create sibling worktrees beside the source repository. " +
		"Before editing, read the source repository's applicable AGENTS.md instructions, inspect its Git status, record its absolute path, and create your own branch and isolated worktree in your agent directory. " +
		"Run repository commands from that worktree. Preserve existing dirty work and other agents' branches. " +
		"On resume, continue the recorded owned worktree; do not move, delete, or recreate live worktrees merely to match this layout. Record the source path, worktree path, and branch in your handoff. " +
		"Explicit user workspace instructions take precedence. This guidance does not change HRX's launch working directory or grant infrastructure, database, or test-compute permissions.\n"
}
