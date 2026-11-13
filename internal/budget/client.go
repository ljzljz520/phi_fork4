package budget

import (
	"context"
	"iter"
	"sync"

	"github.com/pulseaiclub/phi/internal/llm"
	"github.com/pulseaiclub/phi/internal/util"
)

// NewClient wraps inner so every Stream and Compact call reserves quota
// before touching the network and settles against reported usage afterwards.
// A nil budget returns inner unchanged: unconfigured sessions keep the exact
// client they would have had, with no wrapper in the call path.
func NewClient(inner llm.ChatClient, b *Budget, estimate int) llm.ChatClient {
	if b == nil {
		return inner
	}
	if estimate <= 0 {
		estimate = DefaultOutputEstimate
	}
	return &Client{inner: inner, budget: b, estimate: estimate}
}

// Client is the quota-enforcing ChatClient wrapper. It is exported mainly so
// tests and diagnostics can recognize a guarded client.
type Client struct {
	inner    llm.ChatClient
	budget   *Budget
	estimate int
}

// Stream reserves one chat turn, attaches the retry hook so every actual HTTP
// retry reserves its own request slot, then reconciles the reservation exactly
// once no matter how the sequence ends (done, error, cancel or early break).
func (c *Client) Stream(ctx context.Context, messages []llm.Message) iter.Seq2[llm.StreamEvent, error] {
	res, err := c.budget.Reserve(KindChat, c.estimate)
	if err != nil {
		// Refusal happens before any inner call: yield the sentinel directly so
		// errors.Is keeps working all the way up to the engine and UI.
		return func(yield func(llm.StreamEvent, error) bool) {
			yield(llm.StreamEvent{}, err)
		}
	}
	// Already canceled before dispatch: nothing reaches the wire, so return
	// the whole hold and charge no request instead of faking an attempt.
	if ctxErr := ctx.Err(); ctxErr != nil {
		res.Release()
		return func(yield func(llm.StreamEvent, error) bool) {
			yield(llm.StreamEvent{}, ctxErr)
		}
	}

	hookCtx := util.WithRetryHook(ctx, func(int) error {
		return res.ReserveAttempt()
	})

	return func(yield func(llm.StreamEvent, error) bool) {
		var (
			once     sync.Once
			produced bool
			usage    llm.Usage
			hasUsage bool
		)
		finish := func() {
			once.Do(func() {
				if hasUsage {
					res.Settle(usage)
				} else {
					res.SettleUnknown(produced)
				}
			})
		}

		for event, streamErr := range c.inner.Stream(hookCtx, messages) {
			if streamErr != nil {
				finish()
				yield(llm.StreamEvent{}, streamErr)
				return
			}

			if event.Final != nil && event.Final.Usage.ContextTokens() > 0 {
				usage = event.Final.Usage
				hasUsage = true
			}
			if event.Usage.ContextTokens() > 0 {
				usage = event.Usage
				hasUsage = true
			}
			if eventHasOutput(event) {
				produced = true
			}

			switch event.Type {
			case llm.StreamEventTypeDone:
				finish()
				if !yield(event, nil) {
					return
				}
				return
			case llm.StreamEventTypeError:
				finish()
				yield(event, nil)
				return
			}

			if !yield(event, nil) {
				// Consumer broke early (user cancel / stop): no further events
				// can arrive, so reconcile as unknown unless usage was reported.
				finish()
				return
			}
		}
		// The inner iterator ended without a terminal event.
		finish()
	}
}

// Compact reserves one summarization call sized by the request's own output
// cap and settles against the non-streaming usage, falling back to the
// unknown-usage rule when the provider omits it.
func (c *Client) Compact(ctx context.Context, req llm.CompactRequest) (llm.CompactResult, error) {
	estimate := req.MaxTokens
	if estimate <= 0 {
		estimate = c.estimate
	}
	res, err := c.budget.Reserve(KindCompact, estimate)
	if err != nil {
		return llm.CompactResult{}, err
	}
	// Pre-canceled dispatch never reaches the provider: release in full.
	if ctxErr := ctx.Err(); ctxErr != nil {
		res.Release()
		return llm.CompactResult{}, ctxErr
	}

	hookCtx := util.WithRetryHook(ctx, func(int) error {
		return res.ReserveAttempt()
	})

	result, callErr := c.inner.Compact(hookCtx, req)
	if callErr != nil {
		// A non-streaming failure surfaces before any output exists.
		res.SettleUnknown(false)
		return result, callErr
	}
	if result.UsageReported() {
		res.Settle(result.Usage)
	} else {
		res.SettleUnknown(result.Text != "")
	}
	return result, nil
}

// eventHasOutput reports whether the model observably generated something on
// this event: visible text, reasoning, or tool-call deltas.
func eventHasOutput(event llm.StreamEvent) bool {
	if event.Final != nil {
		if event.Final.Content != "" || event.Final.ReasoningContent != "" || len(event.Final.ToolCalls) > 0 {
			return true
		}
	}
	return event.Delta.Content != "" ||
		event.Delta.ReasoningContent != "" ||
		len(event.Delta.ToolCalls) > 0
}
