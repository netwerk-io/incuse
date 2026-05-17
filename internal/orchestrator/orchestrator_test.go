package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ssapi "github.com/actions/scaleset"

	"github.com/netwerk-io/incuse/internal/config"
	"github.com/netwerk-io/incuse/internal/incus"
	"github.com/netwerk-io/incuse/internal/runner"
)

// fakeIncus records every Launch/Stop/Delete/List call.
type fakeIncus struct {
	mu            sync.Mutex
	launches      []incus.LaunchRequest
	stops         []string
	deletes       []string
	listed        int
	remote        []incus.Instance
	launchErr     error
	stopErr       error
	deleteErr     error
	listErr       error
	launchGate    chan struct{} // optional: blocks Launch until closed
	launchEntered chan struct{} // optional: closed by Launch before it blocks
}

func (f *fakeIncus) Launch(_ context.Context, req incus.LaunchRequest) (*incus.Instance, error) {
	if f.launchEntered != nil {
		select {
		case <-f.launchEntered:
		default:
			close(f.launchEntered)
		}
	}
	if f.launchGate != nil {
		<-f.launchGate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launches = append(f.launches, req)
	if f.launchErr != nil {
		return nil, f.launchErr
	}
	return &incus.Instance{Name: req.Name, Status: "Running"}, nil
}

func (f *fakeIncus) Stop(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, name)
	return f.stopErr
}

func (f *fakeIncus) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, name)
	return f.deleteErr
}

func (f *fakeIncus) List(_ context.Context, _ string) ([]incus.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed++
	return append([]incus.Instance(nil), f.remote...), f.listErr
}

func (f *fakeIncus) snapshot() (launches []incus.LaunchRequest, stops, deletes []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]incus.LaunchRequest(nil), f.launches...),
		append([]string(nil), f.stops...),
		append([]string(nil), f.deletes...)
}

// fakeScaleSet stands in for *scaleset.ScaleSet.
type fakeScaleSet struct {
	mu          sync.Mutex
	jitCalls    int
	removeCalls []int64
	jitErr      error
	scaleSetID  int
	spec        config.ScaleSetConfig
	maxRunners  int
}

func (f *fakeScaleSet) GenerateJITConfig(_ context.Context, runnerName, _ string) ([]byte, *ssapi.RunnerReference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jitCalls++
	if f.jitErr != nil {
		return nil, nil, f.jitErr
	}
	return []byte("jit-" + runnerName), &ssapi.RunnerReference{ID: 7777, Name: runnerName}, nil
}

func (f *fakeScaleSet) RemoveRunner(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls = append(f.removeCalls, id)
	return nil
}

func (f *fakeScaleSet) SetMaxRunners(c int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.maxRunners = c
}

func (f *fakeScaleSet) ScaleSetID() int             { return f.scaleSetID }
func (f *fakeScaleSet) Spec() config.ScaleSetConfig { return f.spec }

// fakeResolver is a constant ReleaseResolver for tests.
type fakeResolver struct {
	rel runner.Release
	err error
}

func (f *fakeResolver) Resolve(_ context.Context) (runner.Release, error) {
	return f.rel, f.err
}

// fakeMetrics records every Metrics* call so tests can assert.
type fakeMetrics struct {
	mu              sync.Mutex
	runnerSpawned   int
	launchOK        int
	launchFail      int
	launchDurations []float64
	runnerLifetimes []float64
	reapReasons     map[string]int
	trackedSetTo    []int
}

func newFakeMetrics() *fakeMetrics { return &fakeMetrics{reapReasons: map[string]int{}} }

func (f *fakeMetrics) RunnerSpawned() { f.mu.Lock(); defer f.mu.Unlock(); f.runnerSpawned++ }
func (f *fakeMetrics) LaunchOK()      { f.mu.Lock(); defer f.mu.Unlock(); f.launchOK++ }
func (f *fakeMetrics) LaunchFail()    { f.mu.Lock(); defer f.mu.Unlock(); f.launchFail++ }
func (f *fakeMetrics) LaunchDuration(s float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launchDurations = append(f.launchDurations, s)
}
func (f *fakeMetrics) RunnerLifetime(s float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runnerLifetimes = append(f.runnerLifetimes, s)
}
func (f *fakeMetrics) Reap(r string) { f.mu.Lock(); defer f.mu.Unlock(); f.reapReasons[r]++ }
func (f *fakeMetrics) SetTrackedInstances(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trackedSetTo = append(f.trackedSetTo, n)
}

