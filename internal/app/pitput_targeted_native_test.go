package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/meshcrawler/hrx/internal/catalog"
	"github.com/meshcrawler/hrx/internal/session"
)

// Driven by tests/integration/pitput_targeted_launch.py against a disposable Pitput
// server: real Pitput bridges, a real Codex app server or OpenCode server, and a
// loopback model fixture (no paid calls). The selection flags start as an argv slice
// and pass through the production chain run.go and runOtherHarness use before
// execHarness: prepareLaunchOptions (parse, defaults, resolveLaunchSession and
// restoreSession), BuildLaunch/buildOtherLaunch, attachProfile, prepareYesod (with
// preparePitputRuntime and real CLI enrollment), prepareSessionCapture, then
// startPitputRuntime and startWithSession in execHarness order.
//
// Helper-level, NOT covered (also listed in the receipt): LoadSettings and the
// gateway catalog refresh (fixture Settings/Snapshot replace them because both
// require an HTTPS gateway trusted by the native harness); GenerateCodexCatalog
// (Codex prompt injection rewrites a fixture catalog that is never passed to
// Codex); and execHarness supervision (no TUI: the test client binds the Codex
// thread, and OpenCode notifications run on its private server without a TUI).
const (
	targetedBoard    = "exchange"
	targetedFollowed = "/b/exchange/followed"
	targetedOther    = "/b/exchange/other"
	targetedWindow   = 20 * time.Second
	// targetedController authors every harness post except the self-authored one.
	targetedController = "test/controller/agent"
)

func TestNativePitputTargetedLaunchCodex(t *testing.T) {
	testNativePitputTargetedLaunch(t, "codex")
}

func TestNativePitputTargetedLaunchOpenCode(t *testing.T) {
	testNativePitputTargetedLaunch(t, "opencode")
}

// targetedGateway counts only inference POSTs for the harness's wire API and keeps
// their bodies in memory for marker matching. Bodies never leave the test.
type targetedGateway struct {
	forward string
	mu      sync.Mutex
	server  *httptest.Server
	bodies  []string
}

func newTargetedGateway(harness string) *targetedGateway {
	g := &targetedGateway{}
	suffix := map[string]string{"codex": "/responses", "opencode": "/chat/completions"}[harness]
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, suffix) {
			g.mu.Lock()
			g.bodies = append(g.bodies, string(body))
			g.mu.Unlock()
		}
		if g.forward != "" {
			request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, g.forward+"/responses", bytes.NewReader(body))
			if err != nil {
				http.Error(w, "fixture proxy failed", 500)
				return
			}
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				http.Error(w, "fixture proxy failed", 500)
				return
			}
			defer response.Body.Close()
			w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
			w.WriteHeader(response.StatusCode)
			io.Copy(w, response.Body)
			return
		}
		if harness == "codex" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"targeted-fixture\",\"status\":\"completed\",\"output\":[]}}\n\n")
			return
		}
		var request struct{ Stream bool }
		_ = json.Unmarshal(body, &request)
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"fixture handled\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"fixture","object":"chat.completion","created":1,"model":"fixture","choices":[{"index":0,"message":{"role":"assistant","content":"fixture handled"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	return g
}

func (g *targetedGateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.bodies)
}

func (g *targetedGateway) since(i int) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.bodies[min(i, len(g.bodies)):]...)
}

// containsMessagePath matches the bridge notification's "Message: PATH" line exactly,
// so /b/exchange/followed/1 never matches a notification for .../followed/12.
func containsMessagePath(body, path string) bool {
	marker := "Message: " + path
	for rest := body; ; {
		i := strings.Index(rest, marker)
		if i < 0 {
			return false
		}
		rest = rest[i+len(marker):]
		if rest == "" || rest[0] < '0' || rest[0] > '9' {
			return true
		}
	}
}

// Pitput 01745f4 delivers a pending event with attention metadata in the compact
// attention-v1 form (attention_delivery.render_attention):
//
//	Pitput board from=SENDER ref=at-HANDLE why=REASON
//	Pitput dm from=SENDER ref=at-HANDLE why=incoming_dm message=dm-ID conversation=dc-ID
//
// optionally preceded by the one-time protocol primer. The legacy text with
// "Message: PATH" (codex_bridge._wake_message) is used only for an event without
// attention metadata. Neither form alone proves delivery: a notice counts only once
// bindTargetedNotice resolves it to the exact posted event held pending by the bridge.
var (
	targetedCompact = regexp.MustCompile(`Pitput (board|dm) from=([A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+){0,2}) ref=(at-[a-z2-7]{28}) why=([a-z_]{1,32})(?: message=(dm-[a-z2-7]{12}) conversation=(dc-[a-z2-7]{12}))?`)
	targetedLegacy  = regexp.MustCompile(`Pitput (attention notification|private direct message) for this existing agent/thread\.(?:\\n|\n)[^"]{0,1000}?Message: (/b/[A-Za-z0-9_-]+/[A-Za-z0-9_-]+/[0-9]{1,19}|dm-[a-z2-7]{12})(?:[^0-9]|$)`)
)

const targetedPrimer = "Pitput attention protocol attention-v1."

type targetedNotice struct {
	Form, Source, Sender, Handle, Reason, Message, Conversation, Path string
}

func (n targetedNotice) key() string { return n.Form + "|" + n.Handle + "|" + n.Path }

// targetedNotices extracts every attention notice (either form) from a model request.
func targetedNotices(body string) []targetedNotice {
	var notices []targetedNotice
	seen := map[string]bool{}
	add := func(n targetedNotice) {
		if !seen[n.key()] {
			seen[n.key()] = true
			notices = append(notices, n)
		}
	}
	for _, m := range targetedCompact.FindAllStringSubmatch(body, -1) {
		add(targetedNotice{Form: "compact", Source: m[1], Sender: m[2], Handle: m[3], Reason: m[4], Message: m[5], Conversation: m[6]})
	}
	for _, m := range targetedLegacy.FindAllStringSubmatch(body, -1) {
		source := "board"
		if m[1] == "private direct message" {
			source = "dm"
		}
		add(targetedNotice{Form: "legacy", Source: source, Path: m[2]})
	}
	return notices
}

// targetedEventTag reproduces Pitput's attention_helpers._event_tag: the last 16
// handle characters are base32(sha256(canonical JSON of the event without its
// cursor)), lower case. Values must be ASCII strings or integers.
func targetedEventTag(event map[string]any) (string, error) {
	keys := make([]string, 0, len(event))
	for key := range event {
		if key != "cursor" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Quote(key))
		b.WriteByte(':')
		switch value := event[key].(type) {
		case string:
			for _, r := range value {
				if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
					return "", errors.New("event value outside the canonical ASCII subset")
				}
			}
			b.WriteString(`"` + value + `"`)
		case json.Number:
			if _, err := value.Int64(); err != nil {
				return "", err
			}
			b.WriteString(value.String())
		case int64:
			b.WriteString(strconv.FormatInt(value, 10))
		default:
			return "", errors.New("unsupported event value")
		}
	}
	b.WriteByte('}')
	sum := sha256.Sum256([]byte(b.String()))
	return strings.ToLower(base32.StdEncoding.EncodeToString(sum[:]))[:16], nil
}

func targetedReadJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(target)
}

// targetedCode keeps only fixed diagnostic codes; anything else is "other".
var targetedCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func targetedCode(value string) string {
	if targetedCodePattern.MatchString(value) {
		return value
	}
	return "other"
}

type targetedStatus struct {
	Running bool
	Agents  []struct {
		Source     string
		Status     string
		StateFile  string         `json:"state_file"`
		CursorFile string         `json:"cursor_file"`
		LastError  *string        `json:"last_error"`
		Protocol   map[string]any `json:"protocol"`
		Pending    *struct {
			EventKey string `json:"event_key"`
			Path     string
			Seq      int64
		}
	}
}

type targetedConfigs struct{ board, dm string }

