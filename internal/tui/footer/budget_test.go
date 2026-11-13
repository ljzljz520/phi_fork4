package footer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pulseaiclub/phi/internal/budget"
	"github.com/pulseaiclub/phi/internal/components"
	"github.com/pulseaiclub/phi/internal/session"
)

func TestFormatBudgetLabel(t *testing.T) {
	tests := []struct {
		name string
		snap *budget.Snapshot
		want string
	}{
		{name: "nil hidden", snap: nil, want: ""},
		{name: "uncapped hidden", snap: &budget.Snapshot{}, want: ""},
		{
			name: "requests only",
			snap: &budget.Snapshot{
				Limits:          budget.Limits{MaxRequests: 500},
				SettledRequests: 12,
			},
			want: "budget 12/500 req",
		},
		{
			name: "both dimensions include in-flight holds",
			snap: &budget.Snapshot{
				Limits:               budget.Limits{MaxRequests: 500, MaxOutputTokens: 200000},
				SettledRequests:      10,
				ReservedRequests:     2,
				SettledOutputTokens:  40000,
				ReservedOutputTokens: 5000,
			},
			want: "budget 12/500 req · ↓45k/200k",
		},
		{
			name: "output only skips unlimited request dimension",
			snap: &budget.Snapshot{
				Limits:              budget.Limits{MaxOutputTokens: 1000},
				SettledOutputTokens: 250,
			},
			want: "budget ↓250/1.0k",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatBudgetLabel(tt.snap))
		})
	}
}

func TestBudgetPressureStyle(t *testing.T) {
	th := components.DefaultTheme()

	assert.Equal(t, ChromeLabelStyle(th), budgetPressureStyle(th, nil))

	ambient := &budget.Snapshot{
		Limits:          budget.Limits{MaxRequests: 100},
		SettledRequests: 50,
	}
	assert.Equal(t, ChromeLabelStyle(th), budgetPressureStyle(th, ambient))

	warning := &budget.Snapshot{
		Limits:          budget.Limits{MaxRequests: 100},
		SettledRequests: 90,
	}
	assert.Equal(t, th.Warning, budgetPressureStyle(th, warning))

	exhausted := &budget.Snapshot{
		Limits:           budget.Limits{MaxRequests: 100},
		SettledRequests:  99,
		ReservedRequests: 1,
		Exhausted:        true,
	}
	assert.Equal(t, th.Destructive, budgetPressureStyle(th, exhausted))
}

func TestTokenStatusLabelRendersBudgetAloneAndCombined(t *testing.T) {
	th := components.DefaultTheme()
	snap := &budget.Snapshot{
		Limits:          budget.Limits{MaxRequests: 500, MaxOutputTokens: 200000},
		SettledRequests: 12,
	}

	// No token usage yet: the budget still renders.
	label := tokenStatusLabel(th, session.TokenUsage{}, 0, snap)
	require.True(t, label.Visible())
	require.Empty(t, label.Spans)
	assert.Contains(t, label.Text, "budget 12/500 req")

	// Ambient usage plus ambient quota collapse into the legacy single-text label.
	usage := session.TokenUsage{PromptTokens: 1200, CompletionTokens: 800, TotalTokens: 2000}
	combined := tokenStatusLabel(th, usage, 0, snap)
	require.Empty(t, combined.Spans)
	assert.Contains(t, combined.Text, "↑1.2k")
	assert.Contains(t, combined.Text, "budget 12/500 req")
	assert.Equal(t, ChromeLabelStyle(th), combined.Style)

	// A near-cap quota escalates to its own styled span.
	warningSnap := &budget.Snapshot{Limits: budget.Limits{MaxRequests: 10}, SettledRequests: 9}
	pressured := tokenStatusLabel(th, usage, 0, warningSnap)
	require.NotEmpty(t, pressured.Spans)
	last := pressured.Spans[len(pressured.Spans)-1]
	assert.Contains(t, last.Text, "budget 9/10 req")
	assert.Equal(t, th.Warning, last.Style)

	// Nil budget leaves the pre-budget label untouched.
	plain := tokenStatusLabel(th, usage, 0, nil)
	assert.NotContains(t, plain.Text, "budget")
}
