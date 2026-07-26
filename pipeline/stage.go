// Package pipeline defines the core contract of agentkit: the Request that
// flows through every optimization stage, the Response returned to the caller,
// the Stage extension point, and the Pipeline that runs them in order.
//
// Nothing in this package performs I/O and it has zero third-party
// dependencies. See docs/spec.md §2.3.
package pipeline

import "context"

// Request flows through every stage. Stages read and mutate fields relevant
// to their job and pass the (possibly modified) Request to the next stage.
type Request struct {
	// Query is the user-facing question/instruction for this turn.
	Query string

	// Messages is the full conversation history, oldest first.
	Messages []Message

	// ToolCalls, if non-nil, means this Request represents a tool-call
	// resolution step rather than a fresh LLM turn.
	ToolCalls []ToolCall

	// NeedsRetrieval signals to the RAG stage whether retrieval should run at
	// all. Default false — callers must opt in explicitly.
	NeedsRetrieval bool

	// RetrievedChunks is populated by the RAG stage; empty until then.
	// It is *dynamic* content: it changes every turn, so no cache breakpoint
	// may ever be anchored inside it (see cache.Aligner).
	RetrievedChunks []Chunk

	// TurnType classifies this turn for the budget package.
	TurnType TurnType

	// CacheBreakpoints is populated by cache.Aligner.
	CacheBreakpoints []CacheBreakpoint

	// Metadata is an open bag for stage-specific or caller-specific data that
	// doesn't warrant a first-class field. Stages must namespace keys
	// (e.g. "preprocess.matched_rule") to avoid collisions.
	Metadata map[string]any
}

// SetMeta stores a namespaced metadata value, allocating the map if needed.
func (r *Request) SetMeta(key string, value any) {
	if r.Metadata == nil {
		r.Metadata = map[string]any{}
	}
	r.Metadata[key] = value
}

// MetaShortCircuit is the reserved Metadata key a stage sets to hand a
// finished *Response back to Pipeline.Run without making an LLM call.
const MetaShortCircuit = "_short_circuit_response"

type Message struct {
	Role    string // "user" | "assistant" | "system"
	Content string

	// Static marks content the caller guarantees will not change for the
	// lifetime of the session (system prompt, tool definitions). cache.Aligner
	// anchors provider-side cache breakpoints only after static content.
	//
	// NOTE: additive extension to the spec's Message type — §2.4.6 requires
	// "caller-marked messages" for the static segment but the spec's struct
	// had no field to carry that mark.
	Static bool
}

type ToolCall struct {
	Name string
	Args map[string]any
}

type Chunk struct {
	Content    string
	SourceURL  string
	Similarity float64
}

type TurnType int

const (
	TurnUnknown TurnType = iota
	TurnNewQuestion
	TurnRoutineResume // e.g., resuming right after a tool result
	TurnErrorRecovery
)

func (t TurnType) String() string {
	switch t {
	case TurnNewQuestion:
		return "new_question"
	case TurnRoutineResume:
		return "routine_resume"
	case TurnErrorRecovery:
		return "error_recovery"
	default:
		return "unknown"
	}
}

// CacheBreakpoint marks where provider-side prompt caching should be anchored
// in the outgoing request. AfterMessageIndex is an index into Request.Messages.
type CacheBreakpoint struct {
	AfterMessageIndex int
	Reason            string
}

// Response is the final result returned to the caller after the pipeline
// (or a short-circuiting stage) has produced an answer.
type Response struct {
	Content        string
	ModelUsed      string
	ShortCircuited bool // true if a preprocess rule handled it without an LLM call
	Usage          Usage
}

type Usage struct {
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
}

// Stage is the single extension point of the pipeline. Every optimization
// technique (compression, caching, routing, etc.) is a Stage implementation.
type Stage interface {
	// Process receives the current Request state and returns the next state.
	// Returning a non-nil error aborts the pipeline UNLESS the stage's own
	// contract says otherwise (see the fail-open rule in docs/spec.md §2.5).
	Process(ctx context.Context, req *Request) (*Request, error)

	// Name is used in logs/metrics to identify which stage did what.
	Name() string
}

// ModelClient abstracts a single LLM provider/model endpoint.
//
// NOTE: the spec placed this interface in package providers, but providers
// imports pipeline for Request/Response, which would be an import cycle. The
// interface is therefore declared here and re-exported as a type alias by
// package providers, so `providers.ModelClient` remains a valid, identical
// name for callers exactly as the spec's signatures describe.
type ModelClient interface {
	Send(ctx context.Context, req *Request) (*Response, error)
	Name() string
}

// Router selects the ModelClient used for the final Send. It is deliberately
// NOT a Stage: it runs after every context-shaping stage so it can see the
// final prompt size, and wiring it as a special final step keeps that ordering
// un-misconfigurable (docs/spec.md §2.4.7).
//
// Declared here rather than in package router for the same import-cycle reason
// as ModelClient; package router re-exports it.
type Router interface {
	Route(ctx context.Context, req *Request) RouteDecision
}

type RouteDecision struct {
	Client     ModelClient
	Reason     string
	Confidence float64
}

// Pipeline runs an ordered list of stages, then performs the final model call.
type Pipeline struct {
	stages []Stage
	client ModelClient
	router Router
}

// New builds a Pipeline that always calls client for the final Send.
func New(client ModelClient, stages ...Stage) *Pipeline {
	return &Pipeline{stages: stages, client: client}
}

// NewWithRouter builds a Pipeline that asks router which client to Send to
// after all stages have run. fallback is used when router is nil or returns a
// decision with no Client (fail-open).
func NewWithRouter(fallback ModelClient, router Router, stages ...Stage) *Pipeline {
	return &Pipeline{stages: stages, client: fallback, router: router}
}

// Run executes every stage in order, then performs the final model call.
//
// The stage loop, short-circuit handling and routing all live in prepare, which
// RunStream shares. Keeping one copy is deliberate: duplicated, the streaming
// path could silently drift out of step with this one, and that is a bug class
// better designed out than tested for.
//
// NOTE: a panic from Stage.Process is deliberately NOT recovered; see the Stage
// docs. Name() is guarded, because a broken name must never destroy an error
// Process already returned.
func (p *Pipeline) Run(ctx context.Context, req *Request) (*Response, error) {
	req, resp, client, err := p.prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp != nil {
		return resp, nil // a stage answered; no model call is due
	}
	return client.Send(ctx, req)
}

type StageError struct {
	Stage string
	Err   error
}

func (e *StageError) Error() string { return e.Stage + ": " + e.Err.Error() }
func (e *StageError) Unwrap() error { return e.Err }

type constError string

func (e constError) Error() string { return string(e) }

const errBadShortCircuit = constError("metadata " + MetaShortCircuit + " is not a *Response")

// UnnamedClient is reported in Metadata when a ModelClient's Name method
// panics or returns "".
const UnnamedClient = "unnamed_client"

// UnnamedStage names a StageError when the stage's own Name method panics or
// returns "".
const UnnamedStage = "unnamed_stage"
