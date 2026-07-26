package pipeline

import (
	"context"
	"iter"
	"strings"

	"agentkit/internal/safe"
)

// Delta is one incremental piece of a streamed response.
//
// Text is the increment, not the accumulated answer — concatenating every
// Delta's Text yields the full content.
type Delta struct {
	Text string

	// ModelUsed names the client producing the stream. Set on every delta so a
	// consumer never has to wait for the end to know what answered.
	ModelUsed string

	// Usage is non-nil only on the delta that carries the provider's final
	// accounting, if the provider reports one at all. A CLI backend or a
	// non-streaming client wrapped by StreamOne may never set it.
	Usage *Usage
}

// StreamingClient is an OPTIONAL interface a ModelClient may also implement.
//
// It is a separate interface rather than a method on ModelClient because
// ModelClient froze at v1.0.0 (see README §Stability). Adding a method there
// would have broken every existing implementation; adding an interface breaks
// nothing, and Pipeline.RunStream works with clients that implement neither.
type StreamingClient interface {
	ModelClient

	// SendStream returns a sequence of deltas. The error return is for
	// failures that happen before streaming starts (a rejected request, a
	// refused connection); failures mid-stream are yielded as the error half
	// of a pair, after which the sequence must stop.
	SendStream(ctx context.Context, req *Request) (iter.Seq2[Delta, error], error)
}

// RunStream is Run with an incremental result. Every stage runs first, exactly
// as in Run and in the same order — streaming is purely a property of the final
// model call, which is why no Stage needed to change to support it.
//
// The returned sequence must be consumed to completion or abandoned; abandoning
// it (by breaking out of the range loop) cancels nothing on its own, so pass a
// cancellable ctx if that matters.
//
// A short-circuiting preprocess rule yields exactly one delta and stops, so
// callers need no special case for "the answer arrived without a model".
func (p *Pipeline) RunStream(ctx context.Context, req *Request) (iter.Seq2[Delta, error], error) {
	req, resp, client, err := p.prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp != nil {
		return StreamOne(resp), nil // short-circuited
	}

	// A client that cannot stream is not an error: adapt it, so callers never
	// have to ask which kind of client they hold.
	sc, ok := client.(StreamingClient)
	if !ok {
		out, err := client.Send(ctx, req)
		if err != nil {
			return nil, err
		}
		return StreamOne(out), nil
	}

	seq, err := sc.SendStream(ctx, req)
	if err != nil {
		return nil, err
	}
	if seq == nil {
		// A StreamingClient that returns no sequence and no error is broken.
		// Fall back rather than handing the caller a nil range target.
		out, err := client.Send(ctx, req)
		if err != nil {
			return nil, err
		}
		return StreamOne(out), nil
	}
	return seq, nil
}

// StreamOne adapts a finished Response into a single-delta sequence. It is what
// makes a non-streaming ModelClient usable through RunStream, and what a
// provider can use to satisfy SendStream before it implements real streaming.
func StreamOne(resp *Response) iter.Seq2[Delta, error] {
	return func(yield func(Delta, error) bool) {
		if resp == nil {
			return
		}
		usage := resp.Usage
		yield(Delta{Text: resp.Content, ModelUsed: resp.ModelUsed, Usage: &usage}, nil)
	}
}

// Collect drains a delta sequence into a Response, concatenating text and
// keeping the last reported usage. It stops at the first error and returns it
// alongside whatever had accumulated, because a partial answer is often still
// worth showing.
func Collect(seq iter.Seq2[Delta, error]) (*Response, error) {
	resp := &Response{}
	if seq == nil {
		return resp, nil
	}

	var b strings.Builder
	for delta, err := range seq {
		if delta.ModelUsed != "" {
			resp.ModelUsed = delta.ModelUsed
		}
		b.WriteString(delta.Text)
		if delta.Usage != nil {
			resp.Usage = *delta.Usage
		}
		if err != nil {
			resp.Content = b.String()
			return resp, err
		}
	}
	resp.Content = b.String()
	return resp, nil
}

// prepare runs the stages and resolves the client, shared by Run and RunStream
// so the two can never drift in stage order, short-circuit handling, or
// routing. A non-nil *Response means a stage answered and no model call is due.
func (p *Pipeline) prepare(ctx context.Context, req *Request) (*Request, *Response, ModelClient, error) {
	for _, stage := range p.stages {
		if err := ctx.Err(); err != nil {
			return req, nil, nil, err
		}
		next, err := stage.Process(ctx, req)
		if err != nil {
			return req, nil, nil, &StageError{Stage: safe.Name(stage.Name, UnnamedStage), Err: err}
		}
		if next != nil {
			req = next
		}
		if v, ok := req.Metadata[MetaShortCircuit]; ok {
			resp, ok := v.(*Response)
			if !ok {
				return req, nil, nil, &StageError{
					Stage: safe.Name(stage.Name, UnnamedStage),
					Err:   errBadShortCircuit,
				}
			}
			return req, resp, nil, nil
		}
	}

	client := p.client
	if p.router != nil {
		d, err := safe.Value(func() (RouteDecision, error) {
			return p.router.Route(ctx, req), nil
		})
		switch {
		case err != nil:
			req.SetMeta("router.reason", "router_panicked")
		case d.Client != nil:
			client = d.Client
			req.SetMeta("router.reason", d.Reason)
			req.SetMeta("router.client", safe.Name(d.Client.Name, UnnamedClient))
		}
	}
	return req, nil, client, nil
}
