package orchestrator

import (
	"sync"
	"time"

	"github.com/netwerk-io/incuse/internal/config"
)

// trackedRunner is the orchestrator's per-runner record. Each runner
// is one of three states; the tracker is the source of truth for
// what we have running on Incus.
//
// We follow upstream actions/scaleset's pattern: idle runners exist
// in the pool, GitHub's broker dispatches jobs to them, JobStarted
// transitions idle->busy, JobCompleted transitions busy->reaped.
// Job-to-runner mapping is GitHub's responsibility, not ours; trying
// to bind a JIT mint to a specific JobAssigned races with the broker
// and ends up with stuck attribution (the bug we hit on the rocket
// 10x burst smoke).
type trackedRunner struct {
	Name       string
	RunnerID   int64 // GitHub-side runner registration id, for RemoveRunner cleanup
	LaunchedAt time.Time
	BusyAt     time.Time // zero while idle/launching
	Spec       config.RunnerSpec
	ScaleSetID int
	State      runnerState
	// TerminationPending: HandleJobCompleted (or any other
	// terminate caller) sets this when teardown is requested while
	// CreateInstance is still in flight. The launch goroutine reads
	// it after Launch returns and tears down rather than transitioning
	// to idle.
	TerminationPending bool
}

type runnerState int

const (
	// statusLaunching: IncusClient.Launch in flight; runner has not
	// registered with GitHub yet.
	statusLaunching runnerState = iota
	// statusIdle: launch ok, runner registered, awaiting a job
	// assignment from GitHub's dispatcher.
	statusIdle
	// statusBusy: HandleJobStarted received; runner is executing a
	// job. Reaper switches from registration-timeout to
	// max-job-duration semantics.
	statusBusy
)

// instanceTracker is a small thread-safe registry keyed by runner
// name. The orchestrator has exactly one runner per name (Incus
// enforces uniqueness in a project) so we can use it as the primary
// key throughout.
type instanceTracker struct {
	mu sync.RWMutex
	m  map[string]*trackedRunner
}

func newInstanceTracker() *instanceTracker {
	return &instanceTracker{m: make(map[string]*trackedRunner)}
}

func (t *instanceTracker) add(r *trackedRunner) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m[r.Name] = r
}

func (t *instanceTracker) remove(runnerName string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, runnerName)
}

func (t *instanceTracker) get(runnerName string) (trackedRunner, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	v, ok := t.m[runnerName]
	if !ok {
		return trackedRunner{}, false
	}
	return *v, true
}

// markIdle moves a runner from statusLaunching to statusIdle and
// returns whether termination was requested while the launch was in
// flight. (_, false) means the entry was removed before the launch
// completed.
func (t *instanceTracker) markIdle(runnerName string, now time.Time) (terminationRequested bool, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, exists := t.m[runnerName]
	if !exists {
		return false, false
	}
	if v.State == statusLaunching {
		v.State = statusIdle
		v.LaunchedAt = now
	}
	return v.TerminationPending, true
}

// markBusy stamps BusyAt and transitions the runner to statusBusy.
// Returns the previous state and whether the entry existed; callers
// can log a warning on mismatched events.
func (t *instanceTracker) markBusy(runnerName string, now time.Time) (previousState runnerState, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, exists := t.m[runnerName]
	if !exists {
		return 0, false
	}
	prev := v.State
	v.BusyAt = now
	v.State = statusBusy
	return prev, true
}

// markForTermination flips the TerminationPending bit on a tracked
// runner and returns its current state. (_, false) means the entry
// was already gone — caller should no-op.
func (t *instanceTracker) markForTermination(runnerName string) (runnerState, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, ok := t.m[runnerName]
	if !ok {
		return 0, false
	}
	v.TerminationPending = true
	return v.State, true
}

// terminationPending is a cheap pre-check the launch goroutine uses
// before issuing IncusClient.Launch.
func (t *instanceTracker) terminationPending(runnerName string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	v, ok := t.m[runnerName]
	return ok && v.TerminationPending
}

// snapshot returns a stable copy of the tracker contents. Used by
// the reaper so the sweep doesn't hold the lock across slow Incus
// calls.
func (t *instanceTracker) snapshot() []trackedRunner {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]trackedRunner, 0, len(t.m))
	for _, v := range t.m {
		out = append(out, *v)
	}
	return out
}

func (t *instanceTracker) names() map[string]struct{} {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]struct{}, len(t.m))
	for k := range t.m {
		out[k] = struct{}{}
	}
	return out
}

func (t *instanceTracker) size() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.m)
}
