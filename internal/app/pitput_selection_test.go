package app

import (
	"github.com/meshcrawler/hrx/internal/session"
	"reflect"
	"testing"
)

func TestPitputTargetedOptions(t *testing.T) {
	for _, harness := range []string{"codex", "opencode", "claude"} {
		o, err := parseLaunchFor(harness, []string{"--pitput", "--pitput-follow-thread=/b/dev/topic"})
		if err != nil || !o.PitputTargeted || len(o.PitputFollowThreads) != 1 {
			t.Fatalf("%s: %+v %v", harness, o, err)
		}
	}
	for _, args := range [][]string{
		{"--pitput-targeted"},
		{"--pitput", "--pitput-targeted=true"},
		{"--pitput", "--pitput-follow-thread"},
		{"--pitput", "--pitput-follow-thread=/b/dev/topic/3"},
		{"--pitput", "--pitput-follow-thread=/b/dev/../other"},
		{"--pitput", "--pitput-targeted", "--pitput-dm-only"},
		{"--pitput", "--", "--pitput-targeted"},
	} {
		if _, err := ParseLaunch(args); err == nil {
			t.Fatalf("accepted invalid selection: %v", args)
		}
	}
}

func TestPitputTargetedBoardConfig(t *testing.T) {
	r := &pitputRuntime{identity: "host/dev/agent", targeted: true,
		followed: []string{"/b/dev/one", "/b/devops/two", "/b/ops/three"}}
	got := r.boardAgent("dev", "session")
	want := bridgeAgent{AgentID: r.identity, Board: "dev", ThreadID: "session",
		Targeted: true, FollowedThreads: []string{"/b/dev/one"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cross-board selection or omitted target: %#v", got)
	}
	if got := r.boardAgent("quiet", "session"); got.FollowedThreads != nil {
		t.Fatal("empty followed set should remain mention-only")
	}
	r.targeted = false
	if got := r.boardAgent("dev", "session"); !reflect.DeepEqual(got, bridgeAgent{AgentID: r.identity, Board: "dev", ThreadID: "session"}) {
		t.Fatalf("legacy config changed: %#v", got)
	}
}

func TestPitputDefaultSelectionAndExplicitBroad(t *testing.T) {
	for _, harness := range []string{"codex", "opencode", "claude"} {
		for _, tc := range []struct {
			args     []string
			targeted bool
		}{
			{[]string{"--pitput"}, true},
			{[]string{"--pitput", "--pitput-all-activity"}, false},
			{[]string{"--pitput", "--pitput-dm-only"}, false},
			{[]string{"--pitput", "--resume", "session"}, false},
		} {
			o, err := parseLaunchFor(harness, tc.args)
			if err != nil || o.PitputTargeted != tc.targeted {
				t.Fatalf("%s %v: %+v %v", harness, tc.args, o, err)
			}
		}
	}
	for _, args := range [][]string{
		{"--pitput-all-activity"},
		{"--pitput", "--pitput-all-activity=true"},
		{"--pitput", "--pitput-all-activity", "--pitput-targeted"},
		{"--pitput", "--pitput-follow-thread=/b/dev/topic", "--pitput-all-activity"},
		{"--pitput", "--pitput-all-activity", "--pitput-dm-only"},
	} {
		if _, err := ParseLaunch(args); err == nil {
			t.Fatal("accepted conflicting selection", args)
		}
	}
}
func TestPitputResumeDoesNotMigrateSavedSelection(t *testing.T) {
	for _, targeted := range []bool{false, true} {
		o, err := ParseLaunch([]string{"--pitput", "--resume", "session"})
		if err != nil {
			t.Fatal(err)
		}
		restorePitputSelection(&o, session.Record{Pitput: true, PitputTargeted: targeted})
		if o.PitputTargeted != targeted {
			t.Fatal("resume changed saved policy", o)
		}
	}
	o, _ := ParseLaunch([]string{"--pitput", "--resume", "session", "--pitput-all-activity"})
	restorePitputSelection(&o, session.Record{Pitput: true, PitputTargeted: true, PitputFollowThreads: []string{"/b/dev/topic"}})
	if o.PitputTargeted || len(o.PitputFollowThreads) != 0 {
		t.Fatal("explicit broad option ignored", o)
	}
}
