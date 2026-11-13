package agent

import (
	"context"
	"errors"
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pulseaiclub/phi/internal/budget"
	"github.com/pulseaiclub/phi/internal/llm"
	"github.com/pulseaiclub/phi/internal/permission"
	"github.com/pulseaiclub/phi/internal/session"
	"github.com/pulseaiclub/phi/internal/tools/tooldef"
)

func assistantCount(messages []llm.Message) int {
	n := 0
	for _, m := range messages {
		if m.Role == llm.RoleAssistant {
			n++
		}
	}
	return n
}

func TestLoopBudgetExhaustionStopsGeneration(t *testing.T) {
	sess, err := NewSession(WithCwd(t.TempDir()))
	require.NoError(t, err)

	b := budget.New(budget.Limits{MaxRequests: 1, MaxOutputTokens: 100000})
	stub := &stubChatClient{streams: []func(context.Context, []llm.Message) iter.Seq2[llm.StreamEvent, error]{
		stubMessageStream(llm.Message{
			Role:    llm.RoleAssistant,
			Content: "first",
			Usage:   llm.Usage{CompletionTokens: 10, TotalTokens: 20},
		}),
	}}
	engine, err := NewEngine(
		llm.ModelConfig{Name: "stub", MaxTokens: 100},
		sess,
		WithGate(permission.AllowAll{}),
		WithTools([]tooldef.Tool{}),
		WithBudget(b),
	)
	require.NoError(t, err)
	engine.client = budget.NewClient(stub, b, 100)

	require.NoError(t, runLoop(t, engine, t.Context(), "hello", LoopOpts{}))
	require.True(t, b.Snapshot().Exhausted)
	assistantsAfterFirst := assistantCount(sess.BuildContext())
	require.Equal(t, 1, assistantsAfterFirst)

	loopErr := runLoop(t, engine, t.Context(), "again", LoopOpts{})
	require.Error(t, loopErr)
	assert.True(t, errors.Is(loopErr, budget.ErrExhausted))
	assert.Len(t, stub.calls, 1, "the exhausted turn must never reach the inner client")
	assert.Equal(
		t,
		assistantsAfterFirst,
		assistantCount(sess.BuildContext()),
		"a failed turn adds no assistant message; the conversation stays resumable",
	)
}

func TestSetModelPreservesBudget(t *testing.T) {
	sess, err := NewSession(WithCwd(t.TempDir()))
	require.NoError(t, err)

	b := budget.New(budget.Limits{MaxRequests: 5, MaxOutputTokens: 100000})
	engine, err := NewEngine(
		llm.ModelConfig{Name: "stub", MaxTokens: 100},
		sess,
		WithTools([]tooldef.Tool{}),
		WithBudget(b),
	)
	require.NoError(t, err)

	r, err := b.Reserve(budget.KindChat, 100)
	require.NoError(t, err)
	r.Settle(llm.Usage{CompletionTokens: 40})
	settled := b.Snapshot().SettledRequests

	engine.SetModel(llm.ModelConfig{Name: "stub-2", MaxTokens: 200})

	assert.Same(t, b, engine.Budget(), "model switch must not replace or reset the session budget")
	assert.Equal(t, settled, engine.Budget().Snapshot().SettledRequests)

	_, ok := engine.client.(*budget.Client)
	require.True(t, ok, "rebound client stays quota-wrapped")
}

func TestRunCompactChargesSharedBudget(t *testing.T) {
	sess, err := NewSession(WithCwd(t.TempDir()))
	require.NoError(t, err)
	require.NoError(t, sess.Append(
		llm.Message{Role: llm.RoleUser, Content: compactionSeed},
		llm.Message{Role: llm.RoleAssistant, Content: compactionSeed},
		llm.Message{Role: llm.RoleUser, Content: compactionSeed},
	))

	b := budget.New(budget.Limits{MaxRequests: 5, MaxOutputTokens: 100000})
	engine, err := NewEngine(
		llm.ModelConfig{Name: "fake", BaseURL: "http://unused.test", ContextWindow: 200_000, MaxTokens: 500},
		sess,
		WithGate(permission.AllowAll{}),
		WithTools([]tooldef.Tool{}),
		WithBudget(b),
	)
	require.NoError(t, err)
	engine.client = budget.NewClient(fakeCompactClient{compact: func(context.Context) error { return nil }}, b, 500)

	did, compactErr := engine.runCompact(t.Context(), func(session.Event, error) bool { return true }, 0, true)
	require.NoError(t, compactErr)
	require.True(t, did)

	snap := b.Snapshot()
	assert.Equal(t, int64(1), snap.SettledRequests, "the compaction call is counted")
	assert.Equal(t, int64(500), snap.SettledOutputTokens, "unknown compact usage charges the request cap")
	assert.Equal(t, int64(0), snap.ReservedRequests)
}

func TestNilBudgetLeavesClientUnwrapped(t *testing.T) {
	sess, err := NewSession(WithCwd(t.TempDir()))
	require.NoError(t, err)
	engine, err := NewEngine(
		llm.ModelConfig{Name: "stub"},
		sess,
		WithTools([]tooldef.Tool{}),
	)
	require.NoError(t, err)
	assert.Nil(t, engine.Budget())
	_, ok := engine.client.(*budget.Client)
	assert.False(t, ok, "unguarded engines keep the raw provider client")
}
