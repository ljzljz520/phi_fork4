package footer

import (
	"fmt"
	"strings"

	"github.com/pulseaiclub/xui"

	"github.com/pulseaiclub/phi/internal/budget"
	"github.com/pulseaiclub/phi/internal/components"
	"github.com/pulseaiclub/phi/internal/components/layout"
	"github.com/pulseaiclub/phi/internal/session"
)

// budgetWarnRatio is the used/limit share (including in-flight holds) at
// which the quota label escalates from ambient to warning color.
const budgetWarnRatio = 0.9

// contextFillLevel ranks context-window pressure for the fill label.
type contextFillLevel int

const (
	contextFillOK contextFillLevel = iota
	contextFillRecommend
	contextFillWarning
	contextFillDanger
)

func contextFillRatio(used, window int) float64 {
	if window <= 0 || used <= 0 {
		return 0
	}
	return float64(used) / float64(window)
}

func contextFillLevelFor(ratio float64, window int) contextFillLevel {
	// Tiers scaled by window size: very large windows tolerate higher fills.
	var recommend, warning, danger float64
	switch {
	case window >= 900000:
		recommend, warning, danger = 0.2, 0.8, 0.9
	case window >= 400000:
		recommend, warning, danger = 0.7, 0.8, 0.9
	default:
		recommend, warning, danger = 0.8, 0.9, 0.95
	}
	switch {
	case ratio >= danger:
		return contextFillDanger
	case ratio >= warning:
		return contextFillWarning
	case ratio >= recommend:
		return contextFillRecommend
	default:
		return contextFillOK
	}
}

// formatContextLabel builds a "context: 4% of 128k" fill label (empty when unknown).
func formatContextLabel(usage session.TokenUsage, window int) string {
	if window <= 0 {
		return ""
	}
	used := usage.ContextTokens()
	if used <= 0 {
		return ""
	}
	pct := min(max(int(contextFillRatio(used, window)*100), 0), 100)
	if window >= 1000 {
		return fmt.Sprintf("%d%%/%s", pct, components.FormatTokens(window))
	}
	return fmt.Sprintf("%d%%", pct)
}

