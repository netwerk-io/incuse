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

// fakeIncus records every Launch/Stop/Delete/List call. Test asserts
// against the recorded sequence.
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
	launchEntered chan struct{} // optional: closed by Launch before it blocks on gate
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

func (f *fakeScaleSet) ScaleSetID() int { return f.scaleSetID }

func (f *fakeScaleSet) Spec() config.ScaleSetConfig { return f.spec }

// fakeResolver is a constant ReleaseResolver for tests.
type fakeResolver struct {
	rel runner.Release
	err error
}

func (f *fakeResolver) Resolve(_ context.Context) (runner.Release, error) {
	return f.rel, f.err
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

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

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
			MaxRunners:  2,
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

// waitForLaunches blocks until fi.launches reaches the expected count
// or the timeout fires. The launch goroutine runs asynchronously, so
// every assertion that depends on Launch having completed needs this
// helper.
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

func TestNew_Validation(t *testing.T) {
	base := func() Config {
		return Config{
			IncusClient:     &fakeIncus{},
			ScaleSet:        &fakeScaleSet{spec: config.ScaleSetConfig{MaxRunners: 1}},
			ReleaseResolver: &fakeResolver{},
			IncusCfg:        config.IncusConfig{Project: "p", DefaultProfile: "pr"},
			RunnerCfg:       config.RunnerConfig{RegistrationTimeout: time.Minute, MaxJobDuration: time.Hour},
			Logger:          discardLogger(),
		}
	}
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"missing incus", func(c *Config) { c.IncusClient = nil }, "incus client"},
		{"missing scaleset", func(c *Config) { c.ScaleSet = nil }, "scale set"},
		{"missing resolver", func(c *Config) { c.ReleaseResolver = nil }, "resolver"},
		{"missing logger", func(c *Config) { c.Logger = nil }, "logger"},
		{"missing project", func(c *Config) { c.IncusCfg.Project = "" }, "project"},
		{"missing profile", func(c *Config) { c.IncusCfg.DefaultProfile = "" }, "profile"},
		{"missing reg timeout", func(c *Config) { c.RunnerCfg.RegistrationTimeout = 0 }, "registration_timeout"},
		{"missing max job", func(c *Config) { c.RunnerCfg.MaxJobDuration = 0 }, "max_job_duration"},
		{"max runners zero", func(c *Config) { c.ScaleSet = &fakeScaleSet{spec: config.ScaleSetConfig{MaxRunners: 0}} }, "max_runners"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(&c)
			_, err := New(c)
			if err == nil {
				t.Fatalf("want error containing %q", tc.want)
			}
		})
	}
}

func TestMint_LaunchesVMWithExpectedShape(t *testing.T) {
	o, fi, fs, _ := newTestOrchestrator(t, nil)

	if err := o.Mint(t.Context(), &ssapi.JobAssigned{
		JobMessageBase: ssapi.JobMessageBase{
			JobID: "j-1234", RunnerRequestID: 1234,
			RequestLabels: []string{"incuse-test", "vcpu=2"},
		},
	}); err != nil {
		t.Fatalf("mint: %v", err)
	}

	waitForLaunches(t, fi, 1)
	launches, _, _ := fi.snapshot()
	req := launches[0]

	if req.Type != incus.InstanceTypeVM {
		t.Errorf("type: want VM, got %q", req.Type)
	}
	if req.Name != "incuse-test-aaaa" {
		t.Errorf("name: want incuse-test-aaaa, got %q", req.Name)
	}
	if req.Image.Alias != "ubuntu/24.04/cloud" {
		t.Errorf("image alias: %q", req.Image.Alias)
	}
	if got := req.Config["limits.cpu"]; got != "2" {
		t.Errorf("limits.cpu: want 2, got %q", got)
	}
	if got := req.Config["limits.memory"]; got != "8192MiB" {
		t.Errorf("limits.memory: want 8192MiB, got %q", got)
	}
	if got := req.Config[metaManaged]; got != "true" {
		t.Errorf("%s: want true, got %q", metaManaged, got)
	}
	if got := req.Config[metaJobID]; got != "j-1234" {
		t.Errorf("%s: want j-1234, got %q", metaJobID, got)
	}
	if got := req.Config[metaRunnerRequestID]; got != "1234" {
		t.Errorf("%s: want 1234, got %q", metaRunnerRequestID, got)
	}
	if got := req.Config[metaScaleSetID]; got != "42" {
		t.Errorf("%s: want 42, got %q", metaScaleSetID, got)
	}
	if got := req.Config[metaRunnerName]; got != "incuse-test-aaaa" {
		t.Errorf("%s: want incuse-test-aaaa, got %q", metaRunnerName, got)
	}
	if got := req.Config["cloud-init.user-data"]; got == "" {
		t.Error("cloud-init.user-data: missing")
	}
	if !req.Ephemeral {
		t.Error("ephemeral: want true")
	}
	if got := req.Devices["root"]["size"]; got != "40GiB" {
		t.Errorf("root size: want 40GiB, got %q", got)
	}
	if fs.jitCalls != 1 {
		t.Errorf("jit calls: want 1, got %d", fs.jitCalls)
	}
}

