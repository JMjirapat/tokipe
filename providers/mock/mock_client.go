// Package mock provides a ModelClient test double.
package mock

import (
	"context"
	"iter"
	"strings"
	"sync"

	"github.com/JMjirapat/tokipe/pipeline"
)

// Client is a configurable ModelClient for tests and examples. It records the
// requests it receives so tests can assert on the final shaped Request.
type Client struct {
	ClientName string

	// Response is returned verbatim (with ModelUsed filled in) from Send.
	Response *pipeline.Response

	// Err, if set, is returned from Send instead of Response.
	Err error

	// SendFunc, if set, overrides Response/Err entirely.
	SendFunc func(ctx context.Context, req *pipeline.Request) (*pipeline.Response, error)

	mu       sync.Mutex
	calls    int
	requests []*pipeline.Request
}

// New returns a Client that answers every Send with content.
func New(name, content string) *Client {
	return &Client{
		ClientName: name,
		Response:   &pipeline.Response{Content: content},
	}
}

func (c *Client) Name() string {
	if c.ClientName == "" {
		return "mock"
	}
	return c.ClientName
}

func (c *Client) Send(ctx context.Context, req *pipeline.Request) (*pipeline.Response, error) {
	c.mu.Lock()
	c.calls++
	c.requests = append(c.requests, req)
	c.mu.Unlock()

	if c.SendFunc != nil {
		return c.SendFunc(ctx, req)
	}
	if c.Err != nil {
		return nil, c.Err
	}
	resp := &pipeline.Response{}
	if c.Response != nil {
		copied := *c.Response
		resp = &copied
	}
	if resp.ModelUsed == "" {
		resp.ModelUsed = c.Name()
	}
	return resp, nil
}

// Calls reports how many times Send was invoked.
func (c *Client) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// LastRequest returns the most recent Request passed to Send, or nil.
func (c *Client) LastRequest() *pipeline.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) == 0 {
		return nil
	}
	return c.requests[len(c.requests)-1]
}

// StreamClient is a Client that also implements pipeline.StreamingClient, for
// testing and demonstrating the streaming path without a real provider.
//
// It is a separate type rather than extra methods on Client on purpose: half
// the value of the streaming tests is checking what happens when a client does
// NOT stream, and Client must stay that way to serve as the negative case.
type StreamClient struct {
	*Client

	// Chunks are yielded in order. When empty, Response.Content is split on
	// spaces so a caller gets several deltas without having to spell them out.
	Chunks []string

	// Err, if set, is yielded after every chunk in Chunks — a mid-stream
	// failure, as opposed to Client.Err which fails before streaming starts.
	Err error

	// StreamErr, if set, is returned by SendStream itself, before any delta.
	StreamErr error

	mu          sync.Mutex
	streamCalls int
}

var _ pipeline.StreamingClient = (*StreamClient)(nil)

// NewStream returns a StreamClient that answers with content, streamed.
func NewStream(name, content string, chunks ...string) *StreamClient {
	return &StreamClient{Client: New(name, content), Chunks: chunks}
}

func (s *StreamClient) SendStream(ctx context.Context, req *pipeline.Request) (iter.Seq2[pipeline.Delta, error], error) {
	s.mu.Lock()
	s.streamCalls++
	s.mu.Unlock()

	s.Client.mu.Lock()
	s.Client.requests = append(s.Client.requests, req)
	s.Client.mu.Unlock()

	if s.StreamErr != nil {
		return nil, s.StreamErr
	}

	chunks := s.Chunks
	if len(chunks) == 0 && s.Response != nil {
		chunks = strings.SplitAfter(s.Response.Content, " ")
	}
	name := s.Name()
	usage := pipeline.Usage{}
	if s.Response != nil {
		usage = s.Response.Usage
	}

	return func(yield func(pipeline.Delta, error) bool) {
		for i, c := range chunks {
			if err := ctx.Err(); err != nil {
				yield(pipeline.Delta{ModelUsed: name}, err)
				return
			}
			d := pipeline.Delta{Text: c, ModelUsed: name}
			// Usage rides the last delta, as a real provider reports it.
			if i == len(chunks)-1 && s.Err == nil {
				u := usage
				d.Usage = &u
			}
			if !yield(d, nil) {
				return // the consumer stopped early
			}
		}
		if s.Err != nil {
			yield(pipeline.Delta{ModelUsed: name}, s.Err)
		}
	}, nil
}

// StreamCalls reports how many times SendStream was invoked.
func (s *StreamClient) StreamCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamCalls
}
