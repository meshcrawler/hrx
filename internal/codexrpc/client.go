// This is an unactivated, single-reader JSON-RPC stream owner.
// Transport health is NOT evidence of native subscription or lifecycle coverage.
package codexrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"unicode/utf8"
)

var (
	ErrConfig             = errors.New("invalid observer transport configuration")
	ErrBusy               = errors.New("observer RPC already pending")
	ErrRead               = errors.New("observer transport read failed")
	ErrWrite              = errors.New("observer transport write failed")
	ErrProtocol           = errors.New("observer protocol or ownership mismatch")
	ErrOverflow           = errors.New("observer notification buffer overflow")
	ErrCanceled           = errors.New("observer RPC canceled; outcome uncertain")
	ErrStopped            = errors.New("observer transport stopped")
	ErrRemote             = errors.New("observer RPC rejected")
	ErrRequest            = errors.New("invalid observer request")
	ErrShutdownUnverified = errors.New("observer shutdown not verified")
)

// Wire must enforce the read limit BEFORE unbounded allocation, honor context
// cancellation, and unblock Read/Write when Close is called. New transfers its
// exclusive read ownership; callers must not retain a competing reader.
// NewWebSocketWire supplies a dormant bounded ping/pong adapter; transport
// liveness still does not establish subscription, baseline or coverage.
// This interface neither dials a server nor subscribes to a native thread.
type Wire interface {
	Read(context.Context, int) ([]byte, error)
	Write(context.Context, []byte) error
	Close() error
}

type Limits struct {
	FrameBytes, Messages, BufferedBytes int
}

func DefaultLimits() Limits {
	return Limits{FrameBytes: 4 << 20, Messages: 128, BufferedBytes: 8 << 20}
}

type Kind uint8

const (
	Notification Kind = iota
	ServerRequest
)

// Message retains the exact JSON frame, including fields unknown to this client.
// Ordinal is connection-local, NOT a native sequence/replay/coverage watermark.
// Raw can contain sensitive data; this package never logs it.
type Message struct {
	Kind       Kind
	Method     string
	Raw        json.RawMessage
	Generation string
	Ordinal    uint64
	requestID  json.RawMessage
	owner      *Client
}

type Response struct {
	Result, RemoteError json.RawMessage
	// These locate the reply in the same local order as Message. They are not
	// a native subscription receipt, replay cursor, or coverage watermark.
	Generation string
	Ordinal    uint64
}

type State string

const (
	TransportOpen State = "transport_open"
	TransportLost State = "transport_lost"
	TransportStop State = "transport_stopped"
)

// Snapshot describes only the local transport. There is deliberately no
// OBSERVING, subscription, compaction completion, or coverage-certified field.
type Snapshot struct {
	Generation       string
	State            State
	Cause            error // fixed classifications only, never remote error text
	Frames           uint64
	BufferedMessages int
	BufferedBytes    int
	Discarded        uint64 // known buffered frames discarded, not total native loss
	RPCPending       bool
}

type Shutdown struct {
	ReaderStopped, WritersStopped, TransportClosed bool
	Snapshot                                       Snapshot
}

type outcome struct {
	response Response
	err      error
}

type pending struct {
	id     string
	result chan outcome
}

type Client struct {
	wire       Wire
	limits     Limits
	generation string
	ctx        context.Context
	cancel     context.CancelFunc

	mu        sync.Mutex
	state     State
	cause     error
	serial    uint64
	frames    uint64
	discarded uint64
	queue     []Message
	bytes     int
	pending   *pending
	writes    int
	closeOK   bool

	// Publication gate state (publication.go), guarded by mu like transport state.
	nonce       string
	revision    uint64
	admitted    bool
	delivered   uint64
	published   uint64
	publication *Published
	consumed    *Consumed

	wake        chan struct{}
	done        chan struct{}
	readerDone  chan struct{}
	writersDone chan struct{}
	closeDone   chan struct{}
	writeToken  chan struct{}
}

