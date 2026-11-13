package budget

import (
	"context"
	"errors"
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pulseaiclub/phi/internal/llm"
	"github.com/pulseaiclub/phi/internal/util"
)

func probeRetryHook(ctx context.Context) util.RetryHook {
	return util.RetryHookFromContext(ctx)
}

// fakeChatClient records calls and replays scripted streams/compact results.
type fakeChatClient struct {
	streamCalls  int
	compactCalls int
	stream       func(context.Context, []llm.Message) iter.Seq2[llm.StreamEvent, error]
	compact      func(context.Context, llm.CompactRequest) (llm.CompactResult, error)
	hookProbe    func(attempt int)
}

func (f *fakeChatClient) Stream(ctx context.Context, messages []llm.Message) iter.Seq2[llm.StreamEvent, error] {
	f.streamCalls++
	if f.stream != nil {
		return f.stream(ctx, messages)
	}
	return func(yield func(llm.StreamEvent, error) bool) {}
}

func (f *fakeChatClient) Compact(ctx context.Context, req llm.CompactRequest) (llm.CompactResult, error) {
	f.compactCalls++
	if f.compact != nil {
		return f.compact(ctx, req)
	}
	return llm.CompactResult{}, nil
}

func doneEvent(content string, usage llm.Usage) llm.StreamEvent {
	return llm.StreamEvent{
		Type:  llm.StreamEventTypeDone,
		Final: &llm.Message{Role: llm.RoleAssistant, Content: content, Usage: usage},
	}
}

func TestNewClientNilBudgetReturnsInner(t *testing.T) {
	inner := &fakeChatClient{}
	assert.Same(t, llm.ChatClient(inner), NewClient(inner, nil, 100))
}

func TestStreamPreCanceledContextReleasesWithoutCharging(t *testing.T) {
	inner := &fakeChatClient{}
	b := New(Limits{MaxRequests: 2, MaxOutputTokens: 10000})
	c := NewClient(inner, b, 100)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var sawErr error
	for _, err := range c.Stream(ctx, nil) {
		if err != nil {
			sawErr = err
		}
	}
	require.ErrorIs(t, sawErr, context.Canceled)
	assert.Equal(t, 0, inner.streamCalls, "a pre-canceled turn must never reach the inner client")

	snap := b.Snapshot()
	assert.Equal(t, int64(0), snap.SettledRequests, "never-dispatched requests are not counted")
	assert.Equal(t, int64(0), snap.ReservedRequests)
	assert.Equal(t, int64(0), snap.SettledOutputTokens)
	assert.Equal(t, int64(0), snap.ReservedOutputTokens)
	assert.Equal(t, int64(2), snap.RemainingRequests(), "the full hold must be reusable")
}

func TestCompactPreCanceledContextReleasesWithoutCharging(t *testing.T) {
	inner := &fakeChatClient{}
	b := New(Limits{MaxRequests: 2, MaxOutputTokens: 10000})
	c := NewClient(inner, b, 100)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := c.Compact(ctx, llm.CompactRequest{MaxTokens: 800})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, inner.compactCalls)

	snap := b.Snapshot()
	assert.Equal(t, int64(0), snap.SettledRequests)
	assert.Equal(t, int64(0), snap.ReservedRequests)
	assert.Equal(t, int64(0), snap.ReservedOutputTokens)

	// Headroom came back: a healthy dispatch afterward still fits.
	res, err := b.Reserve(KindCompact, 800)
	require.NoError(t, err)
	res.Release()
}

func TestStreamExhaustionNeverTouchesInner(t *testing.T) {
	inner := &fakeChatClient{}
	b := NewWithState(Limits{MaxRequests: 1, MaxOutputTokens: 10000}, 1, 0)
	c := NewClient(inner, b, 100)

	var sawErr error
	for ev, err := range c.Stream(t.Context(), nil) {
		if err != nil {
			sawErr = err
			continue
		}
		if ev.Type == llm.StreamEventTypeError {
			sawErr = errors.New(ev.Err)
		}
	}
	require.Error(t, sawErr)
	assert.True(t, errors.Is(sawErr, ErrExhausted))
	assert.Equal(t, 0, inner.streamCalls)
}

