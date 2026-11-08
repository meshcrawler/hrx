package monitor

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// PitputInfo is a launch-time observation, not live delivery or message activity.
// Keep board metadata nested so future DM status can be added independently.
type PitputInfo struct {
	Enabled  bool          `json:"enabled"`
	Identity string        `json:"identity,omitempty"`
	Boards   []PitputBoard `json:"boards"`
}

type PitputBoard struct {
	Name       string `json:"name"`
	Membership string `json:"membership"`
}

// Other launch capabilities may add sibling status fields. Ignore those fields
// even when an enclosing API uses strict decoding; do not forward unknown data.
func (p *PitputInfo) UnmarshalJSON(data []byte) error {
	type wire PitputInfo
	var value wire
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*p = PitputInfo(value)
	return nil
}

var boardPattern = regexp.MustCompile(`^/b/[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
var pitputIdentity = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// NewPitputInfo accepts only the public board/identity subset of saved launch
// settings or enrollment output. It never reads profiles, transcripts or tokens.
func NewPitputInfo(enabled bool, identity string, names []string, membership string) *PitputInfo {
	p := &PitputInfo{Enabled: enabled, Boards: []PitputBoard{}}
	if !enabled {
		return p
	}
	if len(identity) <= 200 && pitputIdentity.MatchString(identity) {
		p.Identity = identity
	}
	seen := map[string]bool{}
	for _, name := range names {
		name = "/b/" + strings.TrimPrefix(name, "/b/")
		if boardPattern.MatchString(name) && !seen[name] && len(p.Boards) < 64 {
			p.Boards = append(p.Boards, PitputBoard{Name: name, Membership: membership})
			seen[name] = true
		}
	}
	sort.Slice(p.Boards, func(i, j int) bool { return p.Boards[i].Name < p.Boards[j].Name })
	return p
}

func ValidatePitput(p *PitputInfo) bool {
	if p == nil {
		return true // Old senders do not supply this field.
	}
	if len(p.Boards) > 64 || len(p.Identity) > 200 || (p.Identity != "" && !pitputIdentity.MatchString(p.Identity)) {
		return false
	}
	if !p.Enabled && (len(p.Boards) != 0 || p.Identity != "") {
		return false
	}
	seen := map[string]bool{}
	for _, b := range p.Boards {
		if !boardPattern.MatchString(b.Name) || seen[b.Name] || (b.Membership != "configured" && b.Membership != "joined") {
			return false
		}
		seen[b.Name] = true
	}
	return true
}