func (f *fakeMetrics) snapshot() (spawned, ok, fail int, reasons map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rr := make(map[string]int, len(f.reapReasons))
	for k, v := range f.reapReasons {
		rr[k] = v
	}
	return f.runnerSpawned, f.launchOK, f.launchFail, rr
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fixedClock returns a Now func clamped to a starting timestamp + an
// offset the test can advance.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fixedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestOrchestrator(t *testing.T, mutate func(*Config)) (*Orchestrator, *fakeIncus, *fakeScaleSet, *fixedClock) {
	t.Helper()
	clk := &fixedClock{t: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)}
	fi := &fakeIncus{}
	fs := &fakeScaleSet{
		scaleSetID: 42,
		spec: config.ScaleSetConfig{
			Name:        "incuse-test",
			RunnerGroup: "Default",
			MaxRunners:  10,
			BaseLabels:  []string{"incuse-test"},
		},
	}
	resolver := &fakeResolver{rel: runner.Release{Version: "2.328.0", DownloadURL: "https://example/x64.tgz"}}

	var nameSeq atomic.Int64
	cfg := Config{
		IncusClient:     fi,
		ScaleSet:        fs,
		ReleaseResolver: resolver,
		IncusCfg: config.IncusConfig{
			Project:        "incuse",
			DefaultProfile: "incuse-runner",
		},
		RunnerCfg: config.RunnerConfig{
			ImageServer:         "https://images.linuxcontainers.org",
			ImageProtocol:       "simplestreams",
			ImageAlias:          "ubuntu/24.04/cloud",
			WorkFolder:          "_work",
			VCPUTiers:           []int{1, 2, 4},
			MemoryPerVCPUMiB:    4096,
			RootDiskGiB:         40,
			RegistrationTimeout: 10 * time.Minute,
			MaxJobDuration:      6 * time.Hour,
		},
		HostArch:     "amd64",
		Logger:       discardLogger(),
		ReapInterval: time.Hour,
		Now:          clk.Now,
		NameSuffix: func() string {
			n := nameSeq.Add(1)
			// produce names: aaaa, bbbb, cccc, ...
			ch := byte('a' + (n-1)%26)
			return string([]byte{ch, ch, ch, ch})
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return o, fi, fs, clk
}

// waitForLaunches blocks until fi.launches reaches the expected count.
func waitForLaunches(t *testing.T, fi *fakeIncus, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fi.mu.Lock()
		got := len(fi.launches)
		fi.mu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("waitForLaunches: timed out, want %d", want)
}

// waitForTrackerSize blocks until tracker.size() reaches want, or fails.
func waitForTrackerSize(t *testing.T, o *Orchestrator, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if o.tracker.size() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("waitForTrackerSize: want %d, got %d", want, o.tracker.size())
}

// ------------------ tests ------------------

func TestNew_Validation(t *testing.T) {
	validBase := func() Config {
		return Config{
			IncusClient:     &fakeIncus{},
			ScaleSet:        &fakeScaleSet{spec: config.ScaleSetConfig{MaxRunners: 1}},
			ReleaseResolver: &fakeResolver{},
			IncusCfg:        config.IncusConfig{Project: "p", DefaultProfile: "pr"},
			RunnerCfg: config.RunnerConfig{
				RegistrationTimeout: time.Minute,
				MaxJobDuration:      time.Hour,
				VCPUTiers:           []int{1},
				MemoryPerVCPUMiB:    1024,
				RootDiskGiB:         10,
			},
			Logger: discardLogger(),
		}
	}

	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"missing incus client", func(c *Config) { c.IncusClient = nil }, "incus client"},
		{"missing scaleset", func(c *Config) { c.ScaleSet = nil }, "scale set"},
		{"missing resolver", func(c *Config) { c.ReleaseResolver = nil }, "release resolver"},
		{"missing logger", func(c *Config) { c.Logger = nil }, "logger"},
		{"missing project", func(c *Config) { c.IncusCfg.Project = "" }, "project"},
		{"missing profile", func(c *Config) { c.IncusCfg.DefaultProfile = "" }, "default_profile"},
		{"missing registration timeout", func(c *Config) { c.RunnerCfg.RegistrationTimeout = 0 }, "registration_timeout"},
		{"missing max job duration", func(c *Config) { c.RunnerCfg.MaxJobDuration = 0 }, "max_job_duration"},
		{"missing vcpu tiers", func(c *Config) { c.RunnerCfg.VCPUTiers = nil }, "vcpu_tiers"},
		{"max runners zero", func(c *Config) { c.ScaleSet = &fakeScaleSet{spec: config.ScaleSetConfig{MaxRunners: 0}} }, "max_runners"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validBase()
			tc.mutate(&cfg)
			_, err := New(cfg)
			if err == nil || !contains(err.Error(), tc.wantErr) {
				t.Errorf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestHandleDesiredRunnerCount_SpawnsAtSmallestTier(t *testing.T) {
	o, fi, fs, _ := newTestOrchestrator(t, nil)
	got, err := o.HandleDesiredRunnerCount(t.Context(), 3)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount: %v", err)
	}
	if got != 3 {
		t.Errorf("returned target: want 3, got %d", got)
	}
	waitForLaunches(t, fi, 3)
	if fs.jitCalls != 3 {
		t.Errorf("jit mints: want 3, got %d", fs.jitCalls)
	}
	launches, _, _ := fi.snapshot()
	for _, req := range launches {
		if got := req.Config["limits.cpu"]; got != "1" {
			t.Errorf("limits.cpu: want 1 (smallest tier), got %q", got)
		}
		if got := req.Config[metaManaged]; got != "true" {
			t.Errorf("%s: want true, got %q", metaManaged, got)
		}
		if got := req.Config[metaRunnerName]; got == "" {
			t.Errorf("%s: missing", metaRunnerName)
		}
	}
}

func TestHandleDesiredRunnerCount_CapsAtMaxRunners(t *testing.T) {
	o, fi, _, _ := newTestOrchestrator(t, func(c *Config) {
		c.ScaleSet = &fakeScaleSet{spec: config.ScaleSetConfig{Name: "incuse-test", MaxRunners: 2}}
	})
	got, err := o.HandleDesiredRunnerCount(t.Context(), 5)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount: %v", err)
	}
	if got != 2 {
		t.Errorf("target: want 2 (capped), got %d", got)
	}
	waitForLaunches(t, fi, 2)
}

func TestHandleDesiredRunnerCount_NoOpAtTarget(t *testing.T) {
	o, fi, _, _ := newTestOrchestrator(t, nil)
	if _, err := o.HandleDesiredRunnerCount(t.Context(), 2); err != nil {
		t.Fatalf("first call: %v", err)
	}
	waitForLaunches(t, fi, 2)
	if _, err := o.HandleDesiredRunnerCount(t.Context(), 2); err != nil {
		t.Fatalf("second call: %v", err)
	}
	// Second call must not have spawned additional runners.
	time.Sleep(20 * time.Millisecond)
	launches, _, _ := fi.snapshot()
	if len(launches) != 2 {
		t.Errorf("launches: want 2 (no extra spawn at target), got %d", len(launches))
	}
}

func TestHandleDesiredRunnerCount_NegativeIsZero(t *testing.T) {
	o, _, _, _ := newTestOrchestrator(t, nil)
	got, _ := o.HandleDesiredRunnerCount(t.Context(), -3)
	if got != 0 {
		t.Errorf("want 0, got %d", got)
	}
}

func TestHandleJobStarted_MarksRunnerBusy(t *testing.T) {
	o, fi, _, _ := newTestOrchestrator(t, nil)
	if _, err := o.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatalf("desired count: %v", err)
	}
	waitForLaunches(t, fi, 1)
	waitForTrackerSize(t, o, 1)

	runnerName := "incuse-test-aaaa"
	if err := o.HandleJobStarted(t.Context(), &ssapi.JobStarted{RunnerName: runnerName, JobMessageBase: ssapi.JobMessageBase{JobID: "j-1"}}); err != nil {
		t.Fatalf("HandleJobStarted: %v", err)
	}
	r, ok := o.tracker.get(runnerName)
	if !ok {
		t.Fatal("runner missing from tracker")
	}
	if r.State != statusBusy {
		t.Errorf("state: want busy, got %v", r.State)
	}
	if r.BusyAt.IsZero() {
		t.Error("BusyAt not stamped")
	}
}

