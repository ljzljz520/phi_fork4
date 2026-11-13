package controller

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pulseaiclub/phi/internal/agent"
	"github.com/pulseaiclub/phi/internal/budget"
	"github.com/pulseaiclub/phi/internal/extension/loader"
	"github.com/pulseaiclub/phi/internal/llm"
	"github.com/pulseaiclub/phi/internal/project"
)

// budgetSSEServer answers every chat request with one content delta and a
// usage frame, mirroring the wire shape engine runner tests rely on.
func budgetSSEServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

// setupBudgetTest builds a controller whose global config carries a budget
// block. budgetYAML is appended verbatim and may be empty for the
// unconfigured case.
func setupBudgetTest(t *testing.T, serverURL, budgetYAML string) *EngineController {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, key := range []string{"PHI_MODEL", "PHI_API_KEY", "PHI_BASE_URL", "PHI_THINK_LEVEL"} {
		t.Setenv(key, "")
	}
	t.Setenv(loader.EnvExtensions, "off")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".phi"), 0o700))
	config := fmt.Sprintf(`models:
  - name: primary
    default: true
    api: openai
    api_key: test-key
    base_url: %q
  - name: secondary
    api: openai
    api_key: test-key
    base_url: %q
%s`, serverURL, serverURL, budgetYAML)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".phi", "config.yaml"), []byte(config), 0o600))

	proj, err := project.Discover(t.TempDir())
	require.NoError(t, err)
	ctrl, err := NewController(NewBus(nil), proj)
	require.NoError(t, err)
	t.Cleanup(ctrl.Close)
	return ctrl
}

// drainBudgets returns every populated budget snapshot published since the
// last drain, in arrival order.
func drainBudgets(bus *Bus) []budget.Snapshot {
	var out []budget.Snapshot
	for _, m := range bus.Drain() {
		if msg, ok := m.(BudgetMsg); ok && msg.Snapshot != nil {
			out = append(out, *msg.Snapshot)
		}
	}
	return out
}

func runBudgetTurn(t *testing.T, ctrl *EngineController, prompt string) {
	t.Helper()
	for ev, err := range ctrl.engine.Loop(t.Context(), prompt, agent.LoopOpts{}) {
		require.NoError(t, err)
		_ = ev
	}
}

func TestControllerBudgetPersistsAcrossResumeAndSplitsPerSession(t *testing.T) {
	srv := budgetSSEServer()
	defer srv.Close()
	ctrl := setupBudgetTest(t, srv.URL, "budget:\n  max_requests: 10\n  max_output_tokens: 100000\n")

	firstID := ctrl.SessionID()
	active := ctrl.engine.Budget()
	require.NotNil(t, active)
	require.Len(t, drainBudgets(ctrl.bus), 1, "attachment announces one initial snapshot")

	runBudgetTurn(t, ctrl, "hello")
	snaps := drainBudgets(ctrl.bus)
	require.NotEmpty(t, snaps)
	assert.Equal(t, int64(1), snaps[len(snaps)-1].SettledRequests)
	assert.Equal(t, int64(7), snaps[len(snaps)-1].SettledOutputTokens)

	statePath := ctrl.proj.BudgetFile(firstID)
	diskLimits, settledReq, settledOut, err := budget.LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, budget.Limits{MaxRequests: 10, MaxOutputTokens: 100000}, diskLimits)
	assert.Equal(t, int64(1), settledReq)
	assert.Equal(t, int64(7), settledOut)

	// Resume rebuilds the ledger from disk: accumulation continues, no reset.
	_, err = ctrl.Resume(firstID)
	require.NoError(t, err)
	assert.Equal(t, firstID, ctrl.SessionID())
	require.NotNil(t, ctrl.engine.Budget())
	assert.Equal(t, int64(1), ctrl.engine.Budget().Snapshot().SettledRequests)
	assert.Equal(t, int64(7), ctrl.engine.Budget().Snapshot().SettledOutputTokens)
	_ = drainBudgets(ctrl.bus)

	// A brand-new session gets its own zeroed ledger and state file.
	require.NoError(t, ctrl.NewSession())
	newID := ctrl.SessionID()
	assert.NotEqual(t, firstID, newID)
	require.NotNil(t, ctrl.engine.Budget())
	assert.Equal(t, int64(0), ctrl.engine.Budget().Snapshot().SettledRequests)
	assert.NotEqual(t, statePath, ctrl.proj.BudgetFile(newID))

	// Model switch rebuilds the client but must not touch the quota.
	prev := ctrl.engine.Budget()
	require.NoError(t, ctrl.SetModel("secondary"))
	assert.Same(t, prev, ctrl.engine.Budget())
	assert.Equal(t, int64(0), ctrl.engine.Budget().Snapshot().SettledRequests)

	// Forking starts a fresh ledger bound to the fork's own session id.
	forkID, err := ctrl.ForkSession()
	require.NoError(t, err)
	assert.NotEqual(t, newID, forkID)
	require.NotNil(t, ctrl.engine.Budget())
	assert.Equal(t, int64(0), ctrl.engine.Budget().Snapshot().SettledRequests)
	_, err = os.Stat(ctrl.proj.BudgetFile(forkID))
	assert.True(t, os.IsNotExist(err), "a fresh fork has not settled anything yet")
}

