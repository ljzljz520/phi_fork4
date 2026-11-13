// Package budget enforces an optional shared quota over one main session's
// model work: the main agent, concurrent sub-agent jobs, compaction calls and
// HTTP retries all draw from the same Budget. Quota is atomically reserved
// before any request is dispatched and settled against the reported usage
// afterwards. A nil *Budget disables enforcement with zero overhead.
package budget

import (
	"errors"
	"fmt"
	"sync"

	"github.com/pulseaiclub/phi/internal/llm"
)

// DefaultOutputEstimate is the per-request output hold when the live model
// config does not declare MaxTokens. It is a conservative stand-in, not a
// price estimate: settlements always reconcile against reported usage.
const DefaultOutputEstimate = 4096

// RequestKind tags why a reservation was taken; accounting does not differ
// per kind, but snapshots and traces can tell chat turns from compaction.
type RequestKind string

const (
	KindChat    RequestKind = "chat"
	KindCompact RequestKind = "compact"
)

// Dimension identifies which limit rejected a reservation.
type Dimension string

const (
	DimensionRequests     Dimension = "requests"
	DimensionOutputTokens Dimension = "output_tokens"
)

// Limits are the optional per-session caps. A zero value means that dimension
// is unlimited, so the zero Limits disables the budget entirely.
type Limits struct {
	MaxRequests     int `json:"max_requests,omitempty"`
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
}

// Enabled reports whether at least one dimension is capped.
func (l Limits) Enabled() bool {
	return l.MaxRequests > 0 || l.MaxOutputTokens > 0
}

// Snapshot is an immutable point-in-time view shared with UI layers, so the
// display can never observe a half-updated counter.
type Snapshot struct {
	Limits               Limits `json:"limits"`
	SettledRequests      int64  `json:"settled_requests"`
	SettledOutputTokens  int64  `json:"settled_output_tokens"`
	ReservedRequests     int64  `json:"reserved_requests"`
	ReservedOutputTokens int64  `json:"reserved_output_tokens"`
	Exhausted            bool   `json:"exhausted"`
}

// RemainingRequests returns the request headroom including in-flight holds,
// or -1 when the dimension is unlimited.
func (s Snapshot) RemainingRequests() int64 {
	if s.Limits.MaxRequests <= 0 {
		return -1
	}
	return int64(s.Limits.MaxRequests) - s.SettledRequests - s.ReservedRequests
}

// RemainingOutputTokens returns the output headroom including in-flight holds,
// or -1 when the dimension is unlimited.
func (s Snapshot) RemainingOutputTokens() int64 {
	if s.Limits.MaxOutputTokens <= 0 {
		return -1
	}
	return int64(s.Limits.MaxOutputTokens) - s.SettledOutputTokens - s.ReservedOutputTokens
}

// ErrExhausted is the sentinel callers match with errors.Is. The concrete
// ExhaustedError carries the offending dimension and the quota snapshot.
var ErrExhausted = errors.New("budget exhausted")

// ExhaustedError means a reservation was refused before any network call.
type ExhaustedError struct {
	Dimension Dimension
	Snapshot  Snapshot
}

func (e *ExhaustedError) Error() string {
	switch e.Dimension {
	case DimensionRequests:
		return fmt.Sprintf(
			"budget exhausted: request limit reached (%d/%d)",
			e.Snapshot.SettledRequests+e.Snapshot.ReservedRequests,
			e.Snapshot.Limits.MaxRequests,
		)
	case DimensionOutputTokens:
		return fmt.Sprintf(
			"budget exhausted: output token limit reached (%d/%d reserved)",
			e.Snapshot.SettledOutputTokens+e.Snapshot.ReservedOutputTokens,
			e.Snapshot.Limits.MaxOutputTokens,
		)
	default:
		return "budget exhausted"
	}
}

// Unwrap makes errors.Is(err, ErrExhausted) work without exposing the
// concrete type to callers.
func (e *ExhaustedError) Unwrap() error { return ErrExhausted }

// Budget is the shared, concurrency-safe quota ledger for one main session.
type Budget struct {
	mu sync.Mutex

	limits Limits

	settledRequests  int64
	settledOutput    int64
	reservedRequests int64
	reservedOutput   int64

	// onChange, when set, is invoked after every mutation with a fresh
	// snapshot. Callbacks run outside the lock and must not re-enter Budget.
	onChange func(Snapshot)
	// onPersist, when set, is invoked only after a settlement, so durable
	// state never tracks volatile in-flight holds. Runs outside the lock.
	onPersist func(Snapshot)
}

// New creates a budget with zero consumption.
func New(limits Limits) *Budget {
	return &Budget{limits: limits}
}

// NewWithState recreates a budget from persisted settled counters.
// Outstanding reservations are intentionally not persisted: in-flight
// requests die with the process that owned them.
func NewWithState(limits Limits, settledRequests, settledOutputTokens int64) *Budget {
	return &Budget{
		limits:          limits,
		settledRequests: settledRequests,
		settledOutput:   settledOutputTokens,
	}
}

// OnChange registers a snapshot listener. Replaces any previous listener;
// nil removes it. The listener fires outside the budget lock.
func (b *Budget) OnChange(fn func(Snapshot)) {
	b.mu.Lock()
	b.onChange = fn
	b.mu.Unlock()
}

// Limits returns the configured caps.
func (b *Budget) Limits() Limits {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limits
}

// Snapshot returns a consistent copy of the current state.
func (b *Budget) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.snapshotLocked()
}