// New starts the sole reader on an already-owned Wire. A fresh generation must
// be supplied for EVERY connection, including reconnections. Returning from New
// is a local reader-ownership barrier, never a native subscription/baseline.
// The Wire remains caller-owned if validation fails.
func New(wire Wire, generation string, limits Limits) (*Client, error) {
	if wire == nil || generation == "" || len(generation) > 128 ||
		limits.FrameBytes <= 0 || limits.Messages <= 0 || limits.BufferedBytes <= 0 {
		return nil, ErrConfig
	}
	for _, r := range generation {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return nil, ErrConfig
		}
	}
	if epochErr != nil {
		return nil, errEpochUnavailable
	}
	nonce, err := randomID()
	if err != nil {
		return nil, errEpochUnavailable
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{nonce: nonce,
		wire: wire, limits: limits, generation: generation, ctx: ctx, cancel: cancel,
		state: TransportOpen, wake: make(chan struct{}, 1), done: make(chan struct{}),
		readerDone: make(chan struct{}), writersDone: make(chan struct{}),
		closeDone: make(chan struct{}), writeToken: make(chan struct{}, 1),
	}
	c.writeToken <- struct{}{}
	ready := make(chan struct{})
	go func() {
		defer close(c.readerDone)
		close(ready)
		c.read()
	}()
	<-ready
	return c, nil
}

func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshot()
}

func (c *Client) snapshot() Snapshot {
	return Snapshot{
		Generation: c.generation, State: c.state, Cause: c.cause, Frames: c.frames,
		BufferedMessages: len(c.queue), BufferedBytes: c.bytes,
		Discarded: c.discarded, RPCPending: c.pending != nil,
	}
}

// finish publishes terminal loss/stop BEFORE initiating socket close. The first
// terminal classification wins. Buffered frames are invalidated, not silently
// replayed into a successor generation. A faulty Wire cannot fabricate clean
// shutdown: Stop must still join the reader, writers and Close result.
func (c *Client) finish(state State, cause error) {
	c.mu.Lock()
	if c.state != TransportOpen {
		c.mu.Unlock()
		return
	}
	c.finishLocked(state, cause)
	c.mu.Unlock()
	c.closeWire()
}

// finishLocked requires c.mu held and c.state == TransportOpen in that same
// critical section. Its caller owns the single closeWire call after unlocking.
func (c *Client) finishLocked(state State, cause error) {
	// Clear publication authority before Done closes (issue 23 note 4088).
	c.admitted, c.publication = false, nil
	c.consumed = nil
	c.state, c.cause = state, cause
	c.discarded += uint64(len(c.queue))
	c.queue, c.bytes = nil, 0
	if c.pending != nil {
		c.pending.result <- outcome{err: cause}
		c.pending = nil
	}
	if c.writes == 0 {
		close(c.writersDone)
	}
	close(c.done)
	c.cancel()
}

func (c *Client) closeWire() {
	go func() {
		err := c.wire.Close()
		c.mu.Lock()
		c.closeOK = err == nil
		c.mu.Unlock()
		close(c.closeDone)
	}()
}

func (c *Client) cancelPending(p *pending) {
	c.mu.Lock()
	if c.state != TransportOpen || c.pending != p {
		c.mu.Unlock()
		return
	}
	c.finishLocked(TransportLost, ErrCanceled)
	c.mu.Unlock()
	c.closeWire()
}

// Call permits one pending control RPC. It never reads the Wire. A timeout or
// cancellation of an unresolved request invalidates this connection rather than
// retrying an operation whose native outcome may be unknown.
func (c *Client) Call(ctx context.Context, method string, params any) (Response, error) {
	return c.call(ctx, method, params, nil, nil)
}

// CallAtPublication admits a control RPC only while the exact publication is
// current and covers every frame consumed by the transport reader. The check is
// repeated after obtaining the writer token, under the revocation mutex. This
// is a local write-admission fence, not native execution or handling evidence:
// after admission, concurrent loss/revocation cannot retract external I/O.
// Dormant; production TUI and queue paths must still be integrated separately.
func (c *Client) CallAtPublication(ctx context.Context, publication Published, method string, params any) (Response, error) {
	return c.call(ctx, method, params, &publication, nil)
}

