package editor

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pulseaiclub/phi/internal/budget"
	"github.com/pulseaiclub/phi/internal/components"
	"github.com/pulseaiclub/phi/internal/tui/controller"
)

func TestBudgetMsgStoresLatestSnapshot(t *testing.T) {
	e := newTestEditor(t)
	require.Nil(t, e.budgetSnap)

	snap := &budget.Snapshot{
		Limits:          budget.Limits{MaxRequests: 500, MaxOutputTokens: 200000},
		SettledRequests: 12,
	}
	e.Update(controller.BudgetMsg{Snapshot: snap})
	require.Same(t, snap, e.budgetSnap)

	// A session without a budget clears the stored snapshot.
	e.Update(controller.BudgetMsg{})
	require.Nil(t, e.budgetSnap)
}

func budgetPanelBody(t *testing.T, e *Editor) string {
	t.Helper()
	surface, ok := e.overlays.DrawBottom(
		components.DrawContext{Max: components.Size{Width: 60, Height: 14}},
		60, 14,
	)
	require.True(t, ok)
	return components.SurfaceText(surface)
}

func TestExhaustedCompletionOpensBudgetPanelWithoutRetry(t *testing.T) {
	e := newTestEditor(t)
	snap := &budget.Snapshot{
		Limits:              budget.Limits{MaxRequests: 3, MaxOutputTokens: 1000},
		SettledRequests:     3,
		SettledOutputTokens: 1000,
		Exhausted:           true,
	}
	e.overlays.Apply(controller.OverlayMsg{
		Kind:           controller.OverlayBudgetAsk,
		ErrorText:      "budget exhausted: request limit reached (3/3)",
		BudgetSnapshot: snap,
	})

	require.True(t, e.overlays.ErrorActive())
	body := budgetPanelBody(t, e)
	assert.Contains(t, body, "Budget exhausted")
	assert.Contains(t, body, "Requests: 3/3 used")
	assert.Contains(t, body, "Output tokens: 1.0k/1.0k used")
	assert.NotContains(t, body, "Retry")
}

func TestOrdinaryCompletionStillOpensRetryPanel(t *testing.T) {
	e := newTestEditor(t)
	e.overlays.Apply(controller.OverlayMsg{
		Kind:      controller.OverlayErrorAsk,
		ErrorText: "provider down",
	})

	require.True(t, e.overlays.ErrorActive())
	body := budgetPanelBody(t, e)
	assert.Contains(t, body, "Error")
	assert.Contains(t, body, "Retry")
	assert.NotContains(t, body, "Budget exhausted")
}

// TestLoopFinishedRoutesByExhaustionMarker covers the editor switch itself:
// the zero-value controller's current generation is 0, so Gen 0 is current.
func TestLoopFinishedRoutesByExhaustionMarker(t *testing.T) {
	snap := &budget.Snapshot{
		Limits:          budget.Limits{MaxRequests: 3},
		SettledRequests: 3,
		Exhausted:       true,
	}

	budgetEditor := newTestEditor(t)
	budgetEditor.Update(controller.LoopFinishedMsg{
		Gen:             0,
		ErrorText:       "budget exhausted: request limit reached (3/3)",
		BudgetExhausted: true,
		BudgetSnapshot:  snap,
	})
	require.True(t, budgetEditor.overlays.ErrorActive())
	assert.Contains(t, budgetPanelBody(t, budgetEditor), "Budget exhausted")

	plainEditor := newTestEditor(t)
	plainEditor.Update(controller.LoopFinishedMsg{Gen: 0, ErrorText: "provider down"})
	require.True(t, plainEditor.overlays.ErrorActive())
	body := budgetPanelBody(t, plainEditor)
	assert.Contains(t, body, "Retry")
	assert.NotContains(t, body, "Budget exhausted")
}