func TestSwitchedAwayBudgetStaysQuiet(t *testing.T) {
	srv := budgetSSEServer()
	defer srv.Close()
	ctrl := setupBudgetTest(t, srv.URL, "budget:\n  max_requests: 10\n")

	outgoing := ctrl.engine.Budget()
	require.NotNil(t, outgoing)
	require.NoError(t, ctrl.NewSession())
	assert.NotSame(t, outgoing, ctrl.engine.Budget())
	_ = ctrl.bus.Drain()

	// Simulate an in-flight child job settling the ledger it started on.
	_, err := outgoing.Reserve(budget.KindChat, 16)
	require.NoError(t, err)
	assert.Empty(t, drainBudgets(ctrl.bus), "old session's ledger must not repaint the footer")
}

func TestNoBudgetBlockLeavesEngineUnguarded(t *testing.T) {
	ctrl := newThinkingTestController(t, setupThinkingTest(t, llm.OpenAI, "http://127.0.0.1:9"))
	assert.Nil(t, ctrl.engine.Budget())
	for _, m := range ctrl.bus.Drain() {
		if msg, ok := m.(BudgetMsg); ok {
			assert.Nil(t, msg.Snapshot, "unconfigured sessions only publish the clear marker")
		}
	}
	_, err := os.Stat(ctrl.proj.BudgetDir())
	assert.True(t, os.IsNotExist(err))
}

func TestFinishLoopClassifiesBudgetExhaustion(t *testing.T) {
	b := budget.New(budget.Limits{MaxRequests: 3})
	bus := NewBus(nil)
	ctrl := &EngineController{bus: bus, streamGen: 5}
	ctrl.budget.Store(b)

	exhausted := &budget.ExhaustedError{
		Dimension: budget.DimensionRequests,
		Snapshot:  b.Snapshot(),
	}
	ctrl.finishLoop(t.Context(), 5, exhausted)

	finished := lastLoopFinished(t, bus)
	assert.True(t, finished.BudgetExhausted)
	require.NotNil(t, finished.BudgetSnapshot)
	assert.NotEmpty(t, finished.ErrorText)
	assert.False(t, finished.Cancelled)
	assert.False(t, retryReady(ctrl), "exhaustion is not a retriable provider error")

	// Ordinary provider errors keep the old contract: no marker, retry allowed.
	plainBus := NewBus(nil)
	plainCtrl := &EngineController{bus: plainBus, streamGen: 9}
	plainCtrl.finishLoop(t.Context(), 9, errors.New("provider down"))
	plain := lastLoopFinished(t, plainBus)
	assert.False(t, plain.BudgetExhausted)
	assert.Nil(t, plain.BudgetSnapshot)
	assert.Equal(t, "provider down", plain.ErrorText)
	assert.True(t, retryReady(plainCtrl))
}

func lastLoopFinished(t *testing.T, bus *Bus) LoopFinishedMsg {
	t.Helper()
	var got LoopFinishedMsg
	found := false
	for _, m := range bus.Drain() {
		if finished, ok := m.(LoopFinishedMsg); ok {
			got, found = finished, true
		}
	}
	require.True(t, found)
	return got
}