// CallAtConsumed requires the exact current live frontier at both preflight and
// write admission. Any newly observed frame blocks the write until the owner
// reduces it and renews. Publication's immutable Through/digest remain unchanged.
func (c *Client) CallAtConsumed(ctx context.Context, frontier Consumed, method string, params any) (Response, error) {
	return c.call(ctx, method, params, nil, &frontier)
}

func (c *Client) publicationMatchesLocked(p *Published) bool {
	return p == nil || c.admitted && c.publication != nil && *c.publication == *p && p.Through == c.frames
}

func (c *Client) fenceMatchesLocked(p *Published, f *Consumed) bool {
	return c.publicationMatchesLocked(p) && (f == nil || c.admitted && c.publication != nil &&
		c.consumed != nil && *c.consumed == *f && *c.publication == f.Publication && f.Through == c.frames)
}

func (c *Client) call(ctx context.Context, method string, params any, publication *Published, frontier *Consumed) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	raw, err := json.Marshal(params)
	if err != nil || method == "" || len(method) > 512 {
		return Response{}, ErrRequest
	}
	c.mu.Lock()
	if c.state != TransportOpen {
		err = c.cause
		c.mu.Unlock()
		return Response{}, err
	}
	if c.pending != nil {
		c.mu.Unlock()
		return Response{}, ErrBusy
	}
	if !c.fenceMatchesLocked(publication, frontier) {
		c.mu.Unlock()
		return Response{}, ErrPublicationRefused
	}
	if c.serial == ^uint64(0) {
		c.finishLocked(TransportLost, ErrProtocol)
		c.mu.Unlock()
		c.closeWire()
		return Response{}, ErrProtocol
	}
	c.serial++
	p := &pending{id: fmt.Sprintf("hrx-observer/%s/%d", c.generation, c.serial), result: make(chan outcome, 1)}
	request := map[string]any{"id": p.id, "method": method}
	if params != nil {
		request["params"] = json.RawMessage(raw)
	}
	frame, _ := json.Marshal(request)
	if len(frame) > c.limits.FrameBytes {
		c.mu.Unlock()
		return Response{}, ErrRequest
	}
	c.pending = p
	c.mu.Unlock()

	stopCancel := context.AfterFunc(ctx, func() { c.cancelPending(p) })
	defer stopCancel()
	if err := c.writeAtFence(ctx, frame, publication, frontier); err != nil {
		c.cancelPending(p)
		return Response{}, err
	}
	select {
	case result := <-p.result:
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}
		c.mu.Lock()
		if result.response.Ordinal > c.delivered {
			c.delivered = result.response.Ordinal
		}
		c.mu.Unlock()
		return result.response, result.err
	case <-ctx.Done():
		c.cancelPending(p)
		return Response{}, ctx.Err()
	case <-c.done:
		return Response{}, c.Snapshot().Cause
	}
}

// Notify serializes client notifications (for example initialized) through the
// same writer. It does not establish a native subscription or coverage baseline.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if method == "" || len(method) > 512 {
		return ErrRequest
	}
	request := map[string]any{"method": method}
	if params != nil {
		request["params"] = params
	}
	frame, err := json.Marshal(request)
	if err != nil || len(frame) > c.limits.FrameBytes {
		return ErrRequest
	}
	return c.write(ctx, frame)
}

func (c *Client) write(ctx context.Context, frame []byte) error {
	return c.writeAtFence(ctx, frame, nil, nil)
}

