package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cli "github.com/pulseaiclub/pli"

	"github.com/pulseaiclub/phi/internal/agent"
	"github.com/pulseaiclub/phi/internal/budget"
	"github.com/pulseaiclub/phi/internal/llm"
	"github.com/pulseaiclub/phi/internal/permission"
)

// countingSSEServer returns one completion per request with a usage frame and
// records how many requests actually reached the wire.
func countingSSEServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(
			w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\n",
		)
		_, _ = fmt.Fprint(
			w,
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":7,\"total_tokens\":12}}\n\n",
		)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func budgetTestEngine(t *testing.T, baseURL string, b *budget.Budget) *agent.Engine {
	t.Helper()
	sess, err := agent.NewSession(agent.WithCwd(t.TempDir()))
	require.NoError(t, err)
	engine, err := agent.NewEngine(
		llm.ModelConfig{Name: "fake", BaseURL: baseURL, APIKey: "test"},
		sess,
		agent.WithGate(permission.AllowAll{}),
		agent.WithBudget(b),
	)
	require.NoError(t, err)
	return engine
}

func TestRunOptionsBudgetFlags(t *testing.T) {
	f, _, err := runCommand.Parse([]string{
		"-p", "hi", "--max-requests", "5", "--max-output-tokens", "1000",
	})
	require.NoError(t, err)
	opts, err := runOptionsFromFlags(f)
	require.NoError(t, err)
	assert.Equal(t, 5, opts.maxRequests)
	assert.Equal(t, 1000, opts.maxOutputTokens)

	// Absent flags stay at the -1 sentinel so config limits apply untouched.
	f, _, err = runCommand.Parse([]string{"-p", "hi"})
	require.NoError(t, err)
	opts, err = runOptionsFromFlags(f)
	require.NoError(t, err)
	assert.Equal(t, -1, opts.maxRequests)
	assert.Equal(t, -1, opts.maxOutputTokens)

	// Explicit zero is accepted: it means unlimited on that dimension.
	f, _, err = runCommand.Parse([]string{"-p", "hi", "--max-requests", "0"})
	require.NoError(t, err)
	opts, err = runOptionsFromFlags(f)
	require.NoError(t, err)
	assert.Equal(t, 0, opts.maxRequests)
}

func TestRunOptionsBudgetFlagsRejectNegatives(t *testing.T) {
	for _, args := range [][]string{
		{"-p", "hi", "--max-requests", "-2"},
		{"-p", "hi", "--max-output-tokens", "-9"},
	} {
		f, _, err := runCommand.Parse(args)
		require.NoError(t, err)
		_, err = runOptionsFromFlags(f)
		var ue *cli.UsageError
		require.ErrorAs(t, err, &ue, "args %v", args)
		assert.Contains(t, err.Error(), "non-negative integer")
	}
}

func TestRunLoopBudgetExhaustionExitsTwoWithoutDispatch(t *testing.T) {
	var hits atomic.Int32
	server := countingSSEServer(t, &hits)
	defer server.Close()

	b := budget.New(budget.Limits{MaxRequests: 1, MaxOutputTokens: 100000})
	engine := budgetTestEngine(t, server.URL, b)

	var buf bytes.Buffer
	enc := &jsonlEncoder{out: &buf, enabled: true}

	// First turn settles one request.
	exit := runLoopWithEncoder(t.Context(), engine, runOptions{prompt: "one", jsonl: true}, enc)
	require.Equal(t, ExitOK, exit)
	require.Equal(t, int32(1), hits.Load())

	// Capture stderr for the human-facing distinction.
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stderr = w
	exit = runLoopWithEncoder(t.Context(), engine, runOptions{prompt: "two", jsonl: true}, enc)
	require.NoError(t, w.Close())
	os.Stderr = origStderr
	stderr, err := io.ReadAll(r)
	require.NoError(t, err)

	require.Equal(t, ExitBudget, exit)
	assert.Equal(t, int32(1), hits.Load(), "refusal happens before dispatch: no second HTTP request")
	assert.Contains(t, string(stderr), "budget exhausted:")
	assert.NotContains(t, string(stderr), "error:")

	events := decodeJSONL(t, buf.Bytes())

	var ready, exhausted, budgetErr, doneExhausted int
	var sawDimension, sawKind bool
	for _, ev := range events {
		switch ev["type"] {
		case "budget":
			switch ev["phase"] {
			case "ready":
				ready++
			case "exhausted":
				exhausted++
				sawDimension = ev["dimension"] == "requests"
				snap := ev["snapshot"].(map[string]any)
				reqs := snap["requests"].(map[string]any)
				assert.Equal(t, float64(1), reqs["settled"])
				assert.Equal(t, true, snap["exhausted"])
			}
		case "error":
			if ev["kind"] == "budget_exhausted" {
				budgetErr++
				sawKind = true
				assert.Contains(t, ev["budget"].(map[string]any), "outputTokens")
			}
		case "done":
			if int(ev["exitCode"].(float64)) == ExitBudget {
				doneExhausted++
			}
		}
	}
	assert.Equal(t, 2, ready, "one ready event per runLoop invocation")
	assert.Equal(t, 1, exhausted)
	assert.Equal(t, 1, budgetErr)
	assert.Equal(t, 1, doneExhausted)
	assert.True(t, sawDimension)
	assert.True(t, sawKind)
}

