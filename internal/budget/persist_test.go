package budget

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pulseaiclub/phi/internal/llm"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "sess.budget.json")
	b := New(Limits{MaxRequests: 10, MaxOutputTokens: 10000})

	r, err := b.Reserve(KindChat, 8000)
	require.NoError(t, err)
	r.Settle(llm.Usage{CompletionTokens: 500})
	snap := b.Snapshot()

	require.NoError(t, SaveState(path, "sess", snap))

	loaded, req, out, err := LoadState(path)
	require.NoError(t, err)
	assert.Equal(t, Limits{MaxRequests: 10, MaxOutputTokens: 10000}, loaded)
	assert.Equal(t, int64(1), req)
	assert.Equal(t, int64(500), out)

	restored := NewWithState(loaded, req, out)
	restoredSnap := restored.Snapshot()
	assert.Equal(t, int64(0), restoredSnap.ReservedRequests, "in-flight holds are not restored")
	assert.Equal(t, int64(1), restoredSnap.SettledRequests)
	assert.Equal(t, int64(9500), restoredSnap.RemainingOutputTokens())
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.budget.json")

	require.NoError(t, SaveState(path, "sess", New(Limits{MaxRequests: 1}).Snapshot()))

	_, err := os.Stat(filepath.Join(dir, "sess.budget.json.tmp"))
	assert.True(t, os.IsNotExist(err), "temp file must be renamed away")
}

func TestSaveStateConcurrentWritersDoNotCollide(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bucket", "sess.budget.json")

	const writers = 32
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			snap := Snapshot{
				Limits:          Limits{MaxRequests: 1000, MaxOutputTokens: 1_000_000},
				SettledRequests: int64(n + 1),
			}
			errs <- SaveState(path, "sess", snap)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	// The committed file must always be a complete, parseable snapshot.
	_, settled, _, err := LoadState(path)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, settled, int64(1))
	assert.LessOrEqual(t, settled, int64(writers))

	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	for _, entry := range entries {
		assert.NotContains(t, entry.Name(), ".tmp", "no temp file may survive the rename")
	}
}

func TestLoadCorruptFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sess.budget.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o644))

	_, _, _, err := LoadState(path)
	require.Error(t, err)

	_, err = OpenOrCreate(path, "sess", Limits{MaxRequests: 1})
	require.Error(t, err, "corrupt state must not reset to a zero budget")
}

func TestLoadMissingFileCreatesFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.budget.json")
	b, err := OpenOrCreate(path, "sess", Limits{MaxRequests: 5})
	require.NoError(t, err)
	assert.Equal(t, int64(0), b.Snapshot().SettledRequests)
}

func TestOpenOrCreateKeepsCountersUnderNewLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sess.budget.json")
	b := New(Limits{MaxRequests: 10, MaxOutputTokens: 10000})
	r, err := b.Reserve(KindCompact, 1000)
	require.NoError(t, err)
	r.Settle(llm.Usage{CompletionTokens: 1000})
	require.NoError(t, SaveState(path, "sess", b.Snapshot()))

	// Config raised the caps; settled usage must survive.
	reopened, err := OpenOrCreate(path, "sess", Limits{MaxRequests: 20, MaxOutputTokens: 20000})
	require.NoError(t, err)
	snap := reopened.Snapshot()
	assert.Equal(t, 20, snap.Limits.MaxRequests)
	assert.Equal(t, 20000, snap.Limits.MaxOutputTokens)
	assert.Equal(t, int64(1), snap.SettledRequests)
	assert.Equal(t, int64(1000), snap.SettledOutputTokens)
}

func TestBindPersistenceWritesOnlyOnSettle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sess.budget.json")
	b := New(Limits{MaxRequests: 10, MaxOutputTokens: 10000})
	BindPersistence(b, path, "sess", func(err error) { require.NoError(t, err) })

	r, err := b.Reserve(KindChat, 1000)
	require.NoError(t, err)
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "reservations must not be persisted")

	r.Settle(llm.Usage{CompletionTokens: 250})
	_, req, out, err := LoadState(path)
	require.NoError(t, err)
	assert.Equal(t, int64(1), req)
	assert.Equal(t, int64(250), out)

	r2, err := b.Reserve(KindChat, 1000)
	require.NoError(t, err)
	r2.Release()
	_, req, _, err = LoadState(path)
	require.NoError(t, err)
	assert.Equal(t, int64(1), req, "release changes no settled counters")
}

func TestBindPersistenceSurfacesErrors(t *testing.T) {
	// A path whose parent is a regular file cannot be created.
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o644))
	path := filepath.Join(blocker, "sess.budget.json")

	var got error
	b := New(Limits{MaxRequests: 10})
	BindPersistence(b, path, "sess", func(err error) { got = err })

	r, err := b.Reserve(KindChat, 0)
	require.NoError(t, err)
	r.Settle(llm.Usage{})
	require.Error(t, got)
}
