// Package mock provides a ModelClient test double.
package mock

import (
	"context"
	"sync"

	"agentkit/pipeline"
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