// targetedPost is one harness write as Pitput returned it. For a DM, Path is the
// message id and EventKey/Conversation come from the DM send itself.
type targetedPost struct {
	Kind         string
	Path         string
	Seq          int64
	EventKey     string `json:"event_key"`
	Conversation string
}

type targetedPosted struct {
	PostedAtMs int64          `json:"posted_at_ms"`
	Posts      []targetedPost `json:"posts"`
	Sentinel   string         `json:"sentinel"`
}

type targetedBaseline struct {
	count    int
	at       time.Time
	stableMs int64
}

type targetedFixture struct {
	t            *testing.T
	harness      string
	identity     string
	home         string
	project      string
	cli          string
	handshake    string
	codexCatalog string
	gateway      *targetedGateway
	settings     Settings
	snapshot     catalog.Snapshot
	// quietPaths are every post that must never reach the model.
	quietPaths []string
	receipt    map[string]any
	receiptAt  string
	// nativeID is the session the test started or bound; bound holds every attention
	// handle already resolved to an expected event.
	nativeID string
	bound    map[string]bool
}

// persist writes everything measured so far, atomically, so a later failure never
// loses an earlier phase's evidence.
func (f *targetedFixture) persist() {
	f.receipt["passed"] = !f.t.Failed()
	f.receipt["updated_at_ms"] = time.Now().UnixMilli()
	data, _ := json.MarshalIndent(f.receipt, "", "  ")
	temporary := f.receiptAt + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0600); err != nil {
		f.t.Error("receipt not written:", err)
		return
	}
	if err := os.Rename(temporary, f.receiptAt); err != nil {
		f.t.Error("receipt not published:", err)
	}
}

func testNativePitputTargetedLaunch(t *testing.T, harness string) {
	if os.Getenv("HRX_NATIVE_SESSION_TESTS") != "1" || os.Getenv("HRX_PITPUT_INTEGRATION") != "1" {
		t.Skip("requires disposable native harness and Pitput integration; run tests/integration/pitput_targeted_launch.py")
	}
	f := &targetedFixture{t: t, harness: harness, identity: os.Getenv("HRX_PITPUT_TEST_IDENTITY"),
		handshake: os.Getenv("HRX_PITPUT_TARGETED_HANDSHAKE"), receipt: map[string]any{"harness": harness, "schema_version": 1}}
	receiptPath := os.Getenv("HRX_PITPUT_TARGETED_RECEIPT")
	if !pitputAgentName.MatchString(f.identity) || f.handshake == "" || receiptPath == "" {
		t.Fatal("HRX_PITPUT_TEST_IDENTITY, HRX_PITPUT_TARGETED_HANDSHAKE and HRX_PITPUT_TARGETED_RECEIPT are set by tests/integration/pitput_targeted_launch.py")
	}
	f.receiptAt, f.bound = receiptPath, map[string]bool{}
	f.receipt["phases_completed"] = []string{}
	t.Cleanup(func() {
		f.receipt["finished_at_ms"] = time.Now().UnixMilli()
		f.persist()
	})
	f.setup()
	f.probe()

	// Fresh launch from a real argv slice.
	fresh := []string{"--pitput", "--pitput-board", targetedBoard, "--pitput-targeted",
		"--pitput-follow-thread", targetedFollowed, "--agent-identity", f.identity, "--model", "fixture"}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	o, l, err := f.launch(ctx, fresh)
	if err != nil {
		t.Fatal("fresh launch preparation failed:", err)
	}
	if !o.PitputTargeted || !slices.Equal(o.PitputFollowThreads, []string{targetedFollowed}) || l.pitputRuntime == nil ||
		!l.pitputRuntime.targeted || !slices.Equal(l.pitputRuntime.followed, []string{targetedFollowed}) {
		t.Fatal("argv selection did not reach the prepared Pitput runtime")
	}
	if !l.pitputRuntime.dm {
		t.Fatal("Pitput direct messages unavailable against the disposable server: " + l.pitputRuntime.dmNotice)
	}
	f.receipt["fresh_launch"] = map[string]any{"argv": fresh, "options_targeted": o.PitputTargeted,
		"options_followed_count": len(o.PitputFollowThreads), "runtime_targeted": l.pitputRuntime.targeted,
		"runtime_boards": l.pitputRuntime.boards, "runtime_dm": l.pitputRuntime.dm}
	stop, rpc, id := f.start(&l, "")
	defer stop()
	configs := f.configs(&l, id)
	freshEntry := f.assertConfigs("fresh", configs, id)
	store, err := session.Default()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.Resolve(harness, f.project, id)
	if err != nil || !saved.Pitput || !saved.PitputTargeted || !slices.Equal(saved.PitputFollowThreads, []string{targetedFollowed}) ||
		saved.AgentIdentity != f.identity || !slices.Equal(saved.PitputBoards, []string{targetedBoard}) {
		t.Fatal("session record lost the targeted selection", err)
	}
	f.receipt["session_record"] = map[string]any{"pitput": saved.Pitput, "targeted": saved.PitputTargeted,
		"followed_count": len(saved.PitputFollowThreads), "followed_matches": true}

	// A: settled baseline, B: unrelated and self-authored activity stays quiet, C: the
	// followed topic is delivered past B (liveness barrier), D: a mention, E: a DM.
	freshPhases := map[string]any{}
	f.receipt["fresh"] = freshPhases
	quiet := f.quiet(&l, "fresh", configs)
	f.phaseDone(freshPhases, "fresh-B", "quiet", quiet)
	barrier := quiet["max_quiet_seq"].(int64)
	followed := f.delivered(&l, "fresh-C", configs, barrier, true)
	f.phaseDone(freshPhases, "fresh-C", "followed", followed)
	f.phaseDone(freshPhases, "fresh-D", "mention", f.delivered(&l, "fresh-D", configs, followed["seq"].(int64), true))
	f.phaseDone(freshPhases, "fresh-E", "dm", f.directMessage(&l, configs))
	f.assertQuietNeverDelivered()

	// Owned cleanup: no running bridge, no leftover consumer, no private endpoint.
	monitorID := f.monitorID(configs.board)
	gone := f.endpointGone(&l, configs, id)
	if rpc != nil {
		rpc.conn.Close()
	}
	stop()
	f.receipt["fresh_stop"] = f.assertStopped(configs, gone)
	f.persist()

	// Resume through restoreSession with the saved record. --pitput is explicit.
	resumeArgs := []string{"--resume", id, "--pitput"}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()
	ro, rl, err := f.launch(ctx2, resumeArgs)
	if err != nil {
		t.Fatal("resume preparation failed:", err)
	}
	if ro.Resume != id || !ro.PitputTargeted || !slices.Equal(ro.PitputFollowThreads, []string{targetedFollowed}) ||
		ro.AgentIdentity != f.identity || !slices.Equal(ro.PitputBoards, []string{targetedBoard}) ||
		!rl.pitputRuntime.targeted || !slices.Equal(rl.pitputRuntime.followed, []string{targetedFollowed}) {
		t.Fatal("restored launch options lost the targeted selection")
	}
	stopResumed, resumedRPC, resumedID := f.start(&rl, id)
	defer stopResumed()
	if resumedID != id {
		t.Fatal("resume bound a different native session")
	}
	resumedConfigs := f.configs(&rl, id)
	if resumedConfigs != configs {
		t.Fatal("resume moved the bridge configuration")
	}
	resumedEntry := f.assertConfigs("resume", resumedConfigs, id)
	resumePhases := map[string]any{"options_targeted": ro.PitputTargeted, "options_followed_count": len(ro.PitputFollowThreads),
		"restored_followed_matches": true, "selection_identical": reflect.DeepEqual(freshEntry, resumedEntry)}
	f.receipt["resume"] = resumePhases
	f.persist()
	if !reflect.DeepEqual(freshEntry, resumedEntry) {
		t.Fatal("regenerated bridge selection differs after resume")
	}
	resumeQuiet := f.quiet(&rl, "resume", resumedConfigs)
	f.phaseDone(resumePhases, "resume-B", "quiet", resumeQuiet)
	f.phaseDone(resumePhases, "resume-consumers", "consumers", f.assertSingleConsumer(&rl, resumedConfigs, monitorID))
	// Leave this event pending: the refused policy change below must preserve it.
	resumeFollowed := f.delivered(&rl, "resume-C", resumedConfigs, resumeQuiet["max_quiet_seq"].(int64), false)
	f.phaseDone(resumePhases, "resume-C", "followed", resumeFollowed)
	f.assertQuietNeverDelivered()
	resumedGone := f.endpointGone(&rl, resumedConfigs, id)
	if resumedRPC != nil {
		resumedRPC.conn.Close()
	}
	stopResumed()
	f.receipt["resume_stop"] = f.assertStopped(resumedConfigs, resumedGone)
	f.persist()
	f.receipt["negative"] = f.refusedPolicyChange(store, id, resumedConfigs, resumeFollowed["seq"].(int64))
	f.receipt["phases_completed"] = append(f.receipt["phases_completed"].([]string), "negative")
}

