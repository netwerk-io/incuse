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
	JobID           int64
	RunnerID        int64
	LaunchedAt      time.Time
	RunnerStartedAt time.Time
	Spec            config.RunnerSpec
	ScaleSetID      int
	Status          instanceStatus
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

// (`get` was retired with the JobAssigned -> tracker rework; lookups
// happen by request id only.)

func (t *instanceTracker) get(runnerName string) (trackedInstance, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	v, ok := t.m[runnerName]
	if !ok {
		return trackedInstance{}, false
	}
	return *v, true
}

func (t *instanceTracker) getByRequest(jobID int64) *trackedInstance {
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

// markStartedByRequest stamps RunnerStartedAt for the instance whose
// job_id matches and returns the matched runner name. Empty string
// means no match.
func (t *instanceTracker) markStartedByRequest(jobID int64, now time.Time) string {
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

func (t *instanceTracker) markLaunched(runnerName string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if v, ok := t.m[runnerName]; ok {
		// Don't overwrite a started timestamp — Mint can race with
		// JobStarted on a fast cloud-init path.
		if v.Status == statusLaunching {
			v.Status = statusRunning
			v.LaunchedAt = now
		}
	}
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