func TestStreamSettlesReportedUsage(t *testing.T) {
	inner := &fakeChatClient{
		stream: func(_ context.Context, _ []llm.Message) iter.Seq2[llm.StreamEvent, error] {
			return func(yield func(llm.StreamEvent, error) bool) {
				yield(llm.StreamEvent{Type: llm.StreamEventTypeDelta, Delta: llm.StreamDelta{Content: "hi"}}, nil)
				yield(doneEvent("hi", llm.Usage{CompletionTokens: 120, TotalTokens: 200}), nil)
			}
		},
	}
	b := New(Limits{MaxRequests: 5, MaxOutputTokens: 10000})
	c := NewClient(inner, b, 4000)

	for range c.Stream(t.Context(), nil) {
	}

	snap := b.Snapshot()
	assert.Equal(t, int64(1), snap.SettledRequests)
	assert.Equal(t, int64(120), snap.SettledOutputTokens, "actual completion wins over the 4000 hold")
	assert.Equal(t, int64(0), snap.ReservedRequests)
}

func TestStreamCancelWithPartialOutputChargesEstimate(t *testing.T) {
	inner := &fakeChatClient{
		stream: func(_ context.Context, _ []llm.Message) iter.Seq2[llm.StreamEvent, error] {
			return func(yield func(llm.StreamEvent, error) bool) {
				yield(llm.StreamEvent{Type: llm.StreamEventTypeDelta, Delta: llm.StreamDelta{Content: "partial"}}, nil)
				// Consumer stops; remaining events never arrive.
			}
		},
	}
	b := New(Limits{MaxRequests: 5, MaxOutputTokens: 10000})
	c := NewClient(inner, b, 4000)

	count := 0
	for range c.Stream(t.Context(), nil) {
		count++
		if count == 1 {
			break
		}
	}

	snap := b.Snapshot()
	assert.Equal(t, int64(1), snap.SettledRequests)
	assert.Equal(t, int64(4000), snap.SettledOutputTokens, "unknown usage after partial output charges the hold")
	assert.Equal(t, int64(0), snap.ReservedOutputTokens)
}

func TestStreamErrorBeforeOutputChargesNoOutput(t *testing.T) {
	boom := errors.New("connection refused")
	inner := &fakeChatClient{
		stream: func(_ context.Context, _ []llm.Message) iter.Seq2[llm.StreamEvent, error] {
			return func(yield func(llm.StreamEvent, error) bool) {
				yield(llm.StreamEvent{}, boom)
			}
		},
	}
	b := New(Limits{MaxRequests: 5, MaxOutputTokens: 10000})
	c := NewClient(inner, b, 4000)

	var got error
	for _, err := range c.Stream(t.Context(), nil) {
		if err != nil {
			got = err
		}
	}
	assert.ErrorIs(t, got, boom)

	snap := b.Snapshot()
	assert.Equal(t, int64(1), snap.SettledRequests, "dispatched request still counts")
	assert.Equal(t, int64(0), snap.SettledOutputTokens)
}

func TestStreamErrorEventTypeSettles(t *testing.T) {
	inner := &fakeChatClient{
		stream: func(_ context.Context, _ []llm.Message) iter.Seq2[llm.StreamEvent, error] {
			return func(yield func(llm.StreamEvent, error) bool) {
				yield(llm.StreamEvent{Type: llm.StreamEventTypeDelta, Delta: llm.StreamDelta{Content: "x"}}, nil)
				yield(llm.StreamEvent{Type: llm.StreamEventTypeError, Err: "mid-stream failure"}, nil)
			}
		},
	}
	b := New(Limits{MaxRequests: 5, MaxOutputTokens: 10000})
	c := NewClient(inner, b, 4000)

	for range c.Stream(t.Context(), nil) {
	}

	snap := b.Snapshot()
	assert.Equal(t, int64(1), snap.SettledRequests)
	assert.Equal(t, int64(4000), snap.SettledOutputTokens, "partial output then error: fail-closed estimate")
}

