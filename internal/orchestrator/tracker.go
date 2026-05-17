package orchestrator

import (
	"sync"
	"time"

	"github.com/netwerk-io/incuse/internal/config"
)

// trackedInstance is the orchestrator's per-runner record. Lives in
// memory only; the reaper's drift sweep is the recovery mechanism for
// orchestrator restarts.
type trackedInstance struct {
	RunnerName      string
	JobID           string // upstream JobMessageBase.JobID — stable, populated, primary key
	RunnerRequestID int64  // upstream JobMessageBase.RunnerRequestID — empirically zero for some broker messages, kept for diagnostics
	WorkflowRunID   int64  // upstream JobMessageBase.WorkflowRunID — for cross-referencing with gh CLI
	RunnerID        int64  // GitHub-side runner registration id, for RemoveRunner cleanup
	LaunchedAt      time.Time
	RunnerStartedAt time.Time
	Spec            config.RunnerSpec
	ScaleSetID      int
	Status          instanceStatus
	// TerminationPending is set by HandleJobCompleted (or any other
	// caller of terminateInstance) when teardown is requested while
	// the launch goroutine is still running CreateInstance. Incus
	// rejects delete-during-create, so the launch goroutine reads
	// this after Launch returns and tears the instance down itself.
	TerminationPending bool
}

type instanceStatus int

const (
	// statusLaunching means we have minted a JIT and asked Incus to
	// create the instance, but the create+start operation has not
	// returned yet.
	statusLaunching instanceStatus = iota
	// statusRunning means the Incus launch operation completed
	// successfully. The runner may or may not have registered yet.
	statusRunning
	// statusStarted means the runner picked up its assigned job. The
	// reaper switches from registration-timeout to max-job-duration
	// once we hit this state.
	statusStarted
)

// instanceTracker is a small thread-safe registry keyed by runner
// name. The orchestrator has exactly one instance per name (Incus
// enforces uniqueness in a project) so we can use it as the primary
// key throughout.
type instanceTracker struct {
	mu sync.RWMutex
	m  map[string]*trackedInstance
}

func newInstanceTracker() *instanceTracker {
	return &instanceTracker{m: make(map[string]*trackedInstance)}
}

func (t *instanceTracker) add(inst *trackedInstance) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m[inst.RunnerName] = inst
}

func (t *instanceTracker) remove(runnerName string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, runnerName)
}

func (t *instanceTracker) get(runnerName string) (trackedInstance, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	v, ok := t.m[runnerName]
	if !ok {
		return trackedInstance{}, false
	}
	return *v, true
}

// getByJobID looks up a tracked instance by upstream JobID. JobID is
// a non-empty string for every JobAssigned / JobStarted / JobCompleted
// message GitHub sends; RunnerRequestID by contrast is empirically
// zero for some broker messages, which is why we don't use it as a
// match key.
func (t *instanceTracker) getByJobID(jobID string) *trackedInstance {
	if jobID == "" {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, v := range t.m {
		if v.JobID == jobID {
			out := *v
			return &out
		}
	}
	return nil
}

// markStartedByJobID stamps RunnerStartedAt for the instance whose
// upstream JobID matches and returns the matched runner name. Empty
// string means no match.
func (t *instanceTracker) markStartedByJobID(jobID string, now time.Time) string {
	if jobID == "" {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, v := range t.m {
		if v.JobID == jobID {
			v.RunnerStartedAt = now
			v.Status = statusStarted
			return v.RunnerName
		}
	}
	return ""
}

// markForTermination flips the TerminationPending bit on a tracked
// instance and returns its current status. A return of (_, false)
// means the entry was already gone — caller should no-op.
func (t *instanceTracker) markForTermination(runnerName string) (instanceStatus, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, ok := t.m[runnerName]
	if !ok {
		return 0, false
	}
	v.TerminationPending = true
	return v.Status, true
}

// terminationPending is a cheap pre-check the launch goroutine uses
// before issuing IncusClient.Launch — if a JobCompleted has already
// arrived, we'd rather not pay for the create at all.
func (t *instanceTracker) terminationPending(runnerName string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	v, ok := t.m[runnerName]
	return ok && v.TerminationPending
}

// markLaunched stamps the create-completed timestamp and returns
// whether termination was requested while the launch was in flight.
// On true, the caller should tear down the just-created instance.
// The (_, false) return only happens if the entry was removed (e.g.
// by a previous failure path) before the launch finished.
func (t *instanceTracker) markLaunched(runnerName string, now time.Time) (terminationRequested bool, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, exists := t.m[runnerName]
	if !exists {
		return false, false
	}
	if v.Status == statusLaunching {
		v.Status = statusRunning
		v.LaunchedAt = now
	}
	return v.TerminationPending, true
}

// snapshot returns a stable copy of the tracker contents. Used by the
// reaper so the sweep doesn't hold the lock across slow Incus calls.
func (t *instanceTracker) snapshot() []trackedInstance {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]trackedInstance, 0, len(t.m))
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
