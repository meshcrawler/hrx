package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Keep only approved launch metadata; credentials and arbitrary environment
// values are never returned, persisted, or included in diagnostics.
func stringsFromEnvironment(b []byte) []string {
	var result []string
	for _, entry := range strings.Split(string(b), "\x00") {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "HOME", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "XDG_STATE_HOME", "HRX_SESSION_METADATA", "HRX_AGENT_IDENTITY", RunEnv, "HRX_MONITOR", "HRX_GITLAB_CREDENTIAL_HOST", "HRX_GITLAB_CREDENTIAL_USER":
			if len(entry) <= 16384 {
				result = append(result, entry)
			}
		}
	}
	return result
}

func recoveredRecord(pid int, start, harness string, env []string) (Record, bool) {
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	if values[RunEnv] != "" || values["HRX_MONITOR"] == "off" || start == "" {
		return Record{}, false
	}
	var meta struct {
		Harness string `json:"harness"`
		Model   string `json:"model_identity"`
		Session string `json:"native_id"`
		Agent   string `json:"yesod_agent"`
	}
	if json.Unmarshal([]byte(values["HRX_SESSION_METADATA"]), &meta) != nil || meta.Harness != harness || meta.Model == "" {
		return Record{}, false
	}
	stamp, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", start, time.Local)
	if err != nil {
		return Record{}, false
	}
	agent := values["HRX_AGENT_IDENTITY"]
	if agent == "" {
		agent = meta.Agent
	}
	if agent == "" {
		agent = "Unnamed agent"
	}
	credential := Credential{Provider: "unknown"}
	if values["HRX_GITLAB_CREDENTIAL_HOST"] != "" && values["HRX_GITLAB_CREDENTIAL_USER"] != "" {
		credential = Credential{Provider: "gitlab", Host: values["HRX_GITLAB_CREDENTIAL_HOST"], Username: values["HRX_GITLAB_CREDENTIAL_USER"]}
	}
	id := sha256.Sum256([]byte(strconv.Itoa(pid) + "/" + start))
	return Record{RunID: hex.EncodeToString(id[:16]), PID: pid, ProcessStart: start, Agent: agent, Harness: harness, Model: meta.Model, SessionID: meta.Session, StartedAt: stamp.UTC(), LastActivity: stamp.UTC(), Activity: "recovered-launch", Credential: credential}, true
}

// Recover older HRX launches without changing or restarting the harness. Merely
// finding a Claude/Codex process or a Pitput enrollment is not an HRX binding.
func (s Store) Recover(ctx context.Context) error {
	processes, err := ProcessTable(ctx)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,uid=,comm=")
	b, err := cmd.Output()
	if err != nil {
		return nil
	} // Existing observations remain usable.
	type process struct {
		pid, parent int
		harness     string
	}
	parents := map[int]int{}
	var candidates []process
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		parent, _ := strconv.Atoi(f[1])
		uid, _ := strconv.Atoi(f[2])
		parents[pid] = parent
		harness := filepath.Base(strings.Join(f[3:], " "))
		if uid == os.Getuid() && (harness == "claude" || harness == "codex" || harness == "opencode") {
			candidates = append(candidates, process{pid, parent, harness})
		}
	}
	recovered := map[int]Record{}
	for _, p := range candidates {
		if ctx.Err() != nil {
			break
		}
		env, err := processEnvironment(p.pid)
		if err != nil {
			continue
		}
		if !s.ownsEnvironment(env) {
			continue
		}
		if r, ok := recoveredRecord(p.pid, processes[p.pid], p.harness, env); ok {
			BindTelemetry(&r, env)
			recovered[p.pid] = r
		}
	}
	for pid, r := range recovered {
		// Child harness processes inherit the parent's metadata; do not count
		// those as separate legacy launches.
		nested := false
		visited := map[int]bool{}
		for parent := parents[pid]; parent > 0 && !visited[parent]; parent = parents[parent] {
			visited[parent] = true
			if _, ok := recovered[parent]; ok {
				nested = true
				break
			}
		}
		if nested {
			continue
		}
		r.HostID, err = s.HostID()
		if err != nil {
			return err
		}
		r.Host, _ = os.Hostname()
		if err := s.update(r.RunID, func(current *Record) {
			if current.RunID == "" {
				*current = r
			}
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s Store) ownsEnvironment(env []string) bool {
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	base := values["XDG_STATE_HOME"]
	if base == "" && filepath.IsAbs(values["HOME"]) {
		base = filepath.Join(values["HOME"], ".local", "state")
	}
	return filepath.IsAbs(base) && filepath.Clean(s.Dir) == filepath.Join(base, "hrx", "monitor")
}