func (f *targetedFixture) phaseDone(group map[string]any, phase, key string, result map[string]any) {
	group[key] = result
	f.receipt["phases_completed"] = append(f.receipt["phases_completed"].([]string), phase)
	f.persist()
}

func (f *targetedFixture) setup() {
	t := f.t
	home, err := filepath.EvalSymlinks(os.Getenv("HOME"))
	if err != nil {
		t.Fatal(err)
	}
	f.home, f.project = home, filepath.Join(home, "project")
	for _, dir := range []string{f.project, filepath.Join(home, "codex"), filepath.Join(home, "cache/hrx")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("HRX_FIXTURE_KEY", "fixture-only")
	// The production launch resolves its project from the working directory.
	t.Chdir(f.project)
	if f.project, err = filepath.EvalSymlinks(f.project); err != nil {
		t.Fatal(err)
	}
	f.gateway = newTargetedGateway(f.harness)
	t.Cleanup(f.gateway.server.Close)
	if f.cli, err = exec.LookPath("pitput"); err != nil {
		t.Fatal("Pitput CLI missing:", err)
	}
	capability := catalog.Capability{WireAPI: "chat_completions"}
	f.settings = Settings{Harness: "opencode", ProviderName: "litellm", DefaultModel: "fixture",
		CachePath: filepath.Join(home, "cache/hrx/fixture.json"), Binding: "fixture",
		Provider: Provider{BaseURL: f.gateway.server.URL + "/v1"}}
	if f.harness == "codex" {
		capability = catalog.Capability{WireAPI: "responses", Reasoning: []string{"low"}, DefaultReasoning: "low"}
		f.settings.Harness, f.settings.ProviderName = "", "fixture"
		f.settings.Provider = Provider{BaseURL: f.gateway.server.URL + "/v1", WireAPI: "responses", EnvKey: "HRX_FIXTURE_KEY"}
		f.codexCatalog = filepath.Join(home, "cache/hrx/codex-fixture.json")
		if err := os.WriteFile(f.codexCatalog, []byte(`{"models":[{"slug":"fixture","base_instructions":"HRX targeted fixture"}]}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.snapshot = catalog.Snapshot{Version: 1, Binding: "fixture", RefreshedAt: time.Now(), GeneratedAt: time.Now(),
		Models: []catalog.Model{{ID: "fixture", Route: "fixture", Harnesses: map[string]catalog.Capability{f.harness: capability}}}}
	f.receipt["identity"] = f.identity
	f.receipt["started_at_ms"] = time.Now().UnixMilli()
	f.receipt["helper_level"] = []string{"LoadSettings and gateway catalog refresh replaced by fixture Settings/Snapshot",
		"GenerateCodexCatalog not run; Codex prompt injection writes a fixture catalog not passed to Codex",
		"execHarness supervision not run: no harness TUI; the Codex thread is bound by the test client"}
}

// probe fails, never skips, when the installed Pitput cannot select targeted events.
func (f *targetedFixture) probe() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, f.cli, "monitor", "--help").CombinedOutput()
	supported := err == nil && strings.Contains(string(output), "--follow-thread")
	f.receipt["capability_probe"] = map[string]any{"monitor_help_ok": err == nil, "follow_thread_option": supported}
	if !supported {
		f.t.Fatal("BLOCKED pitput_targeted_unsupported: installed Pitput monitor CLI lacks --follow-thread")
	}
}

// launch mirrors run.go's codex branch and runOtherHarness up to execHarness.
func (f *targetedFixture) launch(ctx context.Context, args []string) (LaunchOptions, Launch, error) {
	o, err := prepareLaunchOptions(f.harness, args)
	if err != nil {
		return o, Launch{}, err
	}
	if err := validateProfileRoute(f.settings, f.snapshot, o); err != nil {
		return o, Launch{}, err
	}
	var l Launch
	if f.harness == "codex" {
		l, err = BuildLaunch(f.settings, f.snapshot, o)
	} else {
		l, err = buildOtherLaunch(f.settings, f.snapshot, o, "fixture-key")
	}
	if err != nil {
		return o, l, err
	}
	attachProfile(o, &l)
	s := f.settings
	if f.harness == "codex" {
		// Only Codex prompt injection reads this; the launch args never reference it.
		s.GeneratedCatalogPath = f.codexCatalog
	}
	var diagnostics bytes.Buffer
	if err := prepareYesod(ctx, s, o, &l, &diagnostics); err != nil {
		return o, l, err
	}
	if err := prepareSessionCapture(ctx, o, &l, &diagnostics); err != nil {
		return o, l, err
	}
	return o, l, nil
}

func targetedUnsupported(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "invalid_bridge_agent") || strings.Contains(err.Error(), "invalid_bridge_selection"))
}

// start follows execHarness: the Pitput runtime first, then the session record is
// persisted the way superviseHarness starts the harness. Codex then needs its
// loaded root thread, which the test client starts or resumes on the private server.
func (f *targetedFixture) start(l *Launch, resume string) (func(), *pitputRPC, string) {
	t := f.t
	receipt := filepath.Join(f.home, "receipt-"+f.harness)
	_ = os.Remove(receipt)
	stop, err := startPitputRuntime(l, receipt, io.Discard)
	if targetedUnsupported(err) {
		t.Fatalf("BLOCKED pitput_targeted_unsupported: bridge configuration rejected (%v)", err)
	}
	if err != nil {
		t.Fatal("Pitput runtime failed:", err)
	}
	if err := l.startWithSession(func() error { return nil }); err != nil {
		stop()
		t.Fatal(err)
	}
	if f.harness == "opencode" {
		f.nativeID = l.pendingSession.NativeID
		return stop, nil, f.nativeID
	}
	if len(l.Args) < 2 || l.Args[0] != "--remote" || slices.Contains(l.Args, "--no-daemon") {
		stop()
		t.Fatal("Codex launch not bound to the private server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rpc, err := connectPitputServer(ctx, strings.TrimPrefix(l.Args[1], "unix://"))
	if err != nil {
		stop()
		t.Fatal(err)
	}
	params := map[string]any{"model": "fixture", "modelProvider": "fixture", "cwd": f.project, "approvalPolicy": "never", "sandbox": "danger-full-access"}
	id := resume
	if resume == "" {
		params["historyMode"] = "paginated"
		var started struct{ Thread struct{ ID string } }
		err = rpc.call("thread/start", params, &started)
		id = started.Thread.ID
	} else {
		params["threadId"] = resume
		err = rpc.call("thread/resume", params, nil)
	}
	if err != nil || id == "" {
		// Bootstrap rejection closes the observer transport. Preserve the
		// controller's classified cause before stop cancels its reporter.
		select {
		case cause := <-l.sidecarErrors:
			t.Log("native session admission failed:", cause)
		case <-time.After(time.Second):
		}
		rpc.conn.Close()
		stop()
		t.Fatal("native Codex thread start/resume failed:", err)
	}
	// The runtime writes the receipt only after saving the session binding.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if data, _ := os.ReadFile(receipt); string(data) == id {
			break
		}
		f.sidecar(l)
		if time.Now().After(deadline) {
			t.Fatal("Pitput runtime did not bind the loaded Codex thread")
		}
	}
	f.nativeID = id
	return stop, rpc, id
}

func (f *targetedFixture) sidecar(l *Launch) {
	select {
	case err := <-l.sidecarErrors:
		if targetedUnsupported(err) {
			f.t.Fatalf("BLOCKED pitput_targeted_unsupported: bridge configuration rejected (%v)", err)
		}
		f.t.Fatal("Pitput sidecar failed:", err)
	default:
	}
}

func (f *targetedFixture) tryStatus(config string) (targetedStatus, error) {
	return f.tryStatusBy(config, time.Now().Add(20*time.Second))
}

// tryStatusBy bounds the status command by the caller's absolute budget (at most 20 s).
func (f *targetedFixture) tryStatusBy(config string, budget time.Time) (targetedStatus, error) {
	if limit := time.Now().Add(20 * time.Second); budget.After(limit) {
		budget = limit
	}
	ctx, cancel := context.WithDeadline(context.Background(), budget)
	defer cancel()
	data, err := exec.CommandContext(ctx, f.cli, "bridge", "status", "--config", config).Output()
	var s targetedStatus
	if err == nil {
		err = json.Unmarshal(data, &s)
	}
	if err == nil && len(s.Agents) != 1 {
		err = errors.New("expected exactly one bridge agent")
	}
	return s, err
}

func (f *targetedFixture) status(config string) targetedStatus {
	s, err := f.tryStatus(config)
	if err != nil {
		f.t.Fatal("bridge status unavailable:", err)
	}
	return s
}

func (f *targetedFixture) configs(l *Launch, id string) targetedConfigs {
	t := f.t
	root := filepath.Join(os.Getenv("XDG_STATE_HOME"), "hrx/pitput/*", id)
	for deadline := time.Now().Add(45 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		board, _ := filepath.Glob(filepath.Join(root, targetedBoard, "bridge.toml"))
		dm, _ := filepath.Glob(filepath.Join(root, pitputDMKey, "bridge.toml"))
		if len(board) == 1 && len(dm) == 1 {
			// The runtime prepares and starts bridges after writing their configs.
			boardStatus, boardErr := f.tryStatus(board[0])
			dmStatus, dmErr := f.tryStatus(dm[0])
			if boardErr == nil && dmErr == nil && boardStatus.Running && dmStatus.Running {
				return targetedConfigs{board: board[0], dm: dm[0]}
			}
		}
		f.sidecar(l)
		if time.Now().After(deadline) {
			t.Fatalf("bridges not running (board configs=%d, DM configs=%d)", len(board), len(dm))
		}
	}
}

func targetedConfig(t *testing.T, path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := toml.Unmarshal(data, &config); err != nil {
		t.Fatal("invalid bridge config", err)
	}
	return config
}

func targetedAgent(t *testing.T, path string) map[string]any {
	agents, _ := targetedConfig(t, path)["agents"].([]any)
	if len(agents) != 1 {
		t.Fatal("bridge config must contain exactly one agent")
	}
	agent, ok := agents[0].(map[string]any)
	if !ok {
		t.Fatal("invalid bridge agent entry")
	}
	return agent
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// assertConfigs checks the generated selection on disk: the board entry is targeted
// with exactly the followed topic; the DM bridge is a separate entry without selection.
func (f *targetedFixture) assertConfigs(label string, configs targetedConfigs, id string) map[string]any {
	t := f.t
	board := targetedAgent(t, configs.board)
	followed, _ := board["followed_threads"].([]any)
	if !slices.Equal(sortedKeys(board), []string{"agent_id", "board", "followed_threads", "targeted", "thread_id"}) ||
		board["targeted"] != true || len(followed) != 1 || followed[0] != targetedFollowed ||
		board["agent_id"] != f.identity || board["board"] != targetedBoard || board["thread_id"] != id {
		t.Fatalf("board bridge selection is wrong: keys=%v", sortedKeys(board))
	}
	dm := targetedAgent(t, configs.dm)
	if !slices.Equal(sortedKeys(dm), []string{"agent_id", "source", "thread_id"}) || dm["source"] != "dm" ||
		dm["agent_id"] != f.identity || dm["thread_id"] != id {
		t.Fatalf("DM bridge must be a separate entry without selection keys: keys=%v", sortedKeys(dm))
	}
	if configs.board == configs.dm {
		t.Fatal("DM and board bridges share a config")
	}
	f.receipt["bridge_config_"+label] = map[string]any{"board_keys": sortedKeys(board), "board_targeted": board["targeted"],
		"board_followed_count": len(followed), "board_followed_matches": true,
		"dm_keys": sortedKeys(dm), "dm_has_selection_keys": dm["targeted"] != nil || dm["followed_threads"] != nil}
	return board
}

// settle waits for a stable request count (>= 5 s) with both bridges running and
// nothing pending. Bridge liveness itself is proven later by the seq barrier.
func (f *targetedFixture) settle(l *Launch, configs targetedConfigs) targetedBaseline {
	t := f.t
	last, since := f.gateway.count(), time.Now()
	for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(250 * time.Millisecond) {
		f.sidecar(l)
		if count := f.gateway.count(); count != last {
			last, since = count, time.Now()
		}
		if time.Since(since) >= 5*time.Second {
			board, dm := f.status(configs.board), f.status(configs.dm)
			idle := board.Running && dm.Running && board.Agents[0].Pending == nil && dm.Agents[0].Pending == nil
			if idle && f.gateway.count() == last {
				now := time.Now()
				return targetedBaseline{count: last, at: now, stableMs: now.Sub(since).Milliseconds()}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("baseline did not settle: request count changing, bridge stopped or event pending")
		}
	}
}

// ready/posted form the file handshake with the Python harness. Each file is
// written atomically; every wait is bounded.
func (f *targetedFixture) ready(phase string, fields map[string]any) {
	data, _ := json.Marshal(fields)
	path := filepath.Join(f.handshake, f.harness+"-"+phase+".ready")
	if err := os.WriteFile(path+".tmp", data, 0600); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		f.t.Fatal(err)
	}
}

func (f *targetedFixture) posted(l *Launch, phase string) targetedPosted {
	path := filepath.Join(f.handshake, f.harness+"-"+phase+".posted")
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if data, err := os.ReadFile(path); err == nil {
			var posted targetedPosted
			if err := json.Unmarshal(data, &posted); err != nil || len(posted.Posts) == 0 || posted.PostedAtMs <= 0 {
				f.t.Fatalf("invalid harness handshake for %s", phase)
			}
			return posted
		}
		f.sidecar(l)
		if time.Now().After(deadline) {
			f.t.Fatalf("harness did not post %s", phase)
		}
	}
}

// quiet measures B: after a settled baseline the harness posts unrelated and
// self-authored activity; for >= 20 s no request may reach the model and no event
// may become pending. Every sample's observation is recorded, not assumed.
func (f *targetedFixture) quiet(l *Launch, generation string, configs targetedConfigs) map[string]any {
	t := f.t
	base := f.settle(l, configs)
	f.ready(generation+"-B", map[string]any{"baseline_requests": base.count})
	readyCount := f.gateway.count()
	posted := f.posted(l, generation+"-B")
	injectionCount := f.gateway.count()
	injected := time.UnixMilli(posted.PostedAtMs)
	var maxSeq int64
	seqs := map[string]int64{}
	for _, post := range posted.Posts {
		if post.Seq <= 0 || (post.Kind != "unrelated" && post.Kind != "self_followed") {
			t.Fatalf("invalid quiet post %q", post.Kind)
		}
		seqs[post.Kind] = post.Seq
		maxSeq = max(maxSeq, post.Seq)
		f.quietPaths = append(f.quietPaths, post.Path)
	}
	if len(seqs) != 2 {
		t.Fatal("harness must post both unrelated and self-authored activity")
	}
	var boardPending, dmPending, boardErrors []bool
	var counts []int
	var times []time.Time
	for {
		at := time.Now()
		f.sidecar(l)
		board, dm := f.status(configs.board), f.status(configs.dm)
		count := f.gateway.count()
		counts, times = append(counts, count), append(times, at)
		boardPending = append(boardPending, board.Agents[0].Pending != nil)
		dmPending = append(dmPending, dm.Agents[0].Pending != nil)
		boardErrors = append(boardErrors, board.Agents[0].LastError != nil)
		if count != base.count || board.Agents[0].Pending != nil || dm.Agents[0].Pending != nil {
			t.Fatalf("%s unrelated/self-authored activity notified: requests %d -> %d, board pending=%t, DM pending=%t",
				generation, base.count, count, board.Agents[0].Pending != nil, dm.Agents[0].Pending != nil)
		}
		if !board.Running || !dm.Running {
			t.Fatalf("%s bridge stopped during the quiet window", generation)
		}
		if len(times) >= 10 && at.Sub(injected) >= targetedWindow {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	var maxGap time.Duration
	for i := 1; i < len(times); i++ {
		maxGap = max(maxGap, times[i].Sub(times[i-1]))
	}
	if maxGap > 5*time.Second {
		t.Fatalf("quiet sampling too sparse (max gap %s)", maxGap)
	}
	offset := posted.PostedAtMs - base.at.UnixMilli()
	if offset <= 0 {
		t.Fatal("injection preceded the settled baseline")
	}
	return map[string]any{"unrelated_seq": seqs["unrelated"], "self_authored_followed_seq": seqs["self_followed"],
		"max_quiet_seq": maxSeq, "baseline_settled_at_ms": base.at.UnixMilli(), "baseline_stable_ms": base.stableMs,
		"injected_at_ms": posted.PostedAtMs, "injection_after_baseline_ms": offset,
		"requests_at_baseline": base.count, "requests_at_ready": readyCount, "requests_when_injection_observed": injectionCount,
		"requests_at_window_end": counts[len(counts)-1], "post_injection_samples": len(times),
		"max_sample_gap_ms": maxGap.Milliseconds(), "observed_window_ms": times[len(times)-1].Sub(injected).Milliseconds(),
		"board_pending_per_sample": boardPending, "dm_pending_per_sample": dmPending,
		"board_last_error_present_per_sample": boardErrors}
}

// delivered measures C and D. Delivery is proven only when a notice in a model
// request binds (bindTargetedNotice) to the exact posted event held pending by the board
// bridge; the post's seq must also exceed the barrier.
func (f *targetedFixture) delivered(l *Launch, phase string, configs targetedConfigs, barrier int64, ack bool) map[string]any {
	t := f.t
	start := f.gateway.count()
	f.ready(phase, map[string]any{"requests": start})
	posted := f.posted(l, phase)
	post := posted.Posts[0]
	want := map[string]string{"C": "followed_no_mention", "D": "mention_unfollowed"}[phase[len(phase)-1:]]
	if len(posted.Posts) != 1 || post.Kind != want || post.Path == "" {
		t.Fatalf("invalid %s handshake", phase)
	}
	if post.Seq <= barrier {
		t.Fatalf("%s seq %d does not pass barrier %d", phase, post.Seq, barrier)
	}
	binding, index := f.awaitNotice(l, phase, configs, configs.board, "board", post, start)
	observed := time.Now()
	reasons := map[string][]string{"C": {"new_activity", "followed_thread"}, "D": {"mention"}}[phase[len(phase)-1:]]
	// A legacy binding is only possible when the bridge kept no attention metadata.
	if binding.Form == "compact" && (binding.Sender != targetedController || !slices.Contains(reasons, binding.Reason)) {
		t.Fatalf("%s notice has unexpected sender or reason (%s)", phase, targetedCode(binding.Reason))
	}
	pending := f.waitPending(configs.board, post.Seq)
	if pending.Path != post.Path || pending.EventKey != binding.EventKey {
		t.Fatalf("%s pending event differs from the delivered post", phase)
	}
	result := map[string]any{"seq": post.Seq, "barrier_seq": barrier, "seq_after_barrier": post.Seq > barrier,
		"delivered": true, "notice_form": binding.Form, "notice_reason": targetedCode(binding.Reason),
		"handle_bound_to_event": true, "primer_included": binding.Primer, "request_index": index, "requests_before": start,
		"delivery_ms_after_post": observed.UnixMilli() - posted.PostedAtMs, "pending_seq_matched": true, "acked": false}
	if ack {
		f.ack(configs.board, pending.EventKey)
		result["acked"] = true
	}
	return result
}

type targetedBinding struct {
	Form, Handle, Sender, Reason, EventKey, Path string
	Seq                                          int64
	Primer                                       bool
}

// awaitNotice waits until the existing 45 s phase deadline for a model request
// whose notice binds to the expected event. Transient misses are retried; a
// definitive mismatch fails at once. Attempts and miss codes go to the receipt.
func (f *targetedFixture) awaitNotice(l *Launch, phase string, configs targetedConfigs, config, source string, post targetedPost, start int) (targetedBinding, int) {
	want := targetedExpect{Source: source, Identity: f.identity, Sender: targetedController, Config: config, Post: post}
	deadline := time.Now().Add(45 * time.Second)
	binding, index, stats, err := awaitTargetedNotice(func() []string { return f.gateway.since(start) }, f.bound,
		func(n targetedNotice) (targetedBinding, string, bool) {
			return bindTargetedNotice(n, want, fixtureSpool{f, deadline})
		},
		time.Now, time.Sleep, deadline, func() { f.sidecar(l) })
	attempts, _ := f.receipt["binding_attempts"].(map[string]any)
	if attempts == nil {
		attempts = map[string]any{}
		f.receipt["binding_attempts"] = attempts
	}
	attempts[phase] = map[string]any{"attempts": stats.Attempts, "misses": stats.Misses, "definitive": stats.Definitive}
	if err != nil {
		f.diagnose(l, phase, configs, source, post, start, stats.Misses)
		f.t.Fatalf("%s (seq %d) was not delivered: %v (%s)", phase, post.Seq, err, targetedCode(stats.Definitive))
	}
	return binding, start + index
}

var (
	errTargetedNoticeTimeout    = errors.New("no model request carried a notice bound to the expected event")
	errTargetedNoticeDefinitive = errors.New("a notice definitively mismatched the expected event")
)

const targetedBindInterval = 250 * time.Millisecond

type targetedAwaitStats struct {
	Attempts   int
	Misses     map[string]int
	Definitive string
}

// awaitTargetedNotice polls request bodies at a bounded rate, retrying every seen,
// unbound notice until deadline. "other_source" notices are skipped; any other
// definitive miss returns immediately. The deadline is absolute: expiry is checked
// before every bind attempt, a bind that completes at or after the deadline is a
// deadline failure even if it succeeded, and sleeps never run past the deadline.
func awaitTargetedNotice(bodies func() []string, bound map[string]bool, bind func(targetedNotice) (targetedBinding, string, bool),
	now func() time.Time, sleep func(time.Duration), deadline time.Time, check func()) (targetedBinding, int, targetedAwaitStats, error) {
	stats := targetedAwaitStats{Misses: map[string]int{}}
	ignored := map[string]bool{}
	expired := func(code string) (targetedBinding, int, targetedAwaitStats, error) {
		stats.Definitive = code
		return targetedBinding{}, -1, stats, errTargetedNoticeTimeout
	}
	for {
		if !now().Before(deadline) {
			return expired("deadline_exceeded")
		}
		tried := map[string]bool{}
		for i, body := range bodies() {
			for _, notice := range targetedNotices(body) {
				key := notice.key()
				if bound[key] || ignored[key] || tried[key] {
					continue
				}
				if !now().Before(deadline) {
					return expired("deadline_exceeded")
				}
				tried[key] = true
				stats.Attempts++
				binding, miss, definitive := bind(notice)
				late := !now().Before(deadline)
				if miss == "" {
					if late {
						// Proof that completes after the deadline is not delivery in time.
						return expired("deadline_exceeded_during_bind")
					}
					binding.Primer = strings.Contains(body, targetedPrimer)
					bound[key] = true
					return binding, i, stats, nil
				}
				stats.Misses[miss]++
				switch {
				case miss == "other_source":
					ignored[key] = true
				case definitive:
					stats.Definitive = miss
					return targetedBinding{}, -1, stats, errTargetedNoticeDefinitive
				case late:
					// A transient miss (including a budget-bounded status timeout) is
					// retryable only while time remains.
					return expired("deadline_exceeded_during_bind")
				}
			}
		}
		check()
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			return expired("deadline_exceeded")
		}
		sleep(min(targetedBindInterval, remaining))
	}
}

// targetedExpect is the exact event a notice must resolve to.
type targetedExpect struct {
	Source, Identity, Sender, Config string
	Post                             targetedPost
}

type targetedPendingView struct {
	Seq                      int64
	Path, EventKey, StateDir string
}

// targetedSpool reads one bridge's private state. ReadJSON and Route wrap
// fs.ErrNotExist when the file is (not yet) present.
type targetedSpool interface {
	Pending(config string) (*targetedPendingView, error)
	ReadJSON(path string, target any) error
	Route(route string) (agentID, configPath string, err error)
}

// bindTargetedNotice resolves a notice to the exact expected event the bridge for
// want.Config holds pending. It returns a fixed miss code and whether that miss is
// definitive (wrong event, identity, handle or form) or transient (state not yet
// visible). Rules, for Pitput 01745f4:
//   - the pending event is the posted one: board seq, path (and event key when the
//     post returned one); DM message id and event key from the DM send;
//   - compact: attention-metadata.json holds exactly that event (a DM also its
//     sender and conversation) with a header equal to the notice; the handle
//     suffix is Pitput's _event_tag of that event; attention-presentation.json
//     holds this handle for this identity; the route record names this bridge;
//   - legacy "Message: PATH" counts only when the bridge has no attention
//     metadata for the event (explicitly absent); with metadata it is a failure.
func bindTargetedNotice(n targetedNotice, want targetedExpect, spool targetedSpool) (targetedBinding, string, bool) {
	miss := func(code string, definitive bool) (targetedBinding, string, bool) {
		return targetedBinding{}, code, definitive
	}
	if n.Source != want.Source {
		return miss("other_source", false)
	}
	if n.Form == "compact" && n.Sender != want.Sender {
		return miss("wrong_sender", true)
	}
	post := want.Post
	if want.Source == "dm" && (post.Path == "" || post.EventKey == "" || post.Conversation == "") {
		return miss("expectation_incomplete", true)
	}
	pending, err := spool.Pending(want.Config)
	if err != nil {
		return miss("status_unavailable", false)
	}
	if pending == nil {
		return miss("nothing_pending", false)
	}
	if pending.Path != post.Path || (post.EventKey != "" && pending.EventKey != post.EventKey) ||
		(want.Source == "board" && pending.Seq != post.Seq) {
		return miss("pending_is_another_event", true)
	}
	binding := targetedBinding{Form: n.Form, EventKey: pending.EventKey, Path: pending.Path, Seq: pending.Seq}
	var metadata struct {
		Event  map[string]any
		Header struct{ Source, Sender, Reason, Message, Conversation string }
	}
	err = spool.ReadJSON(filepath.Join(pending.StateDir, "attention-metadata.json"), &metadata)
	absent := errors.Is(err, fs.ErrNotExist)
	if err != nil && !absent {
		return miss("metadata_unavailable", false)
	}
	if n.Form == "legacy" {
		if !absent {
			return miss("legacy_with_compact_metadata", true)
		}
		if n.Path != pending.Path {
			return miss("legacy_path_mismatch", true)
		}
		return binding, "", false
	}
	if absent {
		return miss("metadata_unavailable", false)
	}
	event := metadata.Event
	if fmt.Sprint(event["seq"]) != strconv.FormatInt(pending.Seq, 10) || event["path"] != pending.Path || event["event_key"] != pending.EventKey {
		return miss("metadata_not_pending_event", true)
	}
	header := metadata.Header
	if header.Source != n.Source || header.Sender != n.Sender || header.Reason != n.Reason {
		return miss("header_mismatch", true)
	}
	if want.Source == "dm" && (event["from"] != want.Sender || event["conversation"] != post.Conversation ||
		header.Message != post.Path || n.Message != post.Path ||
		header.Conversation != post.Conversation || n.Conversation != post.Conversation) {
		return miss("dm_not_this_message", true)
	}
	tag, err := targetedEventTag(event)
	if err != nil || n.Handle[15:] != tag {
		return miss("handle_not_this_event", true)
	}
	var receipt struct{ Handle, Owner string }
	if err := spool.ReadJSON(filepath.Join(pending.StateDir, "attention-presentation.json"), &receipt); err != nil {
		return miss("presentation_unavailable", false)
	}
	if receipt.Owner != want.Identity {
		return miss("wrong_identity", true)
	}
	if receipt.Handle != n.Handle {
		return miss("presentation_not_this_handle", true)
	}
	agentID, configPath, err := spool.Route(n.Handle[3:15])
	if err != nil {
		return miss("route_unavailable", false)
	}
	if agentID != want.Identity {
		return miss("wrong_identity", true)
	}
	if configPath != want.Config {
		return miss("route_not_this_bridge", true)
	}
	binding.Handle, binding.Sender, binding.Reason = n.Handle, n.Sender, n.Reason
	return binding, "", false
}

// fixtureSpool reads the live bridge through `pitput bridge status` and its files;
// the status command never outlives the phase deadline.
type fixtureSpool struct {
	f        *targetedFixture
	deadline time.Time
}

func (s fixtureSpool) Pending(config string) (*targetedPendingView, error) {
	status, err := s.f.tryStatusBy(config, s.deadline)
	if err != nil {
		return nil, err
	}
	agent := status.Agents[0]
	if agent.Pending == nil {
		return nil, nil
	}
	return &targetedPendingView{Seq: agent.Pending.Seq, Path: agent.Pending.Path, EventKey: agent.Pending.EventKey,
		StateDir: filepath.Dir(agent.StateFile)}, nil
}

func (fixtureSpool) ReadJSON(path string, target any) error { return targetedReadJSON(path, target) }

// Route finds Pitput's route record (~/.config/pitput/attention-routes/IDENTITY/
// ROUTE.json, attention_helpers.register_handle).
func (s fixtureSpool) Route(route string) (string, string, error) {
	root := filepath.Join(s.f.home, ".config/pitput/attention-routes")
	var agentID, configPath string
	found := false
	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || found || entry.IsDir() || entry.Name() != route+".json" {
			return nil
		}
		var record struct {
			AgentID    string `json:"agent_id"`
			ConfigPath string `json:"config_path"`
		}
		if targetedReadJSON(path, &record) == nil {
			found, agentID, configPath = true, record.AgentID, record.ConfigPath
			if resolved, err := filepath.EvalSymlinks(configPath); err == nil {
				configPath = resolved
			}
		}
		return nil
	})
	if !found {
		return "", "", fmt.Errorf("route record: %w", errors.Join(fs.ErrNotExist, walkErr))
	}
	return agentID, configPath, nil
}

// diagnose records bounded, content-free evidence for a delivery timeout: bridge
// and spool state, processes, request counts, sidecar error categories, thread
// binding, and a server-side selection probe that bypasses the model.
func (f *targetedFixture) diagnose(l *Launch, phase string, configs targetedConfigs, source string, post targetedPost, start int, misses map[string]int) {
	counts := map[string]int{}
	for _, body := range f.gateway.since(start) {
		for _, notice := range targetedNotices(body) {
			counts[notice.Form+"_"+notice.Source]++
		}
	}
	bridges := map[string]any{}
	for name, config := range map[string]string{"board": configs.board, "dm": configs.dm} {
		entry := map[string]any{}
		status, err := f.tryStatus(config)
		entry["status_ok"] = err == nil
		if err == nil {
			agent := status.Agents[0]
			entry["running"] = status.Running
			entry["status"] = targetedCode(agent.Status)
			entry["pending_present"] = agent.Pending != nil
			if agent.Pending != nil && name == source {
				entry["pending_seq_matches"] = post.Seq == 0 || agent.Pending.Seq == post.Seq
				entry["pending_path_matches"] = post.Path == "" || agent.Pending.Path == post.Path
			}
			if agent.LastError != nil {
				entry["last_error"] = targetedCode(*agent.LastError)
			}
			if protocol, ok := agent.Protocol["status"].(string); ok {
				entry["protocol_status"] = targetedCode(protocol)
			}
			var state struct {
				HandledThrough json.Number `json:"handled_through"`
			}
			if targetedReadJSON(agent.StateFile, &state) == nil {
				entry["handled_through"] = state.HandledThrough
			}
			var cursor struct {
				MonitorID string `json:"monitor_id"`
				Spec      struct {
					After json.Number `json:"after"`
				}
			}
			if targetedReadJSON(agent.CursorFile, &cursor) == nil {
				entry["cursor_monitor_id_present"] = cursor.MonitorID != ""
				entry["cursor_after"] = cursor.Spec.After
			}
			_, metadataErr := os.Stat(filepath.Join(filepath.Dir(agent.StateFile), "attention-metadata.json"))
			_, presentationErr := os.Stat(filepath.Join(filepath.Dir(agent.StateFile), "attention-presentation.json"))
			entry["attention_metadata_present"] = metadataErr == nil
			entry["attention_presentation_present"] = presentationErr == nil
		}
		agents, _ := targetedConfig(f.t, config)["agents"].([]any)
		if len(agents) == 1 {
			if agent, ok := agents[0].(map[string]any); ok {
				entry["config_thread_matches_bound_session"] = agent["thread_id"] == f.nativeID
			}
		}
		bridges[name] = entry
	}
	boardBridges, dmBridges, boardMonitors, dmMonitors := f.consumers(configs)
	var sidecar []string
	for drained := false; !drained; {
		select {
		case err := <-l.sidecarErrors:
			code := "other"
			if m := regexp.MustCompile(`\(([a-z_]{1,64})\)`).FindStringSubmatch(err.Error()); m != nil {
				code = m[1]
			} else if strings.Contains(err.Error(), "exited") {
				code = "sidecar_exited"
			}
			sidecar = append(sidecar, code)
		default:
			drained = true
		}
	}
	missCodes := map[string]int{}
	for code, count := range misses {
		missCodes[targetedCode(code)] = count
	}
	diagnostic := map[string]any{"phase": phase, "post_seq": post.Seq, "requests_since_phase_start": f.gateway.count() - start,
		"requests_total": f.gateway.count(), "notices_since_phase_start": counts, "bind_misses": missCodes,
		"bridges": bridges, "processes": map[string]int{"board_bridges": boardBridges, "dm_bridges": dmBridges,
			"board_monitors": boardMonitors, "dm_monitors": dmMonitors},
		"sidecar_error_codes": sidecar, "session_bound": f.nativeID != ""}
	if source == "board" {
		diagnostic["selection_probe"] = f.selectionProbe(post.Seq)
	}
	diagnostics, _ := f.receipt["diagnostics"].(map[string]any)
	if diagnostics == nil {
		diagnostics = map[string]any{}
		f.receipt["diagnostics"] = diagnostics
	}
	diagnostics[phase] = diagnostic
	f.persist()
	data, _ := json.Marshal(diagnostic)
	f.t.Logf("delivery diagnostics: %s", data)
}

// selectionProbe asks the Pitput server, without the model or the bridge, whether
// this identity's targeted spec selects seq. It uses its own temporary cursor
// file, so Pitput registers a separate monitor id (board_client.monitor_state);
// the bridge's monitor, cursor and pending state are untouched. Diagnostic only.
func (f *targetedFixture) selectionProbe(seq int64) map[string]any {
	directory, err := os.MkdirTemp("", "hrx-selection-probe-")
	if err != nil {
		return map[string]any{"ran": false}
	}
	defer os.RemoveAll(directory)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, f.cli, "--agent", f.identity, "monitor", "/b/"+targetedBoard,
		"--mention", f.identity, "--follow-thread", targetedFollowed, "--exclude-author", f.identity,
		"--cursor-file", filepath.Join(directory, "cursor.json"), "--after", strconv.FormatInt(seq-1, 10),
		"--timeout", "5", "--once").Output()
	var event struct{ Seq int64 }
	parsed := err == nil && (len(bytes.TrimSpace(output)) == 0 || json.Unmarshal(output, &event) == nil)
	return map[string]any{"ran": err == nil, "parsed": parsed, "selected_expected_seq": event.Seq == seq,
		"returned_other_seq": event.Seq != 0 && event.Seq != seq}
}

func (f *targetedFixture) waitPending(config string, seq int64) struct {
	EventKey string `json:"event_key"`
	Path     string
	Seq      int64
} {
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if pending := f.status(config).Agents[0].Pending; pending != nil {
			if seq > 0 && pending.Seq != seq {
				f.t.Fatalf("bridge holds seq %d, expected %d", pending.Seq, seq)
			}
			return *pending
		}
		if time.Now().After(deadline) {
			f.t.Fatal("delivered event is not durably pending")
		}
	}
}

func (f *targetedFixture) ack(config, key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, f.cli, "bridge", "ack", "--config", config, "--agent-id", f.identity, "--event-key", key).CombinedOutput(); err != nil {
		f.t.Fatalf("handling ACK failed: %v %s", err, output)
	}
	for deadline := time.Now().Add(20 * time.Second); f.status(config).Agents[0].Pending != nil; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			f.t.Fatal("event still pending after ACK")
		}
	}
}

// directMessage measures E: a DM reaches the model through the DM bridge only, its
// notice binds to the DM bridge's pending event, and it never carries the body.
func (f *targetedFixture) directMessage(l *Launch, configs targetedConfigs) map[string]any {
	t := f.t
	start := f.gateway.count()
	f.ready("fresh-E", map[string]any{"requests": start})
	posted := f.posted(l, "fresh-E")
	sent := posted.Posts[0]
	if len(posted.Posts) != 1 || sent.Kind != "dm" || len(posted.Sentinel) < 16 ||
		!regexp.MustCompile(`^dm-[a-z2-7]{12}$`).MatchString(sent.Path) ||
		!regexp.MustCompile(`^dc-[a-z2-7]{12}$`).MatchString(sent.Conversation) || sent.EventKey == "" {
		t.Fatal("invalid DM handshake: the DM send's message id, conversation and event key are required")
	}
	// Only this exact DM (message id, conversation, event key from the send) satisfies E.
	binding, index := f.awaitNotice(l, "fresh-E", configs, configs.dm, "dm", sent, start)
	if (binding.Form == "compact" && (binding.Sender != targetedController || binding.Reason != "incoming_dm")) ||
		binding.Path != sent.Path || binding.EventKey != sent.EventKey {
		t.Fatal("DM notice is not the sent DM")
	}
	for _, body := range f.gateway.since(0) {
		if strings.Contains(body, posted.Sentinel) {
			t.Fatal("DM notification leaked the message body")
		}
	}
	if f.status(configs.board).Agents[0].Pending != nil {
		t.Fatal("DM became pending on the board bridge")
	}
	dm := f.status(configs.dm).Agents[0]
	if dm.Source != "dm" || dm.Pending == nil || dm.Pending.EventKey != sent.EventKey || dm.Pending.Path != sent.Path {
		t.Fatal("DM is not the DM bridge's pending event")
	}
	f.ack(configs.dm, binding.EventKey)
	return map[string]any{"inbox_seq": binding.Seq, "delivered": true, "via_dm_bridge": true, "notice_form": binding.Form,
		"message_matches_send": true, "event_key_matches_send": true,
		"handle_bound_to_event": true, "request_index": index, "body_leaked": false, "acked": true}
}

// assertQuietNeverDelivered: no legacy notice names a quiet post, and every
// notice ever sent to the model resolved to an expected event when it arrived.
func (f *targetedFixture) assertQuietNeverDelivered() {
	for _, body := range f.gateway.since(0) {
		for _, path := range f.quietPaths {
			if containsMessagePath(body, path) {
				f.t.Fatal("unrelated or self-authored activity reached the model")
			}
		}
		for _, notice := range targetedNotices(body) {
			if !f.bound[notice.key()] {
				f.t.Fatal("a notice that never bound to an expected event reached the model")
			}
		}
	}
}

func (f *targetedFixture) monitorID(cursorConfig string) string {
	var cursor struct {
		MonitorID string `json:"monitor_id"`
	}
	data, err := os.ReadFile(f.status(cursorConfig).Agents[0].CursorFile)
	if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.MonitorID == "" {
		f.t.Fatal("board cursor has no monitor registration", err)
	}
	return cursor.MonitorID
}

// consumers counts live bridge and monitor processes for this identity's configs.
func (f *targetedFixture) consumers(configs targetedConfigs) (boardBridges, dmBridges, boardMonitors, dmMonitors int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "ps", "-A", "-ww", "-o", "args=").Output()
	if err != nil {
		f.t.Fatal("process listing failed:", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		switch {
		case strings.Contains(line, "bridge run --config "+configs.board):
			boardBridges++
		case strings.Contains(line, "bridge run --config "+configs.dm):
			dmBridges++
		case strings.Contains(line, "--agent "+f.identity+" monitor /b/"+targetedBoard):
			boardMonitors++
		case strings.Contains(line, "--agent "+f.identity+" monitor --dm"):
			dmMonitors++
		}
	}
	return
}

// endpointGone captures, while the runtime runs, how to prove its private server is
// gone after cleanup: the Codex socket, or the OpenCode credential file and API.
func (f *targetedFixture) endpointGone(l *Launch, configs targetedConfigs, id string) func() bool {
	if f.harness == "codex" {
		socket := strings.TrimPrefix(l.Args[1], "unix://")
		return func() bool {
			_, err := os.Stat(socket)
			return os.IsNotExist(err)
		}
	}
	config := targetedConfig(f.t, configs.board)
	endpoint, _ := config["opencode_url"].(string)
	passwordFile, _ := config["opencode_password_file"].(string)
	password, err := os.ReadFile(passwordFile)
	if err != nil || endpoint == "" {
		f.t.Fatal("OpenCode bridge lacks its private server credential")
	}
	if err := openCodeRequest(context.Background(), endpoint, string(password), "GET", "/api/session/"+id, nil, nil); err != nil {
		f.t.Fatal("private OpenCode server unreachable while running:", err)
	}
	return func() bool {
		_, statErr := os.Stat(passwordFile)
		return os.IsNotExist(statErr) &&
			openCodeRequest(context.Background(), endpoint, string(password), "GET", "/api/session/"+id, nil, nil) != nil
	}
}

// assertStopped reuses the existing owned-cleanup evidence: no bridge registered as
// running, no consumer process, and the private endpoint removed.
func (f *targetedFixture) assertStopped(configs targetedConfigs, endpointGone func() bool) map[string]any {
	t := f.t
	if f.status(configs.board).Running || f.status(configs.dm).Running {
		t.Fatal("bridge remained registered as running after cleanup")
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(250 * time.Millisecond) {
		a, b, c, d := f.consumers(configs)
		if a+b+c+d == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("leftover consumers after cleanup: bridges=%d/%d monitors=%d/%d", a, b, c, d)
		}
	}
	if !endpointGone() {
		t.Fatal("private server endpoint survived cleanup")
	}
	return map[string]any{"bridges_running": false, "consumers_left": 0, "endpoint_removed": true}
}

// assertSingleConsumer proves resume added no consumer: a duplicate launch for the
// identity is refused, exactly one bridge per config runs, at most one monitor per
// source exists, and the server-side monitor registration was reused.
func (f *targetedFixture) assertSingleConsumer(l *Launch, configs targetedConfigs, monitorID string) map[string]any {
	t := f.t
	duplicate := *l
	if cleanup, err := startPitputRuntime(&duplicate, "", io.Discard); err == nil {
		cleanup()
		t.Fatal("duplicate identity accepted after resume")
	}
	samples, seenBoardMonitor := 0, false
	maxMonitors := 0
	for deadline := time.Now().Add(15 * time.Second); samples < 6 || !seenBoardMonitor; time.Sleep(500 * time.Millisecond) {
		boardBridges, dmBridges, boardMonitors, dmMonitors := f.consumers(configs)
		samples++
		if boardBridges != 1 || dmBridges != 1 || boardMonitors > 1 || dmMonitors > 1 {
			t.Fatalf("consumer count after resume: bridges=%d/%d monitors=%d/%d", boardBridges, dmBridges, boardMonitors, dmMonitors)
		}
		seenBoardMonitor = seenBoardMonitor || boardMonitors == 1
		maxMonitors = max(maxMonitors, boardMonitors)
		if time.Now().After(deadline) {
			t.Fatal("no live board monitor observed after resume")
		}
	}
	unchanged := f.monitorID(configs.board) == monitorID
	if !unchanged {
		t.Fatal("resume registered a new board monitor instead of continuing the existing one")
	}
	return map[string]any{"duplicate_launch_refused": true, "process_samples": samples, "board_bridges": 1, "dm_bridges": 1,
		"max_board_monitors": maxMonitors, "monitor_registration_unchanged": unchanged}
}

// refusedPolicyChange proves e: a resume adding a different followed topic is
// refused by Pitput and changes no cursor, pending state or session record.
func (f *targetedFixture) refusedPolicyChange(store session.Store, id string, configs targetedConfigs, pendingSeq int64) map[string]any {
	t := f.t
	if pending := f.status(configs.board).Agents[0].Pending; pending == nil || pending.Seq != pendingSeq {
		t.Fatal("stopped runtime did not retain the unhandled followed event")
	}
	var files []string
	for _, config := range []string{configs.board, configs.dm} {
		agent := f.status(config).Agents[0]
		files = append(files, agent.StateFile, agent.CursorFile)
	}
	snapshot := func() map[string][]byte {
		state := map[string][]byte{}
		for _, path := range files {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal("bridge state unreadable:", err)
			}
			state[path] = data
		}
		return state
	}
	before := snapshot()
	recordBefore, err := store.Resolve(f.harness, f.project, id)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--resume", id, "--pitput", "--pitput-follow-thread", targetedOther}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	o, l, err := f.launch(ctx, args)
	if err != nil {
		t.Fatal("changed-policy resume failed before reaching Pitput:", err)
	}
	if !slices.Equal(o.PitputFollowThreads, []string{targetedOther}) {
		t.Fatal("explicit followed topic did not override the saved selection")
	}
	stop, err := startPitputRuntime(&l, "", io.Discard)
	if err == nil {
		stop()
		t.Fatal("changed bridge selection was accepted on resume")
	}
	if !strings.Contains(err.Error(), "bridge_selection_migration_required") {
		t.Fatalf("expected bridge_selection_migration_required, got: %v", err)
	}
	after := snapshot()
	identical := reflect.DeepEqual(before, after)
	if !identical {
		t.Fatal("refused policy change modified cursor or pending state")
	}
	recordAfter, err := store.Resolve(f.harness, f.project, id)
	if err != nil || !reflect.DeepEqual(recordBefore, recordAfter) {
		t.Fatal("refused policy change modified the session record", err)
	}
	a, b, c, d := f.consumers(configs)
	if a+b+c+d != 0 {
		t.Fatal("refused policy change started a consumer")
	}
	// The pending event survived; handle it so the fixture ends clean.
	pending := f.waitPending(configs.board, pendingSeq)
	f.ack(configs.board, pending.EventKey)
	return map[string]any{"refused": true, "code": "bridge_selection_migration_required", "files_compared": len(files),
		"files_byte_identical": identical, "pending_preserved_seq": pendingSeq, "session_record_unchanged": true,
		"consumers_started": 0}
}
