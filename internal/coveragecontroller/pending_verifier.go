package coveragecontroller

import "context"

// OwnedPendingRequest is an exact host verification target, not client-provided
// authorization. The gateway checks the live native binding before invoking its
// host-installed callback. No native payload or credentials are passed through.
type OwnedPendingRequest struct {
	RuntimeBinding string
	ThreadID       string
	DeliveryID     string
}

func (h *gateway) verifyOwnedPending(ctx context.Context, request OwnedPendingRequest) error {
	if h.state.Load() != 1 || h.verifyPending == nil || request.RuntimeBinding != h.attentionBinding() ||
		request.ThreadID != h.b.native.ThreadID || !attentionID.MatchString(request.DeliveryID) {
		return errAttention
	}
	select {
	case <-h.ready:
	default:
		return errWorkNotReady
	}
	ctx, cancel := context.WithTimeout(ctx, h.stageBudget)
	defer cancel()
	if h.ctx != nil {
		stop := context.AfterFunc(h.ctx, cancel)
		defer stop()
		if h.ctx.Err() != nil {
			return errAttention
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := h.verifyPending(ctx, request); err != nil {
		return errAttention
	}
	return ctx.Err()
}