func (c *Client) writeAtFence(ctx context.Context, frame []byte, publication *Published, frontier *Consumed) error {
	select {
	case <-c.writeToken:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.Snapshot().Cause
	}
	defer func() { c.writeToken <- struct{}{} }()
	c.mu.Lock()
	if c.state != TransportOpen {
		err := c.cause
		c.mu.Unlock()
		return err
	}
	if !c.fenceMatchesLocked(publication, frontier) {
		c.mu.Unlock()
		return ErrPublicationRefused
	}
	c.writes++
	c.mu.Unlock()
	err := c.wire.Write(ctx, frame)
	if err == nil {
		err = ctx.Err()
	}
	c.mu.Lock()
	c.writes--
	if c.state != TransportOpen && c.writes == 0 {
		close(c.writersDone)
	}
	c.mu.Unlock()
	if err != nil {
		// A canceled admitted write may already have reached native. Keep its
		// uncertain cancellation classification independent of AfterFunc timing.
		if ctx.Err() != nil {
			c.finish(TransportLost, ErrCanceled)
			return ErrCanceled
		}
		c.finish(TransportLost, ErrWrite)
		return ErrWrite
	}
	return nil
}

// Reply sends a response only for a received request from this generation.
// It grants no approval/permission by itself: the host must apply its policy.
// No unbounded outstanding-server-request map is retained in the foundation.
func (c *Client) Reply(ctx context.Context, request Message, result, remoteError json.RawMessage) error {
	return c.reply(ctx, request, result, remoteError, nil)
}

// ReplyAtConsumed fences permission/input responses against the same live
// frontier as work calls. A server request must already have been reduced before
// its terminal response can authorize native work. The host still owns policy,
// one-shot request correlation, and rejection of replies from auxiliary clients.
func (c *Client) ReplyAtConsumed(ctx context.Context, frontier Consumed, request Message, result, remoteError json.RawMessage) error {
	return c.reply(ctx, request, result, remoteError, &frontier)
}

func (c *Client) reply(ctx context.Context, request Message, result, remoteError json.RawMessage, frontier *Consumed) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if request.Kind != ServerRequest || request.owner != c || request.Generation != c.generation || len(request.requestID) == 0 {
		return ErrRequest
	}
	reply := map[string]json.RawMessage{"id": request.requestID}
	if len(remoteError) > 0 {
		if !validRPCError(remoteError) {
			return ErrRequest
		}
		reply["error"] = remoteError
	} else {
		if len(result) == 0 {
			result = json.RawMessage("null")
		}
		if !json.Valid(result) {
			return ErrRequest
		}
		reply["result"] = result
	}
	frame, err := json.Marshal(reply)
	if err != nil || len(frame) > c.limits.FrameBytes {
		return ErrRequest
	}
	return c.writeAtFence(ctx, frame, nil, frontier)
}

// Next returns notifications and server requests in wire order. Terminal loss
// invalidates the buffered prefix; Next does not expose it as current coverage.
func (c *Client) Next(ctx context.Context) (Message, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Message{}, err
		}
		c.mu.Lock()
		if c.state != TransportOpen {
			err := c.cause
			c.mu.Unlock()
			return Message{}, err
		}
		if len(c.queue) > 0 {
			message := c.queue[0]
			c.bytes -= len(message.Raw) + len(message.Method) + len(message.requestID)
			copy(c.queue, c.queue[1:])
			c.queue[len(c.queue)-1] = Message{}
			c.queue = c.queue[:len(c.queue)-1]
			if message.Ordinal > c.delivered {
				c.delivered = message.Ordinal
			}
			c.mu.Unlock()
			return message, nil
		}
		c.mu.Unlock()
		select {
		case <-c.wake:
		case <-c.done:
		case <-ctx.Done():
			return Message{}, ctx.Err()
		}
	}
}

