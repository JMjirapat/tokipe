// Command streaming demonstrates Pipeline.RunStream — the same stages, with an
// incremental result.
//
//	go run ./examples/streaming                    # mocks only, no key, no network
//	go run ./examples/streaming -cli claude        # stream from a real CLI
//	go run ./examples/streaming -cli opencode -model opencode-go/deepseek-v4-flash
//
// Three properties are worth watching for, because they are what make streaming
// safe to adopt rather than a second code path to maintain:
//
//  1. Every stage runs before the first delta. Streaming is a property of the
//     final model call only, which is why no Stage needed changing for it.
//  2. A short-circuited turn yields exactly one delta. A caller never needs to
//     ask "did a rule answer this?" — it looks like any other stream.
//  3. A client that cannot stream still works through RunStream, adapted into a
//     single delta. Callers never branch on which kind of client they hold.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/JMjirapat/tokipe"
	"github.com/JMjirapat/tokipe/config"
	"github.com/JMjirapat/tokipe/metrics"
	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/preprocess"
	"github.com/JMjirapat/tokipe/providers/cli"
	"github.com/JMjirapat/tokipe/providers/mock"
)

func main() {
	which := flag.String("cli", "", "stream from a real CLI: claude | codex | opencode (default: mocks)")
	model := flag.String("model", "", "model for the opencode preset")
	flag.Parse()

	rec := metrics.NewInMemory()

	streamer, plain, err := backends(*which, *model)
	if err != nil {
		log.Fatal(err)
	}

	kit := tokipe.New(streamer,
		config.WithMetrics(rec),
		config.WithPreprocess(shaRule{}),
		config.WithCacheAlignment(),
	)

	fmt.Printf("backend: %s\n%s\n", nameOf(streamer), strings.Repeat("─", 64))

	for _, turn := range turns() {
		fmt.Printf("\n▸ %s\n  ", turn.label)

		start := time.Now()
		seq, err := kit.RunStream(context.Background(), turn.req())
		if err != nil {
			log.Fatalf("RunStream: %v", err)
		}

		// This loop is the whole consumer API: range over deltas, print as they
		// arrive. `for ... range` over a function is Go 1.23's iter support.
		var (
			deltas  int
			firstAt time.Duration
			usage   *pipeline.Usage
		)
		for d, err := range seq {
			if err != nil {
				fmt.Printf("\n  stream error: %v\n", err)
				break
			}
			if d.Text != "" {
				if deltas == 0 {
					firstAt = time.Since(start)
				}
				deltas++
				fmt.Print(strings.ReplaceAll(d.Text, "\n", "\n  "))
			}
			if d.Usage != nil {
				usage = d.Usage
			}
		}

		fmt.Printf("\n  └ %d delta(s), first after %v, total %v\n",
			deltas, firstAt.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
		if usage != nil && (usage.InputTokens > 0 || usage.OutputTokens > 0) {
			fmt.Printf("    usage: in=%d out=%d cache_read=%d\n",
				usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens)
		}
	}

	// Same pipeline, a client that cannot stream: RunStream still works.
	fmt.Printf("\n%s\n▸ a non-streaming client, through the same RunStream\n  ", strings.Repeat("─", 64))
	plainKit := tokipe.New(plain, config.WithCacheAlignment())
	seq, err := plainKit.RunStream(context.Background(), &pipeline.Request{Query: "and this?"})
	if err != nil {
		log.Fatalf("RunStream: %v", err)
	}
	resp, err := pipeline.Collect(seq)
	if err != nil {
		log.Fatalf("collect: %v", err)
	}
	fmt.Printf("%s\n  └ adapted into one delta by pipeline.StreamOne\n", resp.Content)

	fmt.Printf("\nmetrics: %v\n", rec.Snapshot())
	fmt.Println("\nOK — stages ran before every stream, and both client kinds worked unchanged.")
}

type turn struct {
	label string
	req   func() *pipeline.Request
}

func turns() []turn {
	return []turn{
		{"a streamed answer", func() *pipeline.Request {
			return &pipeline.Request{
				Query: "Count from 1 to 5, one number per line. No other text.",
				Messages: []pipeline.Message{
					{Role: "system", Content: "You are terse. Obey literally.", Static: true},
				},
			}
		}},
		{"short-circuited — one delta, no model", func() *pipeline.Request {
			return &pipeline.Request{Query: "validate sha 3f2a1c9d4e5b6a7c8d9e0f1a2b3c4d5e6f7a8b9c"}
		}},
	}
}

// backends returns the streaming client to demonstrate and a deliberately
// non-streaming one to prove the fallback.
func backends(which, model string) (pipeline.ModelClient, pipeline.ModelClient, error) {
	plain := mock.New("mock-plain", "a single buffered response")

	if which == "" {
		// mock.StreamClient splits its content so there is something to watch.
		return mock.NewStream("mock-stream", "", "1\n", "2\n", "3\n", "4\n", "5"), plain, nil
	}

	var cfg cli.Config
	switch which {
	case "claude":
		cfg = cli.ClaudeStreamPreset("")
	case "codex":
		cfg = cli.CodexStreamPreset(".") // codex requires a trusted directory
	case "opencode":
		cfg = cli.OpenCodeStreamPreset("", model)
	default:
		return nil, nil, fmt.Errorf("unknown -cli %q: want claude, codex, or opencode", which)
	}

	if _, err := exec.LookPath(cfg.Command); err != nil {
		fmt.Printf("%q is not on PATH — falling back to mocks.\n", cfg.Command)
		return mock.NewStream("mock-stream", "", "1\n", "2\n", "3"), plain, nil
	}
	cfg.Timeout = 3 * time.Minute

	client, err := cli.New(cfg)
	if err != nil {
		return nil, nil, err
	}
	return client, plain, nil
}

func nameOf(c pipeline.ModelClient) string {
	if c == nil {
		return "none"
	}
	return c.Name()
}

// shaRule answers a deterministic question without a model, so the example can
// show what a short-circuited stream looks like.
type shaRule struct{}

func (shaRule) Name() string { return "git_sha_validator" }

func (shaRule) CanHandle(req *pipeline.Request) bool {
	return strings.HasPrefix(strings.ToLower(req.Query), "validate sha ")
}

func (shaRule) Handle(req *pipeline.Request) (*pipeline.Response, error) {
	sha := strings.TrimSpace(req.Query[len("validate sha "):])
	valid := len(sha) == 40 && strings.IndexFunc(sha, func(r rune) bool {
		return !strings.ContainsRune("0123456789abcdef", r)
	}) < 0
	return &pipeline.Response{
		Content:   fmt.Sprintf("sha %s is valid: %v", sha, valid),
		ModelUsed: "none",
	}, nil
}

// Compile-time proof the rule satisfies the interface config.WithPreprocess wants.
var _ preprocess.Rule = shaRule{}
