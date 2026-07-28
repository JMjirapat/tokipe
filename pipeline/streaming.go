package pipeline

import (
	"context"
	"iter"
	"strings"

	"github.com/JMjirapat/tokipe/internal/safe"
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
	prep, err := p.Prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	if prep.ShortCircuited() {
		return StreamOne(prep.Response), nil
	}
	req, client := prep.Request, prep.Client

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

// MetaRouterError holds the error from a Router that panicked. The pipeline
// package reports nothing itself — it has no Recorder, by design — so it leaves
// the evidence in Metadata for a caller or a wrapping stage to surface.
const MetaRouterError = "router.error"

// Prepared is the outcome of running every stage without performing the model
// call: the shaped Request, the client routing chose for it, and — when a stage
// answered on its own — the Response that makes a model call unnecessary.
//
// Exactly one of Response and Client is non-nil. A non-nil Response means a
// stage short-circuited the turn; there is nothing left to send.
type Prepared struct {
	// Request is the shaped request: retrieved, deduped, compressed, trimmed
	// and aligned, exactly as Run would have sent it.
	Request *Request

	// Response is non-nil only when a stage answered without an LLM call.
	Response *Response

	// Client is the ModelClient the router selected, or the pipeline's default
	// when no router is configured. Nil when Response is non-nil.
	Client ModelClient
}

// ShortCircuited reports whether a stage answered the turn without a model
// call, making Client nil and Response the final answer.
func (p *Prepared) ShortCircuited() bool { return p != nil && p.Response != nil }

// Prepare runs every stage in order and resolves the client, stopping short of
// the model call itself. Run and RunStream are both built on it, so the three
// can never drift in stage order, short-circuit handling, or routing.
//
// It is also the entry point for callers that own their own model call — an
// external agent runtime that wants the prompt shaped but intends to send it
// with its own credentials and SDK. Such a caller gets every stage's benefit
// except the two that need the send itself: cache alignment emits breakpoints
// it must then honour, and routing returns a Client it is free to ignore.
//
// NOTE: a panic from Stage.Process is deliberately NOT recovered; see the Stage
// docs. Name() is guarded, because a broken name must never destroy an error
// Process already returned.
func (p *Pipeline) Prepare(ctx context.Context, req *Request) (*Prepared, error) {
	for _, stage := range p.stages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		next, err := stage.Process(ctx, req)
		if err != nil {
			return nil, &StageError{Stage: safe.Name(stage.Name, UnnamedStage), Err: err}
		}
		if next != nil {
			req = next
		}
		if v, ok := req.Metadata[MetaShortCircuit]; ok {
			resp, ok := v.(*Response)
			if !ok {
				return nil, &StageError{
					Stage: safe.Name(stage.Name, UnnamedStage),
					Err:   errBadShortCircuit,
				}
			}
			return &Prepared{Request: req, Response: resp}, nil
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
			req.SetMeta(MetaRouterError, err)
		case d.Client != nil:
			client = d.Client
			req.SetMeta("router.reason", d.Reason)
			req.SetMeta("router.client", safe.Name(d.Client.Name, UnnamedClient))
		}
	}
	return &Prepared{Request: req, Client: client}, nil
}