func TestHandleJobStarted_UnknownRunnerLogsAndReturns(t *testing.T) {
	o, _, _, _ := newTestOrchestrator(t, nil)
	if err := o.HandleJobStarted(t.Context(), &ssapi.JobStarted{RunnerName: "does-not-exist"}); err != nil {
		t.Fatalf("HandleJobStarted: %v", err)
	}
}

func TestHandleJobCompleted_StopsAndDeletes(t *testing.T) {
	o, fi, _, _ := newTestOrchestrator(t, nil)
	if _, err := o.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatalf("desired count: %v", err)
	}
	waitForLaunches(t, fi, 1)
	waitForTrackerSize(t, o, 1)

	runnerName := "incuse-test-aaaa"
	_ = o.HandleJobStarted(t.Context(), &ssapi.JobStarted{RunnerName: runnerName})
	if err := o.HandleJobCompleted(t.Context(), &ssapi.JobCompleted{RunnerName: runnerName, Result: "succeeded"}); err != nil {
		t.Fatalf("HandleJobCompleted: %v", err)
	}
	_, stops, deletes := fi.snapshot()
	if len(stops) != 1 || stops[0] != runnerName {
		t.Errorf("stops: want [%s], got %v", runnerName, stops)
	}
	if len(deletes) != 1 || deletes[0] != runnerName {
		t.Errorf("deletes: want [%s], got %v", runnerName, deletes)
	}
	if o.tracker.size() != 0 {
		t.Errorf("tracker size: want 0, got %d", o.tracker.size())
	}
}

