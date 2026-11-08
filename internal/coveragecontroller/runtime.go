package coveragecontroller

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/meshcrawler/hrx/internal/codexlaunch"
	"github.com/meshcrawler/hrx/internal/codexrpc"
	"github.com/meshcrawler/hrx/internal/coveragejournal"
)

// Runtime owns the native connection and all three downstream ingress sockets.
// Endpoints never expose that native connection. Ready reports identity only;
// every subsequent input still passes the owner's live admission fence.
type Runtime struct {
	gateway                        *gateway
	Terminal, Board, DirectMessage string
	done                           chan struct{}
	err                            error // immutable after done closes
}
type RuntimeOptions struct {
	// CombinedResumeDrain permits only journal-witnessed queued work to finish
	// before the ordinary idle baseline on an explicitly opted-in owned resume.
	CombinedResumeDrain bool
	// VerifyOwnedPending is a trusted host callback, never native/client supplied.
	// A nil callback or any verification error denies continuation authority.
	VerifyOwnedPending func(context.Context, OwnedPendingRequest) error
	// AttentionTestHook is a caller-owned acceptance seam; nil in production.
	// It cannot supply admission evidence or alter native request parameters.
	AttentionTestHook func(context.Context, AttentionTestPoint) error
	// ExpectedThreadID binds a saved launch before any native thread operation.
	ExpectedThreadID string
	// Optional host launch-record bindings, checked before baseline publication.
	ExpectedModel, ExpectedWorkingDirectory string
	// ProfileDeveloperInstructions is the selected user-profile value. Its
	// precedence is resolved using a fresh native config/read before creation.
	ProfileDeveloperInstructions codexlaunch.Optional[string]
	// InitialDeveloperInstructions is trusted host-supplied guidance for a new
	// thread. It is combined with terminal guidance before durable admission.
	// Resume retains the native saved instructions and does not re-inject it.
	InitialDeveloperInstructions  string
	SocketRoot, JournalRoot, Name string
	StageTimeout, ShutdownTimeout time.Duration
}

