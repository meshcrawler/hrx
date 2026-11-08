package backend

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

//go:embed assets/enroll_agent.py
var enrollmentHelper string

var agentIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var boardNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// PitputCredential contains only a selected worker's credential. The machine
// enrollment credential is consumed inside the controller's Pitput installation.
type PitputCredential struct {
	Identity string   `json:"identity"`
	Boards   []string `json:"boards"`
	URL      string   `json:"url"`
	token    string
	caPEM    string
}

func (p PitputCredential) String() string {
	return fmt.Sprintf("Pitput profile %s (credential redacted)", p.Identity)
}
func (p PitputCredential) GoString() string { return p.String() }
func (p PitputCredential) PrivateJSON() ([]byte, error) {
	if p.token == "" {
		return nil, errors.New("no scoped Pitput credential")
	}
	return json.Marshal(map[string]string{"url": p.URL, "token": p.token, "ca_pem": p.caPEM})
}

func enrollmentBoards(boards []string) ([]string, error) {
	result := make([]string, 0, len(boards))
	for _, board := range boards {
		board = strings.TrimPrefix(board, "/b/")
		if !boardNamePattern.MatchString(board) || len(board) > 128 {
			return nil, errors.New("invalid enrollment board")
		}
		result = append(result, board)
	}
	slices.Sort(result)
	result = slices.Compact(result)
	if len(result) == 0 || len(result) > 128 {
		return nil, errors.New("explicit enrollment boards required")
	}
	return result, nil
}

// EnrollAgent uses Pitput's idempotent enrollment journal and validates the exact
// identity/board grant before exposing private transfer bytes. No shell is used.
func EnrollAgent(ctx context.Context, python, identity string, boards []string) (PitputCredential, error) {
	if !filepath.IsAbs(python) || !agentIdentityPattern.MatchString(identity) || len(identity) > 240 {
		return PitputCredential{}, errors.New("explicit controller Python and agent identity required")
	}
	selected, err := enrollmentBoards(boards)
	if err != nil {
		return PitputCredential{}, err
	}
	input, _ := json.Marshal(map[string]any{"identity": identity, "boards": selected})
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, python, "-c", enrollmentHelper)
	command.Stdin = bytes.NewReader(input)
	output := &boundedOutput{limit: 1024 * 1024}
	command.Stdout = output
	if command.Run() != nil || output.overflow {
		return PitputCredential{}, errors.New("scoped Pitput enrollment failed; controller diagnostics suppressed")
	}
	return decodeEnrollment(output.buffer.Bytes(), identity, selected)
}

func decodeEnrollment(data []byte, identity string, boards []string) (PitputCredential, error) {
	var response struct {
		Identity string   `json:"identity"`
		Boards   []string `json:"boards"`
		Pitput   struct {
			URL   string `json:"url"`
			Token string `json:"token"`
			CAPEM string `json:"ca_pem"`
		} `json:"pitput"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&response) != nil || decoder.Decode(new(any)) != io.EOF {
		return PitputCredential{}, errors.New("invalid scoped enrollment response")
	}
	selected, err := enrollmentBoards(response.Boards)
	if err != nil || response.Identity != identity || !slices.Equal(selected, boards) || !secureURL(response.Pitput.URL) || response.Pitput.Token == "" || len(response.Pitput.Token) > 4096 || strings.ContainsAny(response.Pitput.Token, " \t\r\n") {
		return PitputCredential{}, errors.New("enrollment grant does not match requested identity and boards")
	}
	return PitputCredential{Identity: identity, Boards: selected, URL: response.Pitput.URL, token: response.Pitput.Token, caPEM: response.Pitput.CAPEM}, nil
}
