package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Credentials stay in the child environment and a private, ephemeral file; they
// never enter CLI arguments, persisted session metadata, or diagnostics.
func openCodeRequest(ctx context.Context, endpoint, password, method, path string, body any, result any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid private OpenCode request")
	}
	req.SetBasicAuth("opencode", password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		return errors.New("private OpenCode API unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("private OpenCode API rejected request")
	}
	if result != nil {
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result); err != nil {
			return errors.New("invalid private OpenCode response")
		}
	}
	return nil
}

func validateOpenCodeBinding(info openCodeSession, id, project string) error {
	actual, err := filepath.EvalSymlinks(info.Location.Directory)
	if err != nil || actual != project || info.ID != id || info.ParentID != "" || info.Time.Archived != nil {
		return errors.New("OpenCode session is not the requested root session in this project")
	}
	return nil
}

func startOpenCodePitput(ctx context.Context, l *Launch, root, tmp, receipt string, out io.Writer) (processes []*pitputProcess, err error) {
	r := l.pitputRuntime
	id := r.record.NativeID
	if !nativeSessionID.MatchString(id) {
		return nil, errors.New("invalid OpenCode bridge session")
	}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return nil, err
	}
	password := hex.EncodeToString(secret)
	canonicalTmp, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		return nil, err
	}
	passwordFile := filepath.Join(canonicalTmp, "opencode-password")
	if err = os.WriteFile(passwordFile, []byte(password), 0600); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)
	configs, err := r.openCodeBridgeConfigs(root, endpoint, passwordFile, id)
	if err != nil {
		return nil, err
	}
	if err := installOpenCodeActionGuard(l, canonicalTmp); err != nil {
		return nil, err
	}
	if err := bindOpenCodeActionGuard(l, configs); err != nil {
		return nil, err
	}
	l.Env = childEnvironment(l.Env, map[string]string{"OPENCODE_SERVER_PASSWORD": password, "OPENCODE_SERVER_USERNAME": "opencode"}, []string{"OPENCODE_PASSWORD"})
	p, err := startPitputProcess(ctx, "opencode", []string{"serve", "--hostname", "127.0.0.1", "--port", fmt.Sprint(port)}, l.Env, r.record.Project)
	if err != nil {
		return nil, errors.New("cannot start private OpenCode server")
	}
	processes = append(processes, p)
	path := "/api/session/" + url.PathEscape(id)
	deadline := time.Now().Add(15 * time.Second)
	var response struct {
		Data openCodeSession `json:"data"`
	}
	for {
		err = openCodeRequest(ctx, endpoint, password, "GET", path, nil, &response)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return processes, ctx.Err()
		case e := <-p.done:
			p.done <- e
			return processes, errors.New("private OpenCode server exited before binding")
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return processes, errors.New("private OpenCode server did not become ready")
		}
	}
	if err = validateOpenCodeBinding(response.Data, id, r.record.Project); err != nil {
		return processes, err
	}
	model := map[string]string{"providerID": "hrx", "id": l.Model}
	if l.Reasoning != "" {
		model["variant"] = l.Reasoning
	}
	if err = openCodeRequest(ctx, endpoint, password, "POST", path+"/model", map[string]any{"model": model}, nil); err != nil {
		return processes, err
	}
	l.Args = slices.DeleteFunc(l.Args, func(arg string) bool { return arg == "--standalone" })
	l.Args = insertOpenCodeFlags(l.Args, "--server", endpoint)
	for _, config := range configs {
		if err := r.prepareBridge(ctx, config, false); err != nil {
			return processes, err
		}
		p, err := startPitputProcess(ctx, r.cli, []string{"bridge", "run", "--config", config}, l.Env, r.record.Project)
		if err != nil {
			return processes, errors.New("Pitput OpenCode bridge failed to start")
		}
		processes = append(processes, p)
	}
	if receipt != "" {
		if err = os.WriteFile(receipt, []byte(id), 0600); err != nil {
			return processes, err
		}
	}
	fmt.Fprintf(out, "hrx: Pitput bridge connected: %s -> %s (%s)\n", r.identity, id, r.targets())
	return processes, nil
}

func openCodeBridge(endpoint, passwordFile, project string) bridgeConfig {
	return bridgeConfig{Backend: "opencode", OpenCodeURL: endpoint, OpenCodePasswordFile: passwordFile,
		OpenCodeProjectDirectory: project, RetrySeconds: 2}
}

func (r *pitputRuntime) openCodeBridgeConfigs(root, endpoint, passwordFile, id string) ([]string, error) {
	if r.combined {
		return r.nativeCombinedConfig(root, root, id, openCodeBridge(endpoint, passwordFile, r.record.Project))
	}
	var paths []string
	for _, board := range r.boards {
		dir := filepath.Join(root, id, board)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		state, err := r.bridgeState(root, board)
		if err != nil {
			return nil, err
		}
		config := openCodeBridge(endpoint, passwordFile, r.record.Project)
		config.StateDirectory, config.Agents = state, []bridgeAgent{r.boardAgent(board, id)}
		path, err := writeBridgeConfig(dir, config)
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	if r.dm {
		path, err := r.directMessageConfig(root, id, openCodeBridge(endpoint, passwordFile, r.record.Project))
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}
