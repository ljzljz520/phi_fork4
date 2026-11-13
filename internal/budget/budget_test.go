package budget

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pulseaiclub/phi/internal/llm"
)

func TestReserveRejectsAtomicLimitsWithoutStateChange(t *testing.T) {
	b := New(Limits{MaxRequests: 2, MaxOutputTokens: 10000})

	r1, err := b.Reserve(KindChat, 4000)
	require.NoError(t, err)
	r2, err := b.Reserve(KindChat, 4000)
	require.NoError(t, err)

	_, err = b.Reserve(KindChat, 1)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrExhausted))
	var exhausted *ExhaustedError
	require.ErrorAs(t, err, &exhausted)
	assert.Equal(t, DimensionRequests, exhausted.Dimension)

	// Refusal must not mutate any counter.
	snap := b.Snapshot()
	assert.Equal(t, int64(2), snap.ReservedRequests)
	assert.Equal(t, int64(8000), snap.ReservedOutputTokens)
	assert.Equal(t, int64(0), snap.SettledRequests)

	// Output dimension fails closed even when request headroom exists.
	r1.Release()
	r2.Release()
	_, err = b.Reserve(KindChat, 10001)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrExhausted))
	require.ErrorAs(t, err, &exhausted)
	assert.Equal(t, DimensionOutputTokens, exhausted.Dimension)
	assert.Equal(t, int64(0), b.Snapshot().ReservedRequests, "refused output reservation leaves no hold")
}

func TestConcurrentReserveHonorsCap(t *testing.T) {
	const cap = 20
	b := New(Limits{MaxRequests: cap, MaxOutputTokens: cap * 100})

	var wg sync.WaitGroup
	var ok, refused atomic.Int64
	for range cap * 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := b.Reserve(KindChat, 100)
			if err != nil {
				assert.True(t, errors.Is(err, ErrExhausted))
				refused.Add(1)
				return
			}
			ok.Add(1)
			r.Settle(llm.Usage{CompletionTokens: 100})
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(cap), ok.Load())
	assert.Equal(t, int64(cap*3), refused.Load())
	snap := b.Snapshot()
	assert.Equal(t, int64(cap), snap.SettledRequests)
	assert.Equal(t, int64(0), snap.ReservedRequests)
	assert.Equal(t, int64(cap*100), snap.SettledOutputTokens)
}

func TestSettleReconcilesActualUsage(t *testing.T) {
	b := New(Limits{MaxRequests: 10, MaxOutputTokens: 10000})

	r, err := b.Reserve(KindChat, 8000)
	require.NoError(t, err)
	pre := b.Snapshot()
	assert.Equal(t, int64(8000), pre.ReservedOutputTokens)
	assert.Equal(t, int64(2000), pre.RemainingOutputTokens())

	r.Settle(llm.Usage{CompletionTokens: 500})

	post := b.Snapshot()
	assert.Equal(t, int64(0), post.ReservedRequests)
	assert.Equal(t, int64(0), post.ReservedOutputTokens)
	assert.Equal(t, int64(1), post.SettledRequests)
	assert.Equal(t, int64(500), post.SettledOutputTokens)
	assert.Equal(t, int64(9500), post.RemainingOutputTokens())
	assert.False(t, post.Exhausted)
}

func TestUnknownUsageBranches(t *testing.T) {
	tests := []struct {
		name            string
		producedOutput  bool
		estimate        int
		wantSettledOut  int64
		wantReservedOut int64
	}{
		{name: "partial output charges estimate", producedOutput: true, estimate: 4000, wantSettledOut: 4000},
		{name: "no output releases hold", producedOutput: false, estimate: 4000, wantSettledOut: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := New(Limits{MaxRequests: 10, MaxOutputTokens: 10000})
			r, err := b.Reserve(KindChat, tt.estimate)
			require.NoError(t, err)

			r.SettleUnknown(tt.producedOutput)

			snap := b.Snapshot()
			assert.Equal(t, tt.wantSettledOut, snap.SettledOutputTokens)
			assert.Equal(t, int64(0), snap.ReservedOutputTokens)
			assert.Equal(t, int64(1), snap.SettledRequests, "dispatched requests always count")
		})
	}
}

