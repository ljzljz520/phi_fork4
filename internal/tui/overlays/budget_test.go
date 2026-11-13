package overlays

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pulseaiclub/xui"

	"github.com/pulseaiclub/phi/internal/budget"
	"github.com/pulseaiclub/phi/internal/components"
	"github.com/pulseaiclub/phi/internal/tui/controller"
)

func exhaustedSnapshot() *budget.Snapshot {
	return &budget.Snapshot{
		Limits: budget.Limits{
			MaxRequests:     500,
			MaxOutputTokens: 200000,
		},
		SettledRequests:      499,
		SettledOutputTokens:  180000,
		ReservedRequests:     1,
		ReservedOutputTokens: 20000,
		Exhausted:            true,
	}
}

func beginBudgetAsk(o *Overlays) {
	o.Apply(controller.OverlayMsg{
		Kind:           controller.OverlayBudgetAsk,
		ErrorText:      "budget exhausted: request limit reached (500/500)",
		BudgetSnapshot: exhaustedSnapshot(),
	})
}

func TestBudgetAskRendersQuotaWithoutRetry(t *testing.T) {
	var replies []controller.ErrorReply
	var texts []string
	o := errorOverlays(&replies, &texts)
	beginBudgetAsk(o)

	require.NotNil(t, o.err)
	require.True(t, o.ErrorActive())
	require.True(t, o.err.budgetMode())

	surf := o.drawError(components.DrawContext{Max: components.Size{Width: 60, Height: 14}}, 60, 14)
	body := components.SurfaceText(surf)
	assert.Contains(t, body, "Budget exhausted")
	assert.Contains(t, body, "Requests: 500/500 used")
	assert.Contains(t, body, "Output tokens: 200k/200k used")
	assert.Contains(t, body, "Copy to Clipboard")
	assert.Contains(t, body, "Dismiss")
	assert.NotContains(t, body, "Retry", "quota refusal must not offer a retry action")
}

func TestBudgetAskUnlimitedDimensionSaysNoLimit(t *testing.T) {
	st := &errorAskState{
		message: "budget exhausted: output token limit reached",
		budget: &budget.Snapshot{
			Limits:              budget.Limits{MaxOutputTokens: 1000},
			SettledOutputTokens: 1000,
			Exhausted:           true,
		},
	}
	body := errorBodyText(st, 60, 12)
	assert.Contains(t, body, "Requests: 0 used (no limit)")
	assert.Contains(t, body, "Output tokens: 1.0k/1.0k used")
}

func TestBudgetAskActionsAreCopyAndDismissOnly(t *testing.T) {
	var replies []controller.ErrorReply
	var texts []string
	o := errorOverlays(&replies, &texts)
	beginBudgetAsk(o)
	ctx := &components.EventContext{}

	// 'r' is a no-op: there is no Retry action in this action set.
	o.handleErrorKey(ctx, xui.KeyEvent{Press: true, Rune: 'r'})
	require.True(t, o.ErrorActive(), "retry shortcut must not close the budget dialog")
	require.Empty(t, replies)

	// Default selection is index 0 = Copy; Enter resolves with Copy only.
	o.handleErrorKey(ctx, xui.KeyEvent{Press: true, Code: xui.KeyEnter})
	require.Len(t, replies, 1)
	assert.True(t, replies[0].Copy)
	assert.False(t, replies[0].Retry)
	require.Equal(t, "budget exhausted: request limit reached (500/500)", texts[0])
}

func TestBudgetAskNavigationStaysInsideTwoActions(t *testing.T) {
	var replies []controller.ErrorReply
	var texts []string
	o := errorOverlays(&replies, &texts)
	beginBudgetAsk(o)
	ctx := &components.EventContext{}

	// Down from Copy wraps straight back to Copy (2 options, not 3).
	o.handleErrorKey(ctx, xui.KeyEvent{Press: true, Code: xui.KeyDown})
	require.Equal(t, budgetOptDismiss, o.err.selected)
	o.handleErrorKey(ctx, xui.KeyEvent{Press: true, Code: xui.KeyDown})
	require.Equal(t, budgetOptCopy, o.err.selected)

	// Enter on Dismiss resolves without side effects.
	o.handleErrorKey(ctx, xui.KeyEvent{Press: true, Code: xui.KeyDown})
	o.handleErrorKey(ctx, xui.KeyEvent{Press: true, Code: xui.KeyEnter})
	require.Len(t, replies, 1)
	assert.False(t, replies[0].Retry)
	assert.False(t, replies[0].Copy)
}

func TestBudgetAskEscapeAndDismiss(t *testing.T) {
	var replies []controller.ErrorReply
	var texts []string
	o := errorOverlays(&replies, &texts)
	beginBudgetAsk(o)

	o.Apply(controller.OverlayMsg{Kind: controller.OverlayBudgetDismiss})
	require.False(t, o.ErrorActive())
	require.Empty(t, replies, "dismiss kind must not invoke the resolve callback")

	// Escape on a fresh dialog resolves as a plain dismiss.
	beginBudgetAsk(o)
	o.handleErrorKey(&components.EventContext{}, xui.KeyEvent{Press: true, Code: xui.KeyEscape})
	require.Len(t, replies, 1)
	assert.False(t, replies[0].Retry)
	assert.False(t, replies[0].Copy)
}

func TestBudgetAskReplacesOtherModals(t *testing.T) {
	var replies []controller.ErrorReply
	var texts []string
	o := errorOverlays(&replies, &texts)
	beginErrorAsk(o)
	require.NotNil(t, o.err)
	require.False(t, o.err.budgetMode())

	beginBudgetAsk(o)
	require.True(t, o.err.budgetMode(), "budget dialog replaces the ordinary error dialog")
	require.Len(t, replies, 1, "replaced error dialog resolves as dismissed")
}