// NewRuntime transfers ownership of conn even on error. SocketRoot must already
// be an owned private directory. JournalRoot must be an existing private journal
// directory; evidence is retained on shutdown. The launcher must pass the native
// server the same configuration environment used by its terminal client.
func NewRuntime(ctx context.Context, conn *websocket.Conn, opts RuntimeOptions) (_ *Runtime, err error) {
	if conn == nil {
		return nil, codexrpc.ErrConfig
	}
	adopted := false
	defer func() {
		if !adopted {
			_ = conn.Close()
		}
	}()
	if ctx == nil || opts.Name == "" || len(opts.ExpectedThreadID) > 512 || len(opts.InitialDeveloperInstructions) > 4<<20 || len(opts.ProfileDeveloperInstructions.Value) > 4<<20 {
		return nil, codexrpc.ErrConfig
	}
	if opts.StageTimeout == 0 {
		opts.StageTimeout = 30 * time.Second
	}
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = 5 * time.Second
	}
	if opts.StageTimeout <= 0 || opts.ShutdownTimeout <= 0 {
		return nil, codexrpc.ErrConfig
	}
	info, err := os.Lstat(opts.SocketRoot)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || stat.Uid != uint32(os.Geteuid()) {
		return nil, coveragejournal.ErrPrivate
	}
	journal, err := coveragejournal.Open(opts.JournalRoot)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !adopted {
			_ = journal.Close()
		}
	}()
	root, err := os.MkdirTemp(opts.SocketRoot, "coverage-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if !adopted {
			_ = os.RemoveAll(root)
		}
	}()
	sources := []workSource{terminalSource, boardSource, directMessageSource}
	names := []string{"terminal.sock", "board.sock", "dm.sock"}
	var listeners []*net.UnixListener
	defer func() {
		if !adopted {
			for _, listener := range listeners {
				_ = listener.Close()
			}
		}
	}()
	for _, name := range names {
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(root, name), Net: "unix"})
		if err != nil {
			return nil, err
		}
		listeners = append(listeners, listener)
	}
	wire, err := codexrpc.NewWebSocketWire(conn, codexrpc.DefaultWebSocketOptions())
	if err != nil {
		return nil, err
	}
	client, err := codexrpc.New(wire, rand.Text(), codexrpc.DefaultLimits())
	if err != nil {
		_ = wire.Close()
		return nil, err
	}
	defer func() {
		if !adopted {
			stop, cancel := context.WithTimeout(context.Background(), opts.ShutdownTimeout)
			defer cancel()
			_, _ = client.Stop(stop)
		}
	}()
	binding, err := client.Admit()
	if err != nil {
		return nil, err
	}
	gateway, err := newGateway(ctx, &codexGate{client: client, binding: binding}, journal, opts.Name, opts.StageTimeout, opts.ShutdownTimeout)
	if err != nil {
		return nil, err
	}
	gateway.attentionTestHook = opts.AttentionTestHook
	gateway.combinedResumeDrain = opts.CombinedResumeDrain
	gateway.verifyPending = opts.VerifyOwnedPending
	gateway.initialDeveloperInstructions = opts.InitialDeveloperInstructions
	gateway.profileDeveloperInstructions = opts.ProfileDeveloperInstructions
	gateway.expectedThreadID = opts.ExpectedThreadID
	gateway.expectedModel = opts.ExpectedModel
	gateway.expectedWorkingDirectory = opts.ExpectedWorkingDirectory
	runtime := &Runtime{gateway: gateway, done: make(chan struct{}), Terminal: "unix://" + filepath.Join(root, names[0]), Board: "unix://" + filepath.Join(root, names[1]), DirectMessage: "unix://" + filepath.Join(root, names[2])}
	results := make(chan error, len(listeners))
	for i, listener := range listeners {
		go func(source workSource, listener *net.UnixListener) {
			err := gateway.serveSocket(ctx, source, listener)
			results <- err
			gateway.cancel() // Unexpected listener termination invalidates the whole runtime.
		}(sources[i], listener)
	}
	adopted = true
	go func() {
		defer close(runtime.done)
		<-gateway.done
		report := gateway.report
		runtime.err = report.CleanupError
		timer := time.NewTimer(opts.ShutdownTimeout)
		defer timer.Stop()
		for range listeners {
			select {
			case err := <-results:
				runtime.err = errors.Join(runtime.err, err)
			case <-timer.C:
				runtime.err = errors.Join(runtime.err, codexrpc.ErrShutdownUnverified)
				return
			}
		}
		if !report.WriterJoined {
			runtime.err = errors.Join(runtime.err, codexrpc.ErrShutdownUnverified)
			return
		}
		runtime.err = errors.Join(runtime.err, journal.Close())
		if runtime.err == nil {
			runtime.err = os.RemoveAll(root)
		}
	}()
	return runtime, nil
}
func (r *Runtime) Ready(ctx context.Context) (codexlaunch.Identity, error) {
	if err := ctx.Err(); err != nil {
		return codexlaunch.Identity{}, err
	}
	select {
	case <-r.gateway.ready:
		if p, ok := r.gateway.g.client.Current(); ok && p.Binding == r.gateway.g.binding {
			return r.gateway.b.native.Identity, nil
		}
		return codexlaunch.Identity{}, errCoordinatorLost
	case <-r.gateway.done:
		return codexlaunch.Identity{}, r.gateway.report.Cause
	case <-ctx.Done():
		return codexlaunch.Identity{}, ctx.Err()
	}
}

// StoppedCause exposes only the immutable report of a terminated gateway.
// A live gateway has no historical failure report to sample as authority.
func (r *Runtime) StoppedCause() (error, bool) {
	select {
	case <-r.gateway.done:
		return r.gateway.report.Cause, true
	default:
		return nil, false
	}
}

func (r *Runtime) Done() <-chan struct{} { return r.done }
func (r *Runtime) Close(ctx context.Context) error {
	r.gateway.cancel()
	select {
	case <-r.done:
		return r.err
	case <-ctx.Done():
		return errors.Join(codexrpc.ErrShutdownUnverified, ctx.Err())
	}
}