func TestReleaseBeforeDispatchSettlesNothing(t *testing.T) {
	b := New(Limits{MaxRequests: 10, MaxOutputTokens: 10000})
	r, err := b.Reserve(KindChat, 4000)
	require.NoError(t, err)

	r.Release()

	snap := b.Snapshot()
	assert.Equal(t, int64(0), snap.SettledRequests)
	assert.Equal(t, int64(0), snap.SettledOutputTokens)
	assert.Equal(t, int64(0), snap.ReservedRequests)
	assert.Equal(t, int64(0), snap.ReservedOutputTokens)
}

func TestRetryAttemptsReserveAndSettle(t *testing.T) {
	b := New(Limits{MaxRequests: 3, MaxOutputTokens: 10000})
	r, err := b.Reserve(KindChat, 2000)
	require.NoError(t, err)

	require.NoError(t, r.ReserveAttempt())
	require.NoError(t, r.ReserveAttempt())
	snap := b.Snapshot()
	assert.Equal(t, int64(3), snap.ReservedRequests)

	// A fourth dispatch would cross the cap.
	err = r.ReserveAttempt()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrExhausted))

	r.SettleUnknown(false)
	snap = b.Snapshot()
	assert.Equal(t, int64(3), snap.SettledRequests, "three dispatched attempts settle")
	assert.Equal(t, int64(0), snap.ReservedRequests)
	assert.True(t, snap.Exhausted)
}

func TestSettleIsIdempotent(t *testing.T) {
	b := New(Limits{MaxRequests: 10, MaxOutputTokens: 10000})
	r, err := b.Reserve(KindChat, 4000)
	require.NoError(t, err)

	r.Settle(llm.Usage{CompletionTokens: 100})
	r.Settle(llm.Usage{CompletionTokens: 999})
	r.SettleUnknown(true)
	r.Release()

	snap := b.Snapshot()
	assert.Equal(t, int64(1), snap.SettledRequests)
	assert.Equal(t, int64(100), snap.SettledOutputTokens)
	assert.Equal(t, int64(0), snap.ReservedRequests)
}

func TestZeroLimitsMeanUnlimited(t *testing.T) {
	b := New(Limits{})
	assert.False(t, b.Limits().Enabled())

	for range 100 {
		r, err := b.Reserve(KindChat, 1_000_000)
		require.NoError(t, err)
		r.Settle(llm.Usage{CompletionTokens: 1_000_000})
	}
	snap := b.Snapshot()
	assert.Equal(t, int64(100), snap.SettledRequests)
	assert.Equal(t, int64(-1), snap.RemainingRequests())
	assert.Equal(t, int64(-1), snap.RemainingOutputTokens())
	assert.False(t, snap.Exhausted)
}

func TestNewWithStateRestoresCounters(t *testing.T) {
	b := NewWithState(Limits{MaxRequests: 10, MaxOutputTokens: 10000}, 4, 3200)
	snap := b.Snapshot()
	assert.Equal(t, int64(4), snap.SettledRequests)
	assert.Equal(t, int64(3200), snap.SettledOutputTokens)
	assert.Equal(t, int64(6), snap.RemainingRequests())
	assert.Equal(t, int64(6800), snap.RemainingOutputTokens())
	assert.Equal(t, int64(0), snap.ReservedRequests, "in-flight holds are not restored")
}

func TestOnChangeReceivesSnapshots(t *testing.T) {
	var changes atomic.Int64
	b := New(Limits{MaxRequests: 10, MaxOutputTokens: 10000})
	b.OnChange(func(Snapshot) { changes.Add(1) })

	r, err := b.Reserve(KindChat, 1000)
	require.NoError(t, err)
	r.Settle(llm.Usage{CompletionTokens: 100})
	r2, err := b.Reserve(KindChat, 1000)
	require.NoError(t, err)
	r2.Release()

	assert.Equal(t, int64(4), changes.Load(), "reserve, settle, reserve, release")
}