// formatUsageStats builds "↑1.2k ↓800 C900 W200 Σ3.0k" (empty when unknown).
// The buckets are disjoint copies of the provider's: ↑ is the input that missed
// the cache, C and W are the cache traffic, Σ their sum (or the reported total).
func formatUsageStats(usage session.TokenUsage) string {
	if !usage.Reported() {
		return ""
	}
	parts := make([]string, 0, 5)
	if usage.PromptTokens > 0 {
		parts = append(parts, "↑"+components.FormatTokens(usage.PromptTokens))
	}
	if usage.CompletionTokens > 0 {
		parts = append(parts, "↓"+components.FormatTokens(usage.CompletionTokens))
	}
	if usage.CachedTokens > 0 {
		parts = append(parts, "C"+components.FormatTokens(usage.CachedTokens))
	}
	if usage.CacheWriteTokens > 0 {
		parts = append(parts, "W"+components.FormatTokens(usage.CacheWriteTokens))
	}
	total := usage.ContextTokens()
	if total > 0 {
		parts = append(parts, "Σ"+components.FormatTokens(total))
	}
	if len(parts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(parts[0])
	for i := 1; i < len(parts); i++ {
		b.WriteString(" " + parts[i])
	}
	return b.String()
}

// PathLabelStyle styles ambient composer-border embeds (path, idle tokens, activity).
// Alias of ChromeLabelStyle — kept for call-site clarity at the cwd label.
func PathLabelStyle(th components.Theme) xui.Style {
	return ChromeLabelStyle(th)
}

// ChromeLabelStyle is the single ambient voice for chat-input border chrome.
// Model Identity stays the frame's only accent; Warning/Destructive escalate
// only under context pressure.
func ChromeLabelStyle(th components.Theme) xui.Style {
	// Muted without Dim so labels stay readable on dark borders.
	st := th.Muted
	st.Dim = false
	return st
}

// contextPressureStyle colors only the context-fill fragment under pressure.
// At rest it matches ChromeLabelStyle so usage stats never shout ToolName cyan.
func contextPressureStyle(th components.Theme, usage session.TokenUsage, window int) xui.Style {
	used := usage.ContextTokens()
	ratio := contextFillRatio(used, window)
	switch contextFillLevelFor(ratio, window) {
	case contextFillDanger:
		return th.Destructive
	case contextFillWarning:
		return th.Warning
	default:
		return ChromeLabelStyle(th)
	}
}

// formatBudgetLabel builds the quota fragment, e.g.
// "budget 12/500 req · ↓45.0k/200k". Unlimited dimensions are omitted, so a
// requests-only budget renders "budget 12/500 req"; a nil snapshot renders
// nothing and keeps the footer unchanged for unconfigured sessions.
func formatBudgetLabel(snap *budget.Snapshot) string {
	if snap == nil {
		return ""
	}
	var parts []string
	if snap.Limits.MaxRequests > 0 {
		used := snap.SettledRequests + snap.ReservedRequests
		parts = append(parts, fmt.Sprintf("%d/%d req", used, snap.Limits.MaxRequests))
	}
	if snap.Limits.MaxOutputTokens > 0 {
		used := snap.SettledOutputTokens + snap.ReservedOutputTokens
		parts = append(parts, fmt.Sprintf(
			"↓%s/%s",
			components.FormatTokens(int(used)),
			components.FormatTokens(snap.Limits.MaxOutputTokens),
		))
	}
	if len(parts) == 0 {
		return ""
	}
	return "budget " + strings.Join(parts, " · ")
}

// budgetPressureStyle colors the quota fragment: destructive once refused,
// warning at >=90% of either cap, ambient otherwise.
func budgetPressureStyle(th components.Theme, snap *budget.Snapshot) xui.Style {
	if snap == nil {
		return ChromeLabelStyle(th)
	}
	if snap.Exhausted {
		return th.Destructive
	}
	maxRatio := 0.0
	if snap.Limits.MaxRequests > 0 {
		maxRatio = max(
			maxRatio,
			float64(snap.SettledRequests+snap.ReservedRequests)/float64(snap.Limits.MaxRequests),
		)
	}
	if snap.Limits.MaxOutputTokens > 0 {
		maxRatio = max(
			maxRatio,
			float64(snap.SettledOutputTokens+snap.ReservedOutputTokens)/float64(snap.Limits.MaxOutputTokens),
		)
	}
	if maxRatio >= budgetWarnRatio {
		return th.Warning
	}
	return ChromeLabelStyle(th)
}

// tokenStatusLabel builds the idle bottom-left label: usage always ambient,
// context % escalates alone when the window is under pressure, and the quota
// fragment escalates independently.
func tokenStatusLabel(
	th components.Theme,
	usage session.TokenUsage,
	window int,
	budgetSnap *budget.Snapshot,
) layout.BorderLabel {
	stats := formatUsageStats(usage)
	ctx := formatContextLabel(usage, window)
	quota := formatBudgetLabel(budgetSnap)
	if stats == "" && ctx == "" && quota == "" {
		return layout.BorderLabel{}
	}

	var spans []layout.BorderSpan
	add := func(text string, style xui.Style) {
		if text == "" {
			return
		}
		if len(spans) > 0 {
			text = " " + text
		}
		spans = append(spans, layout.BorderSpan{Text: text, Style: style})
	}

	chrome := ChromeLabelStyle(th)
	add(stats, chrome)
	if ctx != "" {
		add(ctx, contextPressureStyle(th, usage, window))
	}
	add(quota, budgetPressureStyle(th, budgetSnap))

	if len(spans) == 1 {
		return layout.BorderLabel{Text: spans[0].Text, Style: spans[0].Style}
	}
	// Ambient-only content keeps the legacy single-text label; spans exist
	// only when at least one fragment escalates to its own color.
	allAmbient := true
	for _, span := range spans {
		if span.Style != chrome {
			allAmbient = false
			break
		}
	}
	if allAmbient {
		parts := make([]string, len(spans))
		for i, span := range spans {
			parts[i] = span.Text
		}
		return layout.BorderLabel{Text: strings.Join(parts, ""), Style: chrome}
	}
	return layout.BorderLabel{Spans: spans}
}

// joinBorderParts concatenates non-empty label fragments with a single space.
func joinBorderParts(parts ...string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += " "
		}
		out += p
	}
	return out
}