func TestHandleJobCompleted_UnknownRunnerIsNoop(t *testing.T) {
	o, fi, _, _ := newTestOrchestrator(t, nil)
	if err := o.HandleJobCompleted(t.Context(), &ssapi.JobCompleted{RunnerName: "ghost"}); err != nil {
		t.Fatalf("HandleJobCompleted: %v", err)
	}
	_, stops, deletes := fi.snapshot()
	if len(stops) != 0 || len(deletes) != 0 {
		t.Errorf("expected no incus calls; stops=%v deletes=%v", stops, deletes)
	}
}

func TestHandleJobCompleted_NilEventIsNoop(t *testing.T) {
	o, _, _, _ := newTestOrchestrator(t, nil)
	if err := o.HandleJobCompleted(t.Context(), nil); err != nil {
		t.Fatalf("nil: %v", err)
	}
}

func TestSpawnIdleRunner_LaunchFailureRemovesRunnerFromGitHub(t *testing.T) {
	o, fi, fs, _ := newTestOrchestrator(t, nil)
	fi.launchErr = errors.New("boom")
	if _, err := o.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatalf("desired count: %v", err)
	}
	waitForLaunches(t, fi, 1)

	// Wait a bit for the failure-cleanup goroutine to run.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		fs.mu.Lock()
		if len(fs.removeCalls) == 1 {
			fs.mu.Unlock()
			break
		}
		fs.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.removeCalls) != 1 || fs.removeCalls[0] != 7777 {
		t.Errorf("RemoveRunner: want [7777], got %v", fs.removeCalls)
	}
	if o.tracker.size() != 0 {
		t.Errorf("tracker size: want 0 after launch failure, got %d", o.tracker.size())
	}
}