func (c *Client) read() {
	for {
		raw, err := c.wire.Read(c.ctx, c.limits.FrameBytes)
		if err != nil {
			c.finish(TransportLost, ErrRead)
			return
		}
		if len(raw) > c.limits.FrameBytes {
			c.finish(TransportLost, ErrProtocol)
			return
		}
		fields, err := envelope(raw)
		if err != nil {
			c.finish(TransportLost, ErrProtocol)
			return
		}
		method, hasMethod := fields["method"]
		id, hasID := fields["id"]
		result, hasResult := fields["result"]
		rpcError, hasError := fields["error"]
		c.mu.Lock()
		if c.state != TransportOpen {
			c.mu.Unlock()
			return
		}
		c.frames++
		if !hasMethod {
			var responseID string
			if !hasID || hasResult == hasError || json.Unmarshal(id, &responseID) != nil ||
				c.pending == nil || responseID != c.pending.id || hasError && !validRPCError(rpcError) {
				c.finishLocked(TransportLost, ErrProtocol)
				c.mu.Unlock()
				c.closeWire()
				return
			}
			response := outcome{response: Response{
				Result: result, RemoteError: rpcError, Generation: c.generation, Ordinal: c.frames,
			}}
			if hasError {
				response.err = ErrRemote
			}
			c.pending.result <- response
			c.pending = nil
			c.mu.Unlock()
			continue
		}
		var name string
		if json.Unmarshal(method, &name) != nil || name == "" || len(name) > 512 || hasResult || hasError ||
			hasID && !validID(id) {
			c.finishLocked(TransportLost, ErrProtocol)
			c.mu.Unlock()
			c.closeWire()
			return
		}
		kind := Notification
		if hasID {
			kind = ServerRequest
		}
		message := Message{
			Kind: kind, Method: name, Raw: bytes.Clone(raw), requestID: id,
			Generation: c.generation, Ordinal: c.frames, owner: c,
		}
		size := len(message.Raw) + len(name) + len(id)
		if len(c.queue) >= c.limits.Messages || size > c.limits.BufferedBytes-c.bytes {
			// Invalidate before unlocking: Next must not drain a prefix after
			// overflow has already established that the stream is incomplete.
			c.finishLocked(TransportLost, ErrOverflow)
			c.mu.Unlock()
			c.closeWire()
			return
		}
		c.queue = append(c.queue, message)
		c.bytes += size
		c.mu.Unlock()
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}

// Stop never claims clean shutdown on a context deadline, Close error, or a
// reader/writer that did not exit. A broken Wire can outlive Stop; callers must
// preserve UNKNOWN and must not reuse it or its generation.
func (c *Client) Stop(ctx context.Context) (Shutdown, error) {
	c.finish(TransportStop, ErrStopped)
	wait := func(done <-chan struct{}) bool {
		select {
		case <-done:
			return true
		default:
		}
		select {
		case <-done:
			return true
		case <-ctx.Done():
			return false
		}
	}
	reader, writers, closed := wait(c.readerDone), wait(c.writersDone), wait(c.closeDone)
	c.mu.Lock()
	report := Shutdown{ReaderStopped: reader, WritersStopped: writers, TransportClosed: closed && c.closeOK, Snapshot: c.snapshot()}
	c.mu.Unlock()
	if ctx.Err() != nil || !reader || !writers || !report.TransportClosed {
		return report, ErrShutdownUnverified
	}
	return report, nil
}

// envelope rejects duplicate keys, trailing JSON and non-object messages before
// routing. It accepts unknown fields but retains the exact original frame.
func envelope(raw []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(raw) {
		return nil, ErrProtocol
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrProtocol
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, ErrProtocol
		}
		name, ok := key.(string)
		if !ok {
			return nil, ErrProtocol
		}
		if _, exists := fields[name]; exists {
			return nil, ErrProtocol
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, ErrProtocol
		}
		fields[name] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, ErrProtocol
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, ErrProtocol
	}
	return fields, nil
}

func validID(raw json.RawMessage) bool {
	if len(raw) > 512 {
		return false
	}
	var text string
	if len(raw) > 0 && raw[0] == '"' {
		return json.Unmarshal(raw, &text) == nil
	}
	_, err := strconv.ParseInt(string(raw), 10, 64)
	return err == nil
}

func validRPCError(raw json.RawMessage) bool {
	fields, err := envelope(raw)
	if err != nil {
		return false
	}
	var message string
	_, numberErr := strconv.ParseInt(string(fields["code"]), 10, 64)
	text := fields["message"]
	return numberErr == nil && len(text) > 0 && text[0] == '"' && json.Unmarshal(text, &message) == nil
}
