package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/meshcrawler/hrx/internal/codexrpc"
)

// Native fixtures simulate a live terminal: continuously read notifications and
// answer WebSocket control frames even between RPC calls. This is not a second
// production discovery/coverage owner.
type pitputRPC struct {
	conn   io.Closer
	client *codexrpc.Client
}

var errPitputObservationDeferred = errors.New("native observation deferred by publication fence")

func fixtureObservationDeferred(response codexrpc.Response, err error) bool {
	var remote struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	return errors.Is(err, codexrpc.ErrRemote) && json.Unmarshal(response.RemoteError, &remote) == nil &&
		remote.Code == -32001 && remote.Message == "coverage admission unavailable"
}

type fixtureRPCCloser struct {
	client  *codexrpc.Client
	wire    *codexrpc.WebSocketWire
	drained <-chan struct{}
}

func (c *fixtureRPCCloser) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, first := c.client.Stop(ctx)
	_, second := c.wire.Stop(ctx)
	select {
	case <-c.drained:
	case <-ctx.Done():
		return errors.Join(first, second, ctx.Err())
	}
	return errors.Join(first, second)
}
func connectPitputServer(ctx context.Context, socket string) (*pitputRPC, error) {
	conn, err := connectPitputSocket(ctx, socket)
	if err != nil {
		return nil, err
	}
	wire, err := codexrpc.NewWebSocketWire(conn, codexrpc.DefaultWebSocketOptions())
	if err != nil {
		conn.Close()
		return nil, err
	}
	client, err := codexrpc.New(wire, "pitput-fixture", codexrpc.DefaultLimits())
	if err != nil {
		wire.Close()
		return nil, err
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			if _, err := client.Next(context.Background()); err != nil {
				return
			}
		}
	}()
	closer := &fixtureRPCCloser{client: client, wire: wire, drained: drained}
	r := &pitputRPC{conn: closer, client: client}
	err = r.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "hrx-pitput-fixture", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}}, nil)
	if err == nil {
		err = client.Notify(ctx, "initialized", nil)
	}
	if err != nil {
		closer.Close()
		return nil, err
	}
	return r, nil
}
func (r *pitputRPC) call(method string, params, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return r.callContext(ctx, method, params, result)
}

func (r *pitputRPC) callContext(ctx context.Context, method string, params, result any) error {
	response, err := r.client.Call(ctx, method, params)
	if err != nil {
		if method == "thread/read" && fixtureObservationDeferred(response, err) {
			return errPitputObservationDeferred
		}
		return err
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(response.Result, result)
}
