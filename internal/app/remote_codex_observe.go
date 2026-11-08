package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type codexObserveRequest struct {
	Endpoint string `json:"endpoint"`
	Session  string `json:"session"`
	Project  string `json:"project"`
}

type codexObserveResult struct {
	CompletedTurns int      `json:"completed_turns"`
	Binding        bool     `json:"native_binding"`
	Actions        []string `json:"native_actions"`
}

// This short-lived auxiliary client can only initialize and read one thread
// through the existing board admission endpoint. It never starts or resumes work.
func runRemoteCodexObserve(ctx context.Context, input io.Reader, output io.Writer) error {
	var request codexObserveRequest
	decoder := json.NewDecoder(io.LimitReader(input, 16385))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || !strings.HasPrefix(request.Endpoint, "unix:///") || request.Session == "" || !filepath.IsAbs(request.Project) {
		return errors.New("invalid Codex observation binding")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	dialer := websocket.Dialer{HandshakeTimeout: time.Second, NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(request.Endpoint, "unix://"))
	}}
	conn, response, err := dialer.DialContext(ctx, "ws://localhost/", http.Header{})
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		return errors.New("Codex observation endpoint unavailable")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetReadLimit(4 << 20)
	deadline, _ := ctx.Deadline()
	conn.SetReadDeadline(deadline)
	conn.SetWriteDeadline(deadline)
	call := func(id int, method string, params any) (json.RawMessage, error) {
		if err := conn.WriteJSON(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return nil, err
		}
		for n := 0; n < 512; n++ {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return nil, err
			}
			var reply struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(raw, &reply) != nil {
				return nil, errors.New("invalid observation response")
			}
			if reply.ID == nil {
				continue
			}
			if *reply.ID != id || len(reply.Error) > 0 || len(reply.Result) == 0 {
				return nil, errors.New("observation response mismatch")
			}
			return reply.Result, nil
		}
		return nil, errors.New("observation frame budget exceeded")
	}
	if _, err = call(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "hrx-readiness-observer", "version": version}, "capabilities": map[string]bool{"experimentalApi": true}}); err != nil {
		return errors.New("Codex observation handshake failed")
	}
	if err = conn.WriteJSON(map[string]any{"method": "initialized"}); err != nil {
		return errors.New("Codex observation handshake failed")
	}
	raw, err := call(2, "thread/read", map[string]any{"threadId": request.Session, "includeTurns": true})
	if err != nil {
		return errors.New("Codex thread observation failed")
	}
	result, err := codexReadEvidence(raw, request)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}

func codexReadEvidence(raw []byte, request codexObserveRequest) (codexObserveResult, error) {
	var response struct {
		Thread struct {
			ID    string `json:"id"`
			CWD   string `json:"cwd"`
			Turns []struct {
				Status string `json:"status"`
				Items  []struct {
					Type     string `json:"type"`
					Status   string `json:"status"`
					ExitCode *int   `json:"exitCode"`
					Command  string `json:"command"`
				} `json:"items"`
			} `json:"turns"`
		} `json:"thread"`
	}
	result := codexObserveResult{Actions: []string{}}
	if json.Unmarshal(raw, &response) != nil {
		return result, errors.New("invalid Codex thread observation")
	}
	if response.Thread.ID != request.Session || response.Thread.CWD != request.Project {
		return result, nil
	}
	result.Binding = true
	for _, turn := range response.Thread.Turns {
		if turn.Status == "completed" {
			result.CompletedTurns++
		}
		for _, item := range turn.Items {
			if item.Type != "commandExecution" || item.Status != "completed" || item.ExitCode == nil || *item.ExitCode != 0 || !strings.Contains(item.Command, "pitput") {
				continue
			}
			for _, action := range [][2]string{{"read", "attention read"}, {"post", " post "}, {"ack", "bridge ack"}, {"ack", "attention ack"}} {
				if strings.Contains(item.Command, action[1]) {
					result.Actions = append(result.Actions, action[0])
				}
			}
		}
	}
	return result, nil
}