func TestRunLoopWithoutBudgetStaysOnLegacyContract(t *testing.T) {
	var hits atomic.Int32
	server := countingSSEServer(t, &hits)
	defer server.Close()

	engine := budgetTestEngine(t, server.URL, nil)
	var buf bytes.Buffer
	enc := &jsonlEncoder{out: &buf, enabled: true}

	exit := runLoopWithEncoder(t.Context(), engine, runOptions{prompt: "hi", jsonl: true}, enc)
	require.Equal(t, ExitOK, exit)
	require.Equal(t, int32(1), hits.Load())

	for _, ev := range decodeJSONL(t, buf.Bytes()) {
		assert.NotEqual(t, "budget", ev["type"], "unconfigured runs emit no budget events")
		if ev["type"] == "error" {
			_, hasKind := ev["kind"]
			assert.False(t, hasKind)
		}
	}
}

func TestJSONLEncoderBudgetShapes(t *testing.T) {
	var buf bytes.Buffer
	enc := &jsonlEncoder{out: &buf, enabled: true}
	snap := budget.Snapshot{
		Limits:              budget.Limits{MaxRequests: 500, MaxOutputTokens: 200000},
		SettledRequests:     10,
		ReservedRequests:    2,
		SettledOutputTokens: 40000,
	}
	enc.budgetEvent("ready", "", snap)
	enc.budgetEvent("exhausted", string(budget.DimensionRequests), snap)
	enc.budgetErrorEvent("budget exhausted: request limit reached (12/500)", snap)

	events := decodeJSONL(t, buf.Bytes())
	require.Len(t, events, 3)

	ready := events[0]
	assert.Equal(t, "budget", ready["type"])
	assert.Equal(t, "ready", ready["phase"])
	_, hasDimension := ready["dimension"]
	assert.False(t, hasDimension, "omitempty must hide the empty dimension")
	reqs := ready["snapshot"].(map[string]any)["requests"].(map[string]any)
	assert.Equal(t, float64(10), reqs["settled"])
	assert.Equal(t, float64(2), reqs["reserved"])
	assert.Equal(t, float64(500), reqs["limit"])

	exhausted := events[1]
	assert.Equal(t, "exhausted", exhausted["phase"])
	assert.Equal(t, "requests", exhausted["dimension"])

	errEvent := events[2]
	assert.Equal(t, "error", errEvent["type"])
	assert.Equal(t, "budget_exhausted", errEvent["kind"])
	wire := errEvent["budget"].(map[string]any)
	tokens := wire["outputTokens"].(map[string]any)
	assert.Equal(t, float64(200000), tokens["limit"])
	assert.Equal(t, false, wire["exhausted"])
}

func TestBudgetErrorViewPrefersTypedError(t *testing.T) {
	b := budget.New(budget.Limits{MaxRequests: 1})
	_, err := b.Reserve(budget.KindChat, 10)
	require.NoError(t, err)
	_, err = b.Reserve(budget.KindChat, 10)
	require.Error(t, err)

	snap, dimension := budgetErrorView(err, b)
	assert.Equal(t, "requests", dimension)
	assert.True(t, snap.Exhausted)

	// Fallback: an unrelated error still yields the live ledger, no dimension.
	fresh := budget.New(budget.Limits{MaxRequests: 9})
	snap, dimension = budgetErrorView(context.Canceled, fresh)
	assert.Empty(t, dimension)
	assert.False(t, snap.Exhausted)

	// Nil ledger degrades to the zero snapshot rather than panicking.
	snap, dimension = budgetErrorView(context.Canceled, nil)
	assert.Empty(t, dimension)
	assert.False(t, snap.Exhausted)
}

func decodeJSONL(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	out := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev map[string]any
		require.NoError(t, json.Unmarshal(line, &ev))
		out = append(out, ev)
	}
	return out
}