func TestReap_RegistrationTimeoutForIdleRunnerNeverTakingJob(t *testing.T) {
	o, fi, _, clk := newTestOrchestrator(t, func(c *Config) {
		c.RunnerCfg.RegistrationTimeout = time.Minute
	})
	if _, err := o.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatalf("desired count: %v", err)
	}
	waitForLaunches(t, fi, 1)
	waitForTrackerSize(t, o, 1)

	clk.advance(2 * time.Minute)
	o.reapOnce(t.Context())

	if o.tracker.size() != 0 {
		t.Errorf("tracker size: want 0 after registration timeout, got %d", o.tracker.size())
	}
	_, stops, deletes := fi.snapshot()
	if len(stops) != 1 || len(deletes) != 1 {
		t.Errorf("expected one stop+delete, got stops=%v deletes=%v", stops, deletes)
	}
}

func TestReap_DoesNotReapIdleRunnerBeforeTimeout(t *testing.T) {
	o, fi, _, clk := newTestOrchestrator(t, func(c *Config) {
		c.RunnerCfg.RegistrationTimeout = time.Hour
	})
	if _, err := o.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatalf("desired count: %v", err)
	}
	waitForLaunches(t, fi, 1)
	waitForTrackerSize(t, o, 1)

	clk.advance(time.Minute) // far less than RegistrationTimeout=1h
	o.reapOnce(t.Context())

	if o.tracker.size() != 1 {
		t.Errorf("tracker should still have entry; got size=%d", o.tracker.size())
	}
}

func TestReap_MaxJobDurationForBusyRunner(t *testing.T) {
	o, fi, _, clk := newTestOrchestrator(t, func(c *Config) {
		c.RunnerCfg.MaxJobDuration = time.Hour
	})
	if _, err := o.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatalf("desired count: %v", err)
	}
	waitForLaunches(t, fi, 1)
	waitForTrackerSize(t, o, 1)
	_ = o.HandleJobStarted(t.Context(), &ssapi.JobStarted{RunnerName: "incuse-test-aaaa"})

	clk.advance(2 * time.Hour)
	o.reapOnce(t.Context())

	if o.tracker.size() != 0 {
		t.Errorf("tracker should be empty after max_job_duration reap; got size=%d", o.tracker.size())
	}
}

func TestReap_DriftSweepDeletesOrphanManagedInstances(t *testing.T) {
	o, fi, _, _ := newTestOrchestrator(t, nil)
	fi.remote = []incus.Instance{
		{Name: "orphan-managed", Status: "Running", Config: map[string]string{metaManaged: "true"}},
		{Name: "not-ours", Status: "Running", Config: map[string]string{}},
	}
	o.reapOnce(t.Context())

	_, stops, deletes := fi.snapshot()
	if len(stops) != 1 || stops[0] != "orphan-managed" {
		t.Errorf("orphan stops: want [orphan-managed], got %v", stops)
	}
	if len(deletes) != 1 || deletes[0] != "orphan-managed" {
		t.Errorf("orphan deletes: want [orphan-managed], got %v", deletes)
	}
}

