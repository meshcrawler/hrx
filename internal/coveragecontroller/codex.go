package coveragecontroller

import (
	"context"
	"encoding/json"

	"github.com/meshcrawler/hrx/internal/codexrpc"
)

// codexGate adapts an ALREADY admitted binding. It never calls Admit: validating
// native creator/baseline/configuration evidence remains the launch owner's job.
// The adapter owns all Call/Next consumption after handoff; no competing reader
// or direct caller may bypass the coordinator's ordered response merge.
type codexGate struct {
	client    *codexrpc.Client
	binding   codexrpc.Binding
	approvals *approvalBook
}

func (g *codexGate) identity() (connectionID, uint64) {
	return connectionID{g.binding.Epoch, g.binding.Nonce, g.binding.Generation}, g.binding.Revision
}
func (g *codexGate) done() <-chan struct{} { return g.client.Done() }
func (g *codexGate) publish(through uint64, digest [32]byte) error {
	_, err := g.client.Publish(g.binding, through, digest)
	return err
}
func (g *codexGate) revoke() error { return g.client.Revoke(g.binding) }

func (g *codexGate) receive(ctx context.Context) (frame, error) {
	m, err := g.client.Next(ctx)
	if err != nil {
		return frame{}, err
	}
	id, _ := g.identity()
	if m.Generation != id.generation {
		return frame{}, errPrefixBinding
	}
	kind := notificationFrame
	if m.Kind == codexrpc.ServerRequest {
		kind = serverRequestFrame
		if g.approvals != nil {
			if err := g.approvals.capture(m); err != nil {
				return frame{}, err
			}
		}
	}
	return frame{id, m.Ordinal, kind, m.Raw}, nil
}

// A remote error still returns a frame with a required ordinal. The coordinator
// must account for that frame before handling the RPC error. A zero frame with
// an error is a missing-prefix/loss condition, never an ignorable failed request.
func (g *codexGate) call(ctx context.Context, method string, params any) (frame, error) {
	r, err := g.client.Call(ctx, method, params)
	return g.responseFrame(r, err)
}

func (g *codexGate) responseFrame(r codexrpc.Response, err error) (frame, error) {
	if r.Ordinal == 0 {
		return frame{}, err
	}
	id, _ := g.identity()
	if r.Generation != id.generation {
		return frame{}, errPrefixBinding
	}
	body := map[string]json.RawMessage{"result": r.Result}
	if len(r.RemoteError) > 0 {
		body = map[string]json.RawMessage{"error": r.RemoteError}
	}
	raw, encodeErr := json.Marshal(body)
	if encodeErr != nil {
		return frame{}, errEventFormat
	}
	return frame{id, r.Ordinal, responseFrame, raw}, err
}
