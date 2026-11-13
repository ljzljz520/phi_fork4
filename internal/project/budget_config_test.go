package project

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pulseaiclub/phi/internal/budget"
)

const budgetConfigFixture = `models:
  - name: model-a
    api_key: key-a
budget:
  max_requests: 500
  max_output_tokens: 200000
`

func TestParseBudgetBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(budgetConfigFixture), 0o600))

	cfg, err := parseConfigFile(path)
	require.NoError(t, err)
	assert.Equal(t, BudgetConfig{MaxRequests: 500, MaxOutputTokens: 200000}, cfg.Budget)
	assert.True(t, cfg.Budget.Enabled())
	assert.Equal(t, budget.Limits{MaxRequests: 500, MaxOutputTokens: 200000}, cfg.Budget.Limits())
}

func TestMissingBudgetBlockMeansUnlimited(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("models:\n  - name: m\n    api_key: k\n"), 0o600))

	cfg, err := parseConfigFile(path)
	require.NoError(t, err)
	assert.False(t, cfg.Budget.Enabled())
	assert.Equal(t, budget.Limits{}, cfg.Budget.Limits())
}

func TestExplicitZeroStaysUnlimited(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`models:
  - name: m
    api_key: k
budget:
  max_requests: 0
  max_output_tokens: 0
`), 0o600))

	cfg, err := parseConfigFile(path)
	require.NoError(t, err)
	assert.False(t, cfg.Budget.Enabled())
}

func TestNegativeBudgetRejected(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		err    string
	}{
		{
			name: "negative requests",
			config: `models:
  - name: m
    api_key: k
budget:
  max_requests: -1
`,
			err: "max_requests",
		},
		{
			name: "negative output tokens",
			config: `models:
  - name: m
    api_key: k
budget:
  max_output_tokens: -10
`,
			err: "max_output_tokens",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tc.config), 0o600))
			_, err := parseConfigFile(path)
			require.ErrorContains(t, err, tc.err)
		})
	}
}

func TestBudgetConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(budgetConfigFixture), 0o600))

	doc, err := ReadConfigDoc(path)
	require.NoError(t, err)
	require.NotNil(t, doc.Budget)
	require.NotNil(t, doc.Budget.MaxRequests)
	assert.Equal(t, 500, *doc.Budget.MaxRequests)
	require.NoError(t, doc.Validate())

	saved := filepath.Join(t.TempDir(), "config-saved.yaml")
	require.NoError(t, doc.Save(saved))

	reloaded, err := ReadConfigDoc(saved)
	require.NoError(t, err)
	require.NotNil(t, reloaded.Budget)
	require.NotNil(t, reloaded.Budget.MaxRequests)
	assert.Equal(t, 500, *reloaded.Budget.MaxRequests)
	require.NotNil(t, reloaded.Budget.MaxOutputTokens)
	assert.Equal(t, 200000, *reloaded.Budget.MaxOutputTokens)
}

func TestAbsentBudgetNotWrittenOnSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("models:\n  - name: m\n    api_key: k\n"), 0o600))

	doc, err := ReadConfigDoc(path)
	require.NoError(t, err)
	require.Nil(t, doc.Budget)

	saved := filepath.Join(t.TempDir(), "out.yaml")
	require.NoError(t, doc.Save(saved))
	data, err := os.ReadFile(saved)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "budget")
}

func TestBudgetLimitsApplyOverrides(t *testing.T) {
	base := BudgetConfig{MaxRequests: 500, MaxOutputTokens: 200000}

	// Negative override values mean "not provided on the CLI".
	assert.Equal(t,
		budget.Limits{MaxRequests: 500, MaxOutputTokens: 200000},
		base.Limits(BudgetConfig{MaxRequests: -1, MaxOutputTokens: -1}),
	)
	// Explicit CLI values replace the file settings per dimension.
	assert.Equal(t,
		budget.Limits{MaxRequests: 10, MaxOutputTokens: 200000},
		base.Limits(BudgetConfig{MaxRequests: 10, MaxOutputTokens: -1}),
	)
	assert.Equal(t,
		budget.Limits{MaxRequests: 10, MaxOutputTokens: 5000},
		base.Limits(BudgetConfig{MaxRequests: 10, MaxOutputTokens: 5000}),
	)
}
