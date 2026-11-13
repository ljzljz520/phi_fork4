package agent

import (
	"errors"

	"github.com/pulseaiclub/phi/internal/budget"
	"github.com/pulseaiclub/phi/internal/extension"
	"github.com/pulseaiclub/phi/internal/job"
	"github.com/pulseaiclub/phi/internal/llm"
)

// NewJobManager creates a process-level job manager whose runner drives child Engines.
// modelFn may be nil; then model is used as a fixed snapshot for every role.
// When set, modelFn receives the job role so callers can pick per-role models.
// extensionsFn supplies extensions for child engines (may return nil); prefer a live
// getter so TUI reload updates sub-agents too. budgetFn resolves the parent
// session quota shared by every child; nil leaves sub-agents unguarded.
func NewJobManager(
	root string,
	model llm.ModelConfig,
	modelFn func(role job.Role) llm.ModelConfig,
	extensionsFn func() *extension.Runner,
	budgetFn func() *budget.Budget,
) (*job.Manager, error) {
	if root == "" {
		return nil, errors.New("agent: jobs root is required")
	}
	return job.New(job.Options{
		Root: root,
		Runner: EngineRunner{
			Model:        model,
			ModelFn:      modelFn,
			ExtensionsFn: extensionsFn,
			BudgetFn:     budgetFn,
		},
	})
}