func TestReap_DriftSweepIgnoresInstancesInTracker(t *testing.T) {
	o, fi, _, _ := newTestOrchestrator(t, nil)
	if _, err := o.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatalf("desired count: %v", err)
	}
	waitForLaunches(t, fi, 1)
	waitForTrackerSize(t, o, 1)

	fi.mu.Lock()
	fi.remote = []incus.Instance{
		{Name: "incuse-test-aaaa", Status: "Running", Config: map[string]string{metaManaged: "true"}},
	}
	fi.mu.Unlock()
	// Don't touch reapOnce stop/delete count via the registration
	// timer; advance clock minimally.
	o.driftSweep(t.Context())
	_, stops, deletes := fi.snapshot()
	if len(stops) != 0 || len(deletes) != 0 {
		t.Errorf("drift sweep should ignore tracked runners; stops=%v deletes=%v", stops, deletes)
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	o, _, _, _ := newTestOrchestrator(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("want context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

func TestMetrics_HappyPath(t *testing.T) {
	fm := newFakeMetrics()
	o, fi, _, _ := newTestOrchestrator(t, func(c *Config) { c.Metrics = fm })

	if _, err := o.HandleDesiredRunnerCount(t.Context(), 2); err != nil {
		t.Fatalf("desired count: %v", err)
	}
	waitForLaunches(t, fi, 2)
	waitForTrackerSize(t, o, 2)
	_ = o.HandleJobStarted(t.Context(), &ssapi.JobStarted{RunnerName: "incuse-test-aaaa"})
	_ = o.HandleJobCompleted(t.Context(), &ssapi.JobCompleted{RunnerName: "incuse-test-aaaa", Result: "succeeded"})

	spawned, ok, fail, reasons := fm.snapshot()
	if spawned != 2 {
		t.Errorf("RunnerSpawned: want 2, got %d", spawned)
	}
	if ok != 2 || fail != 0 {
		t.Errorf("launches: want ok=2 fail=0, got ok=%d fail=%d", ok, fail)
	}
	if reasons["job_completed"] != 1 {
		t.Errorf("reap reasons: want job_completed=1, got %v", reasons)
	}

	fm.mu.Lock()
	defer fm.mu.Unlock()
	if len(fm.runnerLifetimes) != 1 {
		t.Errorf("RunnerLifetime: want 1 sample, got %d", len(fm.runnerLifetimes))
	}
}

func TestMetrics_LaunchFailure(t *testing.T) {
	fm := newFakeMetrics()
	o, fi, _, _ := newTestOrchestrator(t, func(c *Config) { c.Metrics = fm })
	fi.launchErr = errors.New("boom")
	if _, err := o.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatalf("desired count: %v", err)
	}
	waitForLaunches(t, fi, 1)

	// Give failure-cleanup goroutine time to set fail counter.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, ok, fail, _ := fm.snapshot()
		if ok+fail >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	_, ok, fail, _ := fm.snapshot()
	if ok != 0 || fail != 1 {
		t.Errorf("launches: want ok=0 fail=1, got ok=%d fail=%d", ok, fail)
	}
}

func TestMetrics_ReapReasons(t *testing.T) {
	fm := newFakeMetrics()
	o, fi, _, clk := newTestOrchestrator(t, func(c *Config) {
		c.Metrics = fm
		c.RunnerCfg.RegistrationTimeout = time.Minute
	})
	if _, err := o.HandleDesiredRunnerCount(t.Context(), 1); err != nil {
		t.Fatalf("desired count: %v", err)
	}
	waitForLaunches(t, fi, 1)
	waitForTrackerSize(t, o, 1)
	clk.advance(2 * time.Minute)
	o.reapOnce(t.Context())
	_, _, _, reasons := fm.snapshot()
	if reasons["registration_timeout"] != 1 {
		t.Errorf("reap reasons: want registration_timeout=1, got %v", reasons)
	}
}

// Race: JobCompleted arrives while IncusClient.Launch is still in
// flight. Must NOT call Stop/Delete (Incus refuses delete-during-
// create). Should set TerminationPending and let the launch
// goroutine handle teardown after Launch returns.
func TestRace_JobCompletedDuringLaunchDefersTeardown(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	o, fi, _, _ := newTestOrchestrator(t, nil)
	fi.launchGate = gate
	fi.launchEntered = entered
	ctx := t.Context()

	if _, err := o.HandleDesiredRunnerCount(ctx, 1); err != nil {
		t.Fatalf("desired count: %v", err)
	}

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Launch never entered")
	}

	if err := o.HandleJobCompleted(ctx, &ssapi.JobCompleted{RunnerName: "incuse-test-aaaa"}); err != nil {
		t.Fatalf("HandleJobCompleted: %v", err)
	}
	_, stops, deletes := fi.snapshot()
	if len(stops) != 0 || len(deletes) != 0 {
		t.Fatalf("Stop/Delete fired during launch; stops=%v deletes=%v", stops, deletes)
	}

	close(gate)
	for i := 0; i < 200; i++ {
		_, stops, deletes = fi.snapshot()
		if len(stops) == 1 && len(deletes) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(stops) != 1 || len(deletes) != 1 {
		t.Fatalf("expected one stop+delete after gate; stops=%v deletes=%v", stops, deletes)
	}
	if o.tracker.size() != 0 {
		t.Errorf("tracker should be empty after teardown; got %d", o.tracker.size())
	}
}

func contains(s, sub string) bool {
	return s != "" && sub != "" && (s == sub || (len(s) >= len(sub) && (indexOf(s, sub) >= 0)))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
