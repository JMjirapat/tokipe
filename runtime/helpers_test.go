package runtime_test

import (
	"context"

	"github.com/JMjirapat/tokipe/config"
	"github.com/JMjirapat/tokipe/pipeline"
)

// Small adapters so the tests can supply Go-only components without dragging in
// a mock package for each interface.

type embedderFunc func(context.Context, string) ([]float32, error)

func (f embedderFunc) Embed(ctx context.Context, text string) ([]float32, error) {
	return f(ctx, text)
}

type storeFunc func(context.Context, []float32, int) ([]pipeline.Chunk, error)

func (f storeFunc) Search(ctx context.Context, vec []float32, topK int) ([]pipeline.Chunk, error) {
	return f(ctx, vec, topK)
}

type stageFunc struct {
	name string
	fn   func(context.Context, *pipeline.Request) (*pipeline.Request, error)
}

func (s stageFunc) Name() string { return s.name }

func (s stageFunc) Process(ctx context.Context, req *pipeline.Request) (*pipeline.Request, error) {
	return s.fn(ctx, req)
}

type ruleFunc struct {
	name  string
	match func(*pipeline.Request) bool
	fn    func(*pipeline.Request) (*pipeline.Response, error)
}

func (r ruleFunc) Name() string                         { return r.name }
func (r ruleFunc) CanHandle(req *pipeline.Request) bool { return r.match(req) }
func (r ruleFunc) Handle(req *pipeline.Request) (*pipeline.Response, error) {
	return r.fn(req)
}

// configWithStage exists so the test reads as "a Go-only option", which is the
// property under test, rather than as a config package call.
func configWithStage(s pipeline.Stage) config.Option { return config.WithStage(s) }
