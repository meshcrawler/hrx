package codexrpc

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Opted in ONLY by the existing pinned GitLab native gate. Creates a disposable
// local app-server fixture, no initialization/model RPC, thread or subscription.
// It never attaches to a live agent or uses a user's native home/credentials.
func TestWebSocketNativeControlPongWithoutAppTraffic(t *testing.T) {
	socketCI(t)
	if os.Getenv("HRX_CODEXRPC_NATIVE_TESTS") != "1" {
		t.Skip("requires the pinned native GitLab acceptance step")
	}
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal("pinned native codex prerequisite missing")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("native fixture requires the Python subreaper")
	}
	supervisor, err := filepath.Abs(filepath.Join("..", "..", "tests", "compatibility", "subreaper.py"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "s")
	home := filepath.Join(dir, "native")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel) // Runs after owned process cleanup, not before it.
	summary := filepath.Join(dir, "supervisor.json")
	cmd := exec.CommandContext(ctx, python, "-I", supervisor, "--summary", summary, "--grace", "2", "--",
		codex, "app-server", "--listen", "unix://"+socketPath)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "CODEX_HOME=" + home, "XDG_CONFIG_HOME=" + dir}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The native leader may exit before plugin-clone children. Let the existing
	// subreaper stop and reap every descendant before deleting the fixture home.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	if err := cmd.Start(); err != nil {
		t.Fatal("native fixture failed to start")
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		select {
		case <-exited:
		default:
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-exited:
			case <-time.After(8 * time.Second):
				t.Error("native fixture cleanup unverified")
				return
			}
		}
		var report struct {
			Subreaper bool `json:"subreaper"`
			Complete  bool `json:"complete"`
			Forced    bool `json:"forced"`
		}
		data, err := os.ReadFile(summary)
		if err != nil || json.Unmarshal(data, &report) != nil || !report.Subreaper || !report.Complete || report.Forced {
			t.Error("native fixture descendant cleanup was incomplete or forced")
		}
	})
	dialer := websocket.Dialer{
		HandshakeTimeout: time.Second,
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	var conn *websocket.Conn
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		conn, _, err = dialer.DialContext(ctx, "ws://localhost/", nil)
		if err == nil {
			break
		}
		select {
		case err := <-exited:
			// Restore the receipt for the cleanup owner.
			exited <- err
			t.Fatal("native fixture exited before its socket was ready")
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal("native fixture control socket did not become ready")
	}
	w := socketWire(t, conn)
	t.Cleanup(func() { _ = conn.Close() })
	read := make(chan error, 1)
	go func() {
		raw, err := w.Read(context.Background(), 256)
		if len(raw) != 0 {
			read <- ErrProtocol // No app message is expected or discarded.
		} else {
			read <- err
		}
	}()
	time.Sleep(1800 * time.Millisecond)
	select {
	case err := <-read:
		t.Fatalf("native quiet read ended before Stop: %v", err)
	default:
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	report, err := w.Stop(stopCtx)
	if err != nil || report.Cause != ErrStopped || report.Pongs < 5 ||
		!report.ReaderStopped || !report.WritersStopped || !report.HeartbeatStopped || !report.TransportClosed {
		t.Fatalf("native control liveness/cleanup: %+v %v", report, err)
	}
	if err := receive(t, read); err == nil {
		t.Fatal("native pending read did not unblock")
	}
	t.Logf("pinned native control-only pings=%d matching-pongs=%d; no subscription/coverage claim", report.Pings, report.Pongs)
}
