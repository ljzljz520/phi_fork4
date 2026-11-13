package budget

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// stateVersion is the on-disk schema version of the budget state file.
const stateVersion = 1

// persistedState is the durable record for one main session. Only settled
// counters are written; in-flight reservations die with their process and
// are rebuilt as zero on load.
type persistedState struct {
	Version             int    `json:"version"`
	SessionID           string `json:"session_id"`
	MaxRequests         int    `json:"max_requests,omitempty"`
	MaxOutputTokens     int    `json:"max_output_tokens,omitempty"`
	SettledRequests     int64  `json:"settled_requests"`
	SettledOutputTokens int64  `json:"settled_output_tokens"`
	UpdatedAt           string `json:"updated_at"`
}

// SaveState atomically writes the settled portion of snap to path. Writes go
// through a uniquely named sibling temp file plus rename so a crash cannot
// leave a truncated state file and concurrent settlements cannot truncate
// each other's temp file.
func SaveState(path, sessionID string, snap Snapshot) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create budget dir %q: %w", dir, err)
		}
	}
	state := persistedState{
		Version:             stateVersion,
		SessionID:           sessionID,
		MaxRequests:         snap.Limits.MaxRequests,
		MaxOutputTokens:     snap.Limits.MaxOutputTokens,
		SettledRequests:     snap.SettledRequests,
		SettledOutputTokens: snap.SettledOutputTokens,
		UpdatedAt:           time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal budget state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create budget temp state: %w", err)
	}
	tmpName := tmp.Name()
	// Remove becomes a no-op once the rename commits; before that it cleans
	// up on any error path.
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write budget state %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close budget state %q: %w", tmpName, err)
	}
	// CreateTemp defaults to 0600; the state holds no secrets and 0644 matches
	// the other phi state files under ~/.phi.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod budget state %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("commit budget state %q: %w", path, err)
	}
	return nil
}

// LoadState reads a state file and returns the limits recorded on disk plus
// the settled counters. A missing file is reported with os.IsNotExist so
// callers can treat first use as a fresh budget.
func LoadState(path string) (Limits, int64, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Limits{}, 0, 0, err
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return Limits{}, 0, 0, fmt.Errorf("parse budget state %q: %w", path, err)
	}
	if state.Version != stateVersion {
		return Limits{}, 0, 0, fmt.Errorf("budget state %q has unsupported version %d", path, state.Version)
	}
	if state.SettledRequests < 0 || state.SettledOutputTokens < 0 {
		return Limits{}, 0, 0, fmt.Errorf("budget state %q contains negative counters", path)
	}
	return Limits{
		MaxRequests:     state.MaxRequests,
		MaxOutputTokens: state.MaxOutputTokens,
	}, state.SettledRequests, state.SettledOutputTokens, nil
}

// OpenOrCreate loads a persisted budget for sessionID when a state file
// exists, otherwise starts a fresh one with the supplied limits. On-disk
// limits are ignored in favor of limits: a config change takes effect
// immediately while accumulated usage is preserved. A corrupt file errors
// instead of silently resetting the balance.
func OpenOrCreate(path, sessionID string, limits Limits) (*Budget, error) {
	diskLimits, settledReq, settledOut, err := LoadState(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return New(limits), nil
		}
		return nil, err
	}
	_ = diskLimits
	return NewWithState(limits, settledReq, settledOut), nil
}

// BindPersistence flushes b to path after every settlement. Reservation
// holds and releases do not trigger a write. Persistence failures are
// surfaced through onError instead of being swallowed; a nil onError drops
// them, which callers must choose deliberately.
func BindPersistence(b *Budget, path, sessionID string, onError func(error)) {
	if b == nil {
		return
	}
	flush := func(snap Snapshot) {
		if err := SaveState(path, sessionID, snap); err != nil && onError != nil {
			onError(err)
		}
	}
	b.mu.Lock()
	b.onPersist = flush
	b.mu.Unlock()
}
