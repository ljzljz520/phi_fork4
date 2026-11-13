package overlays

import (
	"fmt"

	"github.com/pulseaiclub/phi/internal/budget"
	"github.com/pulseaiclub/phi/internal/components"
	"github.com/pulseaiclub/phi/internal/tui/controller"
)

// budgetUsageLines renders both quota dimensions from the refusal snapshot.
// In-flight reservations count as used because they hold headroom against
// the same cap; unlimited dimensions say so instead of printing a zero cap.
func budgetUsageLines(snap budget.Snapshot) []string {
	usedRequests := snap.SettledRequests + snap.ReservedRequests
	usedOutput := snap.SettledOutputTokens + snap.ReservedOutputTokens

	requests := fmt.Sprintf("Requests: %d", usedRequests)
	if snap.Limits.MaxRequests > 0 {
		requests += fmt.Sprintf("/%d used", snap.Limits.MaxRequests)
	} else {
		requests += " used (no limit)"
	}

	output := fmt.Sprintf("Output tokens: %s", components.FormatTokens(int(usedOutput)))
	if snap.Limits.MaxOutputTokens > 0 {
		output += fmt.Sprintf("/%s used", components.FormatTokens(snap.Limits.MaxOutputTokens))
	} else {
		output += " used (no limit)"
	}
	return []string{requests, output}
}

// beginBudgetAsk opens the quota-refusal dialog. It shares the error dialog's
// state machine (focus, draw, resolve callback) but renders a distinct title,
// dimension usage and a Copy/Dismiss-only action set.
func (o *Overlays) beginBudgetAsk(msg controller.OverlayMsg) {
	if o.err != nil {
		o.resolveError(controller.ErrorReply{})
	}
	if o.perm != nil {
		o.resolvePermission(controller.AskReply{})
	}
	if o.confirm != nil {
		o.resolveExtConfirm(controller.ExtConfirmReply{})
	}
	if o.composer != nil {
		o.composer.HideCompleters()
		o.composer.HidePalette()
	}
	o.err = &errorAskState{message: msg.ErrorText, budget: msg.BudgetSnapshot}
	o.activity.Apply(controller.ActivityIdle)
	if o.focusEditor != nil {
		o.focusEditor()
	}
}