func TestMint_LaunchFailureRemovesRunnerFromGitHub(t *testing.T) {
	o, fi, fs, _ := newTestOrchestrator(t, nil)
	fi.launchErr = errors.New("incus exploded")

	if err := o.Mint(t.Context(), &ssapi.JobAssigned{
		JobMessageBase: ssapi.JobMessageBase{
			JobID: "j-1", RunnerRequestID: 1,
			RequestLabels: []string{"incuse-test"},
		},
	}); err != nil {
		t.Fatalf("mint must not return error to listener: %v", err)
	}
	waitForLaunches(t, fi, 1)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fs.mu.Lock()
		n := len(fs.removeCalls)
		fs.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if len(fs.removeCalls) != 1 || fs.removeCalls[0] != 7777 {
		t.Errorf("RemoveRunner: want [7777], got %v", fs.removeCalls)
	}
	if o.tracker.size() != 0 {
		t.Errorf("tracker size: want 0 after failed launch, got %d", o.tracker.size())
	}
}

func TestMint_NilEventIsNoop(t *testing.T) {
	o, fi, _, _ := newTestOrchestrator(t, nil)
	if err := o.Mint(t.Context(), nil); err != nil {
		t.Fatalf("mint nil: %v", err)
	}
	if got, _, _ := fi.snapshot(); len(got) != 0 {
		t.Errorf("nil event must not launch; got %d", len(got))
	}
}