func TestCompactSettlementBranches(t *testing.T) {
	t.Run("reported usage", func(t *testing.T) {
		inner := &fakeChatClient{
			compact: func(_ context.Context, _ llm.CompactRequest) (llm.CompactResult, error) {
				return llm.CompactResult{
					Text:  "summary",
					Usage: llm.Usage{CompletionTokens: 80, TotalTokens: 100},
				}, nil
			},
		}
		b := New(Limits{MaxRequests: 5, MaxOutputTokens: 10000})
		c := NewClient(inner, b, 4000)

		_, err := c.Compact(t.Context(), llm.CompactRequest{MaxTokens: 2000})
		require.NoError(t, err)

		snap := b.Snapshot()
		assert.Equal(t, int64(80), snap.SettledOutputTokens)
		assert.Equal(t, int64(1), snap.SettledRequests)
	})

	t.Run("unknown usage with text charges estimate", func(t *testing.T) {
		inner := &fakeChatClient{
			compact: func(_ context.Context, _ llm.CompactRequest) (llm.CompactResult, error) {
				return llm.CompactResult{Text: "summary"}, nil
			},
		}
		b := New(Limits{MaxRequests: 5, MaxOutputTokens: 10000})
		c := NewClient(inner, b, 4000)

		_, err := c.Compact(t.Context(), llm.CompactRequest{MaxTokens: 2000})
		require.NoError(t, err)
		assert.Equal(t, int64(2000), b.Snapshot().SettledOutputTokens, "uses the compact request cap")
	})

	t.Run("error before output charges zero output", func(t *testing.T) {
		inner := &fakeChatClient{
			compact: func(_ context.Context, _ llm.CompactRequest) (llm.CompactResult, error) {
				return llm.CompactResult{}, errors.New("500")
			},
		}
		b := New(Limits{MaxRequests: 5, MaxOutputTokens: 10000})
		c := NewClient(inner, b, 4000)

		_, err := c.Compact(t.Context(), llm.CompactRequest{MaxTokens: 2000})
		require.Error(t, err)
		snap := b.Snapshot()
		assert.Equal(t, int64(0), snap.SettledOutputTokens)
		assert.Equal(t, int64(1), snap.SettledRequests)
	})

	t.Run("exhausted refuses before inner", func(t *testing.T) {
		inner := &fakeChatClient{}
		b := New(Limits{MaxRequests: 1, MaxOutputTokens: 10000})
		c := NewClient(inner, b, 100)
		first, err := b.Reserve(KindChat, 100)
		require.NoError(t, err)
		defer first.Release()

		_, err = c.Compact(t.Context(), llm.CompactRequest{MaxTokens: 100})
		assert.ErrorIs(t, err, ErrExhausted)
		assert.Equal(t, 0, inner.compactCalls)
	})
}

func TestRetryHookReservesEachAttempt(t *testing.T) {
	inner := &fakeChatClient{
		stream: func(ctx context.Context, _ []llm.Message) iter.Seq2[llm.StreamEvent, error] {
			return func(yield func(llm.StreamEvent, error) bool) {
				// Simulate two HTTP retries under the same logical call by
				// driving the hook the util layer would invoke.
				if hook := probeRetryHook(ctx); hook != nil {
					require.NoError(t, hook(1))
					require.NoError(t, hook(2))
				}
				yield(doneEvent("ok", llm.Usage{CompletionTokens: 10, TotalTokens: 20}), nil)
			}
		},
	}
	b := New(Limits{MaxRequests: 5, MaxOutputTokens: 10000})
	c := NewClient(inner, b, 4000)

	for range c.Stream(t.Context(), nil) {
	}

	snap := b.Snapshot()
	assert.Equal(t, int64(3), snap.SettledRequests, "initial attempt plus two retries")
	assert.Equal(t, int64(10), snap.SettledOutputTokens)
}

func TestRetryHookExhaustionStopsAttempts(t *testing.T) {
	inner := &fakeChatClient{
		stream: func(ctx context.Context, _ []llm.Message) iter.Seq2[llm.StreamEvent, error] {
			return func(yield func(llm.StreamEvent, error) bool) {
				hook := probeRetryHook(ctx)
				require.NotNil(t, hook)
				err := hook(1) // cap is 1: the retry must be refused
				yield(llm.StreamEvent{}, err)
			}
		},
	}
	b := New(Limits{MaxRequests: 1, MaxOutputTokens: 10000})
	c := NewClient(inner, b, 100)

	var got error
	for _, err := range c.Stream(t.Context(), nil) {
		if err != nil {
			got = err
		}
	}
	assert.ErrorIs(t, got, ErrExhausted)
	// The refused retry still leaves one dispatched attempt settled.
	snap := b.Snapshot()
	assert.Equal(t, int64(1), snap.SettledRequests)
	assert.True(t, snap.Exhausted)
}