func (b *Budget) snapshotLocked() Snapshot {
	s := Snapshot{
		Limits:               b.limits,
		SettledRequests:      b.settledRequests,
		SettledOutputTokens:  b.settledOutput,
		ReservedRequests:     b.reservedRequests,
		ReservedOutputTokens: b.reservedOutput,
	}
	s.Exhausted = b.exhaustedLocked(s)
	return s
}

func (b *Budget) exhaustedLocked(s Snapshot) bool {
	if b.limits.MaxRequests > 0 && s.SettledRequests+s.ReservedRequests >= int64(b.limits.MaxRequests) {
		return true
	}
	if b.limits.MaxOutputTokens > 0 && s.SettledOutputTokens+s.ReservedOutputTokens >= int64(b.limits.MaxOutputTokens) {
		return true
	}
	return false
}

func (b *Budget) notify(snap Snapshot) {
	// Read the callback under the same lock that guards OnChange registration
	// so a concurrent register cannot race the field read.
	b.mu.Lock()
	fn := b.onChange
	b.mu.Unlock()
	if fn != nil {
		fn(snap)
	}
}

// Reserve atomically holds one request slot and outputEstimate output tokens
// before the caller dispatches anything. On refusal no state changes and the
// returned error unwraps to ErrExhausted.
func (b *Budget) Reserve(kind RequestKind, outputEstimate int) (*Reservation, error) {
	if outputEstimate < 0 {
		outputEstimate = 0
	}

	b.mu.Lock()
	if b.limits.MaxRequests > 0 && b.settledRequests+b.reservedRequests+1 > int64(b.limits.MaxRequests) {
		snap := b.snapshotLocked()
		b.mu.Unlock()
		return nil, &ExhaustedError{Dimension: DimensionRequests, Snapshot: snap}
	}
	if b.limits.MaxOutputTokens > 0 &&
		b.settledOutput+b.reservedOutput+int64(outputEstimate) > int64(b.limits.MaxOutputTokens) {
		snap := b.snapshotLocked()
		b.mu.Unlock()
		return nil, &ExhaustedError{Dimension: DimensionOutputTokens, Snapshot: snap}
	}

	b.reservedRequests++
	b.reservedOutput += int64(outputEstimate)
	r := &Reservation{
		budget:         b,
		kind:           kind,
		outputEstimate: int64(outputEstimate),
		attempts:       1,
	}
	snap := b.snapshotLocked()
	b.mu.Unlock()

	b.notify(snap)
	return r, nil
}

// Reservation is one held quota slot. Exactly one of Settle, SettleUnknown or
// Release may take effect; repeated calls are no-ops.
type Reservation struct {
	budget         *Budget
	kind           RequestKind
	outputEstimate int64
	attempts       int64
	done           bool
}

// ReserveAttempt holds one additional request slot for an actual HTTP retry
// attempt. It is invoked by the util retry hook before the retry is
// dispatched, so no network call can bypass the request cap.
func (r *Reservation) ReserveAttempt() error {
	b := r.budget
	b.mu.Lock()
	if r.done {
		b.mu.Unlock()
		return nil
	}
	if b.limits.MaxRequests > 0 &&
		b.settledRequests+b.reservedRequests+1 > int64(b.limits.MaxRequests) {
		snap := b.snapshotLocked()
		b.mu.Unlock()
		return &ExhaustedError{Dimension: DimensionRequests, Snapshot: snap}
	}
	r.attempts++
	b.reservedRequests++
	snap := b.snapshotLocked()
	b.mu.Unlock()

	b.notify(snap)
	return nil
}

// Settle closes the reservation against provider-reported usage.
func (r *Reservation) Settle(usage llm.Usage) {
	r.close(int64(usage.CompletionTokens))
}

// SettleUnknown closes a reservation whose provider gave no usage. When the
// call produced observable output (partial stream then cancel/error, or a
// compact summary without usage), the full estimate is charged fail-closed;
// failures before any output release the output hold. Request attempts are
// always counted once a request was dispatched.
func (r *Reservation) SettleUnknown(producedOutput bool) {
	charged := int64(0)
	if producedOutput {
		charged = r.outputEstimate
	}
	r.close(charged)
}

// Release gives the whole hold back without settling anything. It is only for
// reservations whose request never reached the wire (build failure,
// pre-cancelled context).
func (r *Reservation) Release() {
	b := r.budget
	b.mu.Lock()
	if r.done {
		b.mu.Unlock()
		return
	}
	r.done = true
	b.reservedRequests -= r.attempts
	b.reservedOutput -= r.outputEstimate
	snap := b.snapshotLocked()
	b.mu.Unlock()

	b.notify(snap)
}

// close moves the hold into settled counters. chargedOutput may be zero.
func (r *Reservation) close(chargedOutput int64) {
	if chargedOutput < 0 {
		chargedOutput = 0
	}
	b := r.budget
	b.mu.Lock()
	if r.done {
		b.mu.Unlock()
		return
	}
	r.done = true
	b.reservedRequests -= r.attempts
	b.reservedOutput -= r.outputEstimate
	b.settledRequests += r.attempts
	b.settledOutput += chargedOutput
	snap := b.snapshotLocked()
	persist := b.onPersist
	b.mu.Unlock()

	b.notify(snap)
	if persist != nil {
		persist(snap)
	}
}

// Kind reports why the reservation was taken.
func (r *Reservation) Kind() RequestKind { return r.kind }