func TestMint_BadLabelLogsAndContinues(t *testing.T) {
	o, fi, _, _ := newTestOrchestrator(t, nil)

	if err := o.Mint(t.Context(), &ssapi.JobAssigned{
		JobMessageBase: ssapi.JobMessageBase{
			JobID: "j-1", RunnerRequestID: 1,
			RequestLabels: []string{"vcpu=1", "vcpu=2"}, // conflict
		},
	}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if got, _, _ := fi.snapshot(); len(got) != 0 {
		t.Errorf("bad spec must not launch; got %d", len(got))
	}
}

func TestHandleJobStarted_StampsRunnerStartedAt(t *testing.T) {
	o, _, _, clk := newTestOrchestrator(t, func(c *Config) {
		c.IncusClient.(*fakeIncus).launchGate = make(chan struct{})
	})
	gate := o.cfg.IncusClient.(*fakeIncus).launchGate

	if err := o.Mint(t.Context(), &ssapi.JobAssigned{
		JobMessageBase: ssapi.JobMessageBase{JobID: "j-99", RunnerRequestID: 99, RequestLabels: []string{"incuse-test"}},
	}); err != nil {
		t.Fatalf("mint: %v", err)
	}

	clk.advance(5 * time.Second)
	if err := o.HandleJobStarted(t.Context(), &ssapi.JobStarted{
		JobMessageBase: ssapi.JobMessageBase{JobID: "j-99", RunnerRequestID: 99},
	}); err != nil {
		t.Fatalf("handle started: %v", err)
	}
	close(gate)
	waitForLaunches(t, o.cfg.IncusClient.(*fakeIncus), 1)

	got := o.tracker.getByJobID("j-99")
	if got == nil {
		t.Fatal("tracked instance disappeared")
	}
	if got.RunnerStartedAt.IsZero() {
		t.Error("RunnerStartedAt must be stamped")
	}
	if got.Status != statusStarted {
		t.Errorf("status: want started, got %d", got.Status)
	}
}

func TestHandleJobCompleted_StopsAndDeletes(t *testing.T) {
	o, fi, _, _ := newTestOrchestrator(t, nil)
	if err := o.Mint(t.Context(), &ssapi.JobAssigned{
		JobMessageBase: ssapi.JobMessageBase{JobID: "j-11", RunnerRequestID: 11, RequestLabels: []string{"incuse-test"}},
	}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	waitForLaunches(t, fi, 1)

	if err := o.HandleJobCompleted(t.Context(), &ssapi.JobCompleted{
		JobMessageBase: ssapi.JobMessageBase{JobID: "j-11", RunnerRequestID: 11},
	}); err != nil {
		t.Fatalf("handle completed: %v", err)
	}

	_, stops, deletes := fi.snapshot()
	if len(stops) != 1 || stops[0] != "incuse-test-aaaa" {
		t.Errorf("stops: want [incuse-test-aaaa], got %v", stops)
	}
	if len(deletes) != 1 || deletes[0] != "incuse-test-aaaa" {
		t.Errorf("deletes: want [incuse-test-aaaa], got %v", deletes)
	}
	if o.tracker.size() != 0 {
		t.Errorf("tracker size: want 0 after job complete, got %d", o.tracker.size())
	}
}

func TestHandleJobCompleted_UnknownRequestIsNoop(t *testing.T) {
	o, fi, _, _ := newTestOrchestrator(t, nil)
	if err := o.HandleJobCompleted(t.Context(), &ssapi.JobCompleted{
		JobMessageBase: ssapi.JobMessageBase{JobID: "j-999", RunnerRequestID: 999},
	}); err != nil {
		t.Fatalf("handle completed: %v", err)
	}
	if _, stops, deletes := fi.snapshot(); len(stops) != 0 || len(deletes) != 0 {
		t.Errorf("unknown request must not call stop/delete; stops=%v deletes=%v", stops, deletes)
	}
}

func TestHandleDesiredRunnerCount_CapsAtMaxRunners(t *testing.T) {
	o, _, _, _ := newTestOrchestrator(t, nil)
	got, err := o.HandleDesiredRunnerCount(t.Context(), 100)
	if err != nil {
		t.Fatalf("handle desired: %v", err)
	}
	if got != 2 {
		t.Errorf("capacity: want 2 (max_runners), got %d", got)
	}
	got, _ = o.HandleDesiredRunnerCount(t.Context(), 1)
	if got != 1 {
		t.Errorf("capacity: want 1 (request below max), got %d", got)
	}
	got, _ = o.HandleDesiredRunnerCount(t.Context(), -5)
	if got != 0 {
		t.Errorf("capacity: want 0 (clamp negative), got %d", got)
	}
}

func TestReap_RegistrationTimeout(t *testing.T) {
	o, fi, fs, clk := newTestOrchestrator(t, nil)

	if err := o.Mint(t.Context(), &ssapi.JobAssigned{
		JobMessageBase: ssapi.JobMessageBase{JobID: "j-1", RunnerRequestID: 1, RequestLabels: []string{"incuse-test"}},
	}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	waitForLaunches(t, fi, 1)

	clk.advance(11 * time.Minute)
	o.reapOnce(t.Context())

	_, stops, deletes := fi.snapshot()
	if len(stops) != 1 || len(deletes) != 1 {
		t.Errorf("expected stop+delete from registration timeout; stops=%v deletes=%v", stops, deletes)
	}
	if len(fs.removeCalls) != 1 {
		t.Errorf("expected RemoveRunner once on reg timeout; got %v", fs.removeCalls)
	}
	if o.tracker.size() != 0 {
		t.Errorf("tracker should be empty after reap")
	}
}

func TestReap_DoesNotReapBeforeTimeout(t *testing.T) {
	o, fi, _, clk := newTestOrchestrator(t, nil)

	if err := o.Mint(t.Context(), &ssapi.JobAssigned{
		JobMessageBase: ssapi.JobMessageBase{JobID: "j-1", RunnerRequestID: 1, RequestLabels: []string{"incuse-test"}},
	}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	waitForLaunches(t, fi, 1)

	clk.advance(5 * time.Minute)
	o.reapOnce(t.Context())

	if _, stops, _ := fi.snapshot(); len(stops) != 0 {
		t.Errorf("must not reap before registration timeout; stops=%v", stops)
	}
}

func TestReap_MaxJobDuration(t *testing.T) {
	o, fi, _, clk := newTestOrchestrator(t, nil)

	if err := o.Mint(t.Context(), &ssapi.JobAssigned{
		JobMessageBase: ssapi.JobMessageBase{JobID: "j-1", RunnerRequestID: 1, RequestLabels: []string{"incuse-test"}},
	}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	waitForLaunches(t, fi, 1)

	if err := o.HandleJobStarted(t.Context(), &ssapi.JobStarted{
		JobMessageBase: ssapi.JobMessageBase{JobID: "j-1", RunnerRequestID: 1},
	}); err != nil {
		t.Fatalf("started: %v", err)
	}

	clk.advance(7 * time.Hour)
	o.reapOnce(t.Context())

	if _, stops, _ := fi.snapshot(); len(stops) != 1 {
		t.Errorf("expected reap on max_job_duration; stops=%v", stops)
	}
}

func TestReap_DriftSweepDeletesOrphanManagedInstances(t *testing.T) {
	o, fi, _, _ := newTestOrchestrator(t, nil)

	fi.remote = []incus.Instance{
		{
			Name:   "leftover-from-previous-process",
			Status: "Running",
			Config: map[string]string{metaManaged: "true"},
		},
		{
			Name:   "not-ours",
			Status: "Running",
			Config: map[string]string{}, // no managed tag — leave alone
		},
	}

	o.reapOnce(t.Context())

	_, stops, deletes := fi.snapshot()
	if len(stops) != 1 || stops[0] != "leftover-from-previous-process" {
		t.Errorf("orphan stop: want [leftover-from-previous-process], got %v", stops)
	}
	if len(deletes) != 1 || deletes[0] != "leftover-from-previous-process" {
		t.Errorf("orphan delete: want [leftover-from-previous-process], got %v", deletes)
	}
}

func TestReap_DriftSweepIgnoresInstancesInTracker(t *testing.T) {
	o, fi, _, _ := newTestOrchestrator(t, nil)

	if err := o.Mint(t.Context(), &ssapi.JobAssigned{
		JobMessageBase: ssapi.JobMessageBase{JobID: "j-1", RunnerRequestID: 1, RequestLabels: []string{"incuse-test"}},
	}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	waitForLaunches(t, fi, 1)

	fi.remote = []incus.Instance{
		{
			Name:   "incuse-test-aaaa",
			Status: "Running",
			Config: map[string]string{metaManaged: "true"},
		},
	}

	o.reapOnce(t.Context())

	if _, stops, deletes := fi.snapshot(); len(stops) != 0 || len(deletes) != 0 {
		t.Errorf("must not reap own instances; stops=%v deletes=%v", stops, deletes)
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	o, _, _, _ := newTestOrchestrator(t, func(c *Config) {
		c.ReapInterval = time.Hour
	})
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

// fakeMetrics records every Metrics* call so tests can assert that
// the orchestrator drives the hook the way operators expect.
type fakeMetrics struct {
	mu              sync.Mutex
	jobAssigned     int
	launchOK        int
	launchFail      int
	launchDurations []float64
	runnerLifetimes []float64
	reapReasons     map[string]int
	trackedSetTo    []int
}

func newFakeMetrics() *fakeMetrics { return &fakeMetrics{reapReasons: map[string]int{}} }

func (f *fakeMetrics) JobAssigned() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobAssigned++
}
func (f *fakeMetrics) LaunchOK() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launchOK++
}
func (f *fakeMetrics) LaunchFail() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launchFail++
}
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
func (f *fakeMetrics) Reap(reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reapReasons[reason]++
}
func (f *fakeMetrics) SetTrackedInstances(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trackedSetTo = append(f.trackedSetTo, n)
}

func (f *fakeMetrics) snapshot() (jobAssigned, launchOK, launchFail int, reapReasons map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rr := make(map[string]int, len(f.reapReasons))
	for k, v := range f.reapReasons {
		rr[k] = v
	}
	return f.jobAssigned, f.launchOK, f.launchFail, rr
}

func TestMetrics_HappyPath_AssignedThenLaunchOKThenJobCompleted(t *testing.T) {
	fm := newFakeMetrics()
	o, fi, _, _ := newTestOrchestrator(t, func(c *Config) { c.Metrics = fm })
	ctx := t.Context()

	if err := o.Mint(ctx, &ssapi.JobAssigned{JobMessageBase: ssapi.JobMessageBase{JobID: "j-1", RunnerRequestID: 1}}); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	waitForLaunches(t, fi, 1)
	if err := o.HandleJobCompleted(ctx, &ssapi.JobCompleted{JobMessageBase: ssapi.JobMessageBase{JobID: "j-1", RunnerRequestID: 1}}); err != nil {
		t.Fatalf("HandleJobCompleted: %v", err)
	}

	assigned, ok, fail, reasons := fm.snapshot()
	if assigned != 1 {
		t.Errorf("jobAssigned: want 1, got %d", assigned)
	}
	if ok != 1 || fail != 0 {
		t.Errorf("launches: want ok=1 fail=0, got ok=%d fail=%d", ok, fail)
	}
	if reasons["job_completed"] != 1 {
		t.Errorf("reap reasons: want job_completed=1, got %v", reasons)
	}

	fm.mu.Lock()
	defer fm.mu.Unlock()
	if len(fm.launchDurations) != 1 {
		t.Errorf("launch durations: want 1 sample, got %d", len(fm.launchDurations))
	}
	if len(fm.runnerLifetimes) != 1 {
		t.Errorf("runner lifetimes: want 1 sample, got %d", len(fm.runnerLifetimes))
	}
}

func TestMetrics_LaunchFailureBumpsLaunchFail(t *testing.T) {
	fm := newFakeMetrics()
	o, fi, _, _ := newTestOrchestrator(t, func(c *Config) { c.Metrics = fm })
	fi.launchErr = errors.New("boom")
	if err := o.Mint(t.Context(), &ssapi.JobAssigned{JobMessageBase: ssapi.JobMessageBase{JobID: "j-1", RunnerRequestID: 1}}); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	waitForLaunches(t, fi, 1)

	_, ok, fail, _ := fm.snapshot()
	if ok != 0 || fail != 1 {
		t.Errorf("launches: want ok=0 fail=1, got ok=%d fail=%d", ok, fail)
	}
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if len(fm.launchDurations) != 1 {
		t.Errorf("launch durations: want 1 sample even on failure, got %d", len(fm.launchDurations))
	}
}

func TestMetrics_ReapReasonsRoute(t *testing.T) {
	fm := newFakeMetrics()
	o, _, _, clk := newTestOrchestrator(t, func(c *Config) {
		c.Metrics = fm
		c.RunnerCfg.RegistrationTimeout = time.Minute
	})
	if err := o.Mint(t.Context(), &ssapi.JobAssigned{JobMessageBase: ssapi.JobMessageBase{JobID: "j-1", RunnerRequestID: 1}}); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	clk.advance(2 * time.Minute)
	o.reapOnce(t.Context())

	_, _, _, reasons := fm.snapshot()
	if reasons["registration_timeout"] != 1 {
		t.Errorf("reap reasons: want registration_timeout=1, got %v", reasons)
	}
}

// Regression for the rocket deploy: every JobAssigned/JobCompleted on
// our broker session arrived with RunnerRequestID=0. Without
// JobID-based matching the tracker mis-routes JobCompleted to
// whatever runner happens to be in flight. JobID is the stable key.
func TestTracker_MatchesByJobIDWhenRunnerRequestIDIsZero(t *testing.T) {
	o, fi, _, _ := newTestOrchestrator(t, nil)
	ctx := t.Context()
	if err := o.Mint(ctx, &ssapi.JobAssigned{
		JobMessageBase: ssapi.JobMessageBase{
			JobID:           "abc-123",
			RunnerRequestID: 0,
		},
	}); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	waitForLaunches(t, fi, 1)

	if err := o.HandleJobCompleted(ctx, &ssapi.JobCompleted{
		JobMessageBase: ssapi.JobMessageBase{
			JobID:           "abc-123",
			RunnerRequestID: 0,
		},
	}); err != nil {
		t.Fatalf("HandleJobCompleted: %v", err)
	}
	_, stops, deletes := fi.snapshot()
	if len(stops) != 1 || len(deletes) != 1 {
		t.Fatalf("want one stop+delete, got stops=%v deletes=%v", stops, deletes)
	}
	if o.tracker.size() != 0 {
		t.Errorf("tracker size: want 0, got %d", o.tracker.size())
	}
}

// Regression for the rocket deploy: a JobCompleted that arrives while
// IncusClient.Launch is still in flight must NOT call Stop/Delete
// (Incus refuses delete-during-create). Instead, set a termination
// flag so the launch goroutine handles teardown after Launch returns.
func TestRace_JobCompletedDuringLaunchDefersTeardown(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	o, fi, _, _ := newTestOrchestrator(t, nil)
	fi.launchGate = gate
	fi.launchEntered = entered
	ctx := t.Context()

	if err := o.Mint(ctx, &ssapi.JobAssigned{
		JobMessageBase: ssapi.JobMessageBase{JobID: "race-1"},
	}); err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Wait until the launch goroutine has actually entered
	// IncusClient.Launch (and is now blocked on gate). Otherwise the
	// goroutine might still be at the terminationPending pre-check
	// and bail early via the abort path, which is a different
	// behaviour than the one we're testing here.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Launch never entered")
	}

	// JobCompleted arrives mid-flight.
	if err := o.HandleJobCompleted(ctx, &ssapi.JobCompleted{
		JobMessageBase: ssapi.JobMessageBase{JobID: "race-1"},
	}); err != nil {
		t.Fatalf("HandleJobCompleted: %v", err)
	}

	// At this point Stop/Delete must NOT have been called — Launch
	// is still blocked.
	_, stops, deletes := fi.snapshot()
	if len(stops) != 0 || len(deletes) != 0 {
		t.Fatalf("Stop/Delete fired during launch: stops=%v deletes=%v", stops, deletes)
	}
	if o.tracker.size() != 1 {
		t.Fatalf("tracker should still hold the entry; size=%d", o.tracker.size())
	}

	// Unblock Launch. The goroutine should now see TerminationPending,
	// transition to running, and call Stop+Delete.
	close(gate)

	// Poll until teardown happens.
	for i := 0; i < 200; i++ {
		_, stops, deletes = fi.snapshot()
		if len(stops) == 1 && len(deletes) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(stops) != 1 || len(deletes) != 1 {
		t.Fatalf("expected one stop+delete after launch unblocks; stops=%v deletes=%v", stops, deletes)
	}
	if o.tracker.size() != 0 {
		t.Errorf("tracker should be empty after teardown; size=%d", o.tracker.size())
	}
}
