// Package orchestrator wires the GitHub scale-set listener,
// JIT-runner config minting, and Incus VM launches into one
// supervised loop. It implements upstream's sslistener.Scaler:
//
//   - HandleDesiredRunnerCount(ctx, count) — ensures
//     min(count, MaxRunners) runners (idle+busy) exist. Each new
//     runner is minted as an unattached idle JIT runner; GitHub's
//     broker assigns jobs to them after they register.
//   - HandleJobStarted(event) — marks the named runner busy. Reaper
//     switches from registration-timeout to max-job-duration.
//   - HandleJobCompleted(event) — terminates the named runner's VM.
//
// We do NOT intercept JobAssigned. Earlier versions of incuse did,
// minting one JIT per JobAssigned message — but GenerateJitRunnerConfig
// only takes a runner Name and WorkFolder, not a JobID, so the JIT
// doesn't bind to the specific job. The broker dispatches jobs to
// idle runners in its own order, racing with our mint-time
// expectations. Following upstream's Scaler pattern eliminates the
// race entirely; runner-to-job mapping is GitHub's job, not ours.
//
// The reaper goroutine catches what the event stream misses:
// registration-timeout victims (cloud-init or image broken),
// max-job-duration runaways (callbacks dropped), and drift between
// our in-memory map and the actual instance set on the Incus host.
package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	ssapi "github.com/actions/scaleset"

	"github.com/netwerk-io/incuse/internal/config"
	"github.com/netwerk-io/incuse/internal/incus"
	"github.com/netwerk-io/incuse/internal/runner"
)

// MetricsHook is the slice of *observability.Recorder the
// orchestrator drives. Pulled out as a package-local interface so
// tests don't have to spin up a Prometheus registry.
type MetricsHook interface {
	RunnerSpawned()
	LaunchOK()
	LaunchFail()
	LaunchDuration(seconds float64)
	RunnerLifetime(seconds float64)
	Reap(reason string)
	SetTrackedInstances(n int)
}

type noopMetrics struct{}

func (noopMetrics) RunnerSpawned()            {}
func (noopMetrics) LaunchOK()                 {}
func (noopMetrics) LaunchFail()               {}
func (noopMetrics) LaunchDuration(_ float64)  {}
func (noopMetrics) RunnerLifetime(_ float64)  {}
func (noopMetrics) Reap(_ string)             {}
func (noopMetrics) SetTrackedInstances(_ int) {}

// IncusClient is the slice of internal/incus.Client the orchestrator
// drives. Pulled out as a package-local interface so tests can wire
// a fake without depending on the upstream Incus REST shapes.
type IncusClient interface {
	Launch(ctx context.Context, req incus.LaunchRequest) (*incus.Instance, error)
	Stop(ctx context.Context, name string) error
	Delete(ctx context.Context, name string) error
	List(ctx context.Context, projectFilter string) ([]incus.Instance, error)
}

// ScaleSetClient is the slice of *scaleset.ScaleSet the orchestrator
// uses. Production wires *scaleset.ScaleSet directly.
type ScaleSetClient interface {
	GenerateJITConfig(ctx context.Context, runnerName, workFolder string) ([]byte, *ssapi.RunnerReference, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
	SetMaxRunners(count int)
	ScaleSetID() int
	Spec() config.ScaleSetConfig
}

// ReleaseResolver returns the current actions/runner release. Backed
// by *runner.LatestResolver in production.
type ReleaseResolver interface {
	Resolve(ctx context.Context) (runner.Release, error)
}

// CloudInitRenderer renders the per-launch #cloud-config payload.
// var so tests can override; production points at runner.Render.
var CloudInitRenderer = runner.Render

// Config bundles the orchestrator's collaborators and tunables.
type Config struct {
	IncusClient     IncusClient
	ScaleSet        ScaleSetClient
	ReleaseResolver ReleaseResolver
	IncusCfg        config.IncusConfig
	RunnerCfg       config.RunnerConfig

	// HostArch is runtime.GOARCH on the orchestrator process. Test
	// override; defaults to runtime.GOARCH.
	HostArch string

	// Logger is required.
	Logger *slog.Logger

	// ReapInterval is how often the reaper sweeps. Defaults to 30s.
	ReapInterval time.Duration

	// Now is a clock hook for tests; defaults to time.Now.
	Now func() time.Time

	// NameSuffix returns a runner-name suffix; defaults to a random
	// 8-byte hex. Test override gives deterministic instance names.
	NameSuffix func() string

	// Metrics is optional. nil installs a no-op hook so the
	// orchestrator never has to nil-check.
	Metrics MetricsHook
}

// Orchestrator is the long-lived process glue. Construct via New.
type Orchestrator struct {
	cfg     Config
	tracker *instanceTracker

	// spawnSem caps in-flight runner spawns. Buffer size = MaxRunners
	// so a burst HandleDesiredRunnerCount call only blocks when we
	// are at capacity.
	spawnSem chan struct{}

	// spawnInflight counts runners we've decided to spawn but
	// haven't yet added to the tracker. Prevents a fast-fire
	// HandleDesiredRunnerCount loop from over-spawning.
	spawnInflight int
	spawnMu       sync.Mutex
}

// New validates the config and returns a ready orchestrator.
func New(cfg Config) (*Orchestrator, error) {
	if cfg.IncusClient == nil {
		return nil, errors.New("incus client is required")
	}
	if cfg.ScaleSet == nil {
		return nil, errors.New("scale set is required")
	}
	if cfg.ReleaseResolver == nil {
		return nil, errors.New("release resolver is required")
	}
	if cfg.Logger == nil {
		return nil, errors.New("logger is required")
	}
	if cfg.IncusCfg.Project == "" {
		return nil, errors.New("incus.project is required")
	}
	if cfg.IncusCfg.DefaultProfile == "" {
		return nil, errors.New("incus.default_profile is required")
	}
	if cfg.RunnerCfg.RegistrationTimeout <= 0 {
		return nil, errors.New("runner.registration_timeout must be positive")
	}
	if cfg.RunnerCfg.MaxJobDuration <= 0 {
		return nil, errors.New("runner.max_job_duration must be positive")
	}
	if len(cfg.RunnerCfg.VCPUTiers) == 0 {
		return nil, errors.New("runner.vcpu_tiers must have at least one tier")
	}
	max := cfg.ScaleSet.Spec().MaxRunners
	if max <= 0 {
		return nil, errors.New("scale_set.max_runners must be positive")
	}

	if cfg.HostArch == "" {
		cfg.HostArch = runtime.GOARCH
	}
	if cfg.ReapInterval == 0 {
		cfg.ReapInterval = 30 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NameSuffix == nil {
		cfg.NameSuffix = randomNameSuffix
	}
	if cfg.Metrics == nil {
		cfg.Metrics = noopMetrics{}
	}

	return &Orchestrator{
		cfg:      cfg,
		tracker:  newInstanceTracker(),
		spawnSem: make(chan struct{}, max),
	}, nil
}

// Run blocks until ctx is cancelled, ticking the reaper on
// cfg.ReapInterval. Listener events arrive concurrently via the
// Scaler methods.
func (o *Orchestrator) Run(ctx context.Context) error {
	t := time.NewTicker(o.cfg.ReapInterval)
	defer t.Stop()

	o.cfg.Logger.Info("orchestrator running",
		"reap_interval", o.cfg.ReapInterval,
		"max_runners", o.cfg.ScaleSet.Spec().MaxRunners,
		"project", o.cfg.IncusCfg.Project,
	)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			o.reapOnce(ctx)
		}
	}
}

// HandleDesiredRunnerCount implements sslistener.Scaler. GitHub's
// broker tells us how many jobs are assigned to our scale set; we
// scale the runner pool up to match, capped at MaxRunners. We never
// scale down idle runners — JobCompleted handles teardown of busy
// ones, and idle runners that never get a job are caught by the
// registration-timeout reaper.
func (o *Orchestrator) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	max := o.cfg.ScaleSet.Spec().MaxRunners
	target := count
	if target > max {
		target = max
	}
	if target < 0 {
		target = 0
	}

	o.spawnMu.Lock()
	current := o.tracker.size() + o.spawnInflight
	scaleUp := target - current
	if scaleUp > 0 {
		o.spawnInflight += scaleUp
	}
	o.spawnMu.Unlock()

	o.cfg.Logger.Debug("desired runner count",
		"requested", count,
		"target", target,
		"current", current,
		"scale_up", scaleUp,
	)

	for i := 0; i < scaleUp; i++ {
		o.spawnIdleRunner(ctx)
	}

	return target, nil
}

// HandleJobStarted implements sslistener.Scaler. Marks the named
// runner busy.
func (o *Orchestrator) HandleJobStarted(_ context.Context, event *ssapi.JobStarted) error {
	if event == nil || event.RunnerName == "" {
		return nil
	}
	prev, ok := o.tracker.markBusy(event.RunnerName, o.cfg.Now())
	if !ok {
		o.cfg.Logger.Warn("job started for unknown runner",
			"runner_name", event.RunnerName,
			"job_id", event.JobID,
		)
		return nil
	}
	o.cfg.Logger.Info("job started",
		"runner_name", event.RunnerName,
		"job_id", event.JobID,
		"previous_state", prev,
	)
	return nil
}

// HandleJobCompleted implements sslistener.Scaler. Terminates the
// named runner's VM and removes it from the tracker.
func (o *Orchestrator) HandleJobCompleted(ctx context.Context, event *ssapi.JobCompleted) error {
	if event == nil || event.RunnerName == "" {
		return nil
	}
	if _, ok := o.tracker.get(event.RunnerName); !ok {
		o.cfg.Logger.Warn("job completed for unknown runner",
			"runner_name", event.RunnerName,
			"job_id", event.JobID,
			"result", event.Result,
		)
		return nil
	}
	o.cfg.Logger.Info("job completed",
		"runner_name", event.RunnerName,
		"job_id", event.JobID,
		"result", event.Result,
	)
	o.cfg.Metrics.Reap("job_completed")
	o.terminateInstance(ctx, event.RunnerName, "job completed")
	return nil
}

// spawnIdleRunner mints a JIT-runner config, renders cloud-init, and
// dispatches an Incus launch in the background. The listener thread
// returns immediately so a burst HandleDesiredRunnerCount call
// doesn't block on hypervisor work.
//
// Runners are minted at the smallest configured VCPU tier — at idle
// mint time we don't know what labels a future JobAssigned will
// carry, so we go conservative. Multi-tier support requires multiple
// scale sets, matching upstream ARC's pattern.
func (o *Orchestrator) spawnIdleRunner(ctx context.Context) {
	defer func() {
		o.spawnMu.Lock()
		o.spawnInflight--
		o.spawnMu.Unlock()
	}()

	spec := defaultSpec(o.cfg.RunnerCfg, o.cfg.HostArch)

	var release runner.Release
	if !o.cfg.RunnerCfg.UseBakedImage {
		// Vanilla mode needs the runner tarball URL in cloud-init.
		// Baked mode skips this — the version is whatever was baked
		// into the image at scripts/build-runner-image.sh time.
		var err error
		release, err = o.cfg.ReleaseResolver.Resolve(ctx)
		if err != nil {
			o.cfg.Logger.Error("resolve runner release", "error", err)
			return
		}
	}

	runnerName := makeRunnerName(o.cfg.ScaleSet.Spec().Name, o.cfg.NameSuffix())
	jit, ref, err := o.cfg.ScaleSet.GenerateJITConfig(ctx, runnerName, o.cfg.RunnerCfg.WorkFolder)
	if err != nil {
		o.cfg.Logger.Error("mint jit config", "error", err, "runner_name", runnerName)
		return
	}

	cloudInit, err := CloudInitRenderer(runner.CloudInitSpec{
		Release:    release,
		JITConfig:  string(jit),
		WorkFolder: o.cfg.RunnerCfg.WorkFolder,
		RunnerName: runnerName,
		Baked:      o.cfg.RunnerCfg.UseBakedImage,
		Container:  o.cfg.RunnerCfg.InstanceType == config.InstanceTypeContainer,
	})
	if err != nil {
		o.cfg.Logger.Error("render cloud-init", "error", err, "runner_name", runnerName)
		o.removeRunnerBestEffort(ctx, ref, runnerName, "cloud-init render failed")
		return
	}

	mintedAt := o.cfg.Now()
	req := buildLaunchRequest(launchInputs{
		runnerName:  runnerName,
		spec:        spec,
		incusCfg:    o.cfg.IncusCfg,
		runnerCfg:   o.cfg.RunnerCfg,
		cloudInit:   cloudInit,
		scaleSetID:  o.cfg.ScaleSet.ScaleSetID(),
		mintedAt:    mintedAt,
		description: fmt.Sprintf("incuse runner %s", runnerName),
	})

	tracked := &trackedRunner{
		Name:       runnerName,
		RunnerID:   runnerRefID(ref),
		LaunchedAt: mintedAt,
		Spec:       spec,
		ScaleSetID: o.cfg.ScaleSet.ScaleSetID(),
		State:      statusLaunching,
	}
	o.tracker.add(tracked)
	o.cfg.Metrics.RunnerSpawned()
	o.cfg.Metrics.SetTrackedInstances(o.tracker.size())

	o.cfg.Logger.Info("spawning runner",
		"runner_name", runnerName,
		"vcpu", spec.VCPUs,
		"mem_mb", spec.MemoryMB,
		"disk_gb", spec.DiskGB,
		"arch", spec.Arch,
	)

	o.dispatchLaunch(ctx, req, tracked)
}

// dispatchLaunch starts the Launch goroutine. Bounded by spawnSem so
// a 100-runner burst doesn't fork-bomb Incus.
func (o *Orchestrator) dispatchLaunch(ctx context.Context, req incus.LaunchRequest, tracked *trackedRunner) {
	go func() {
		select {
		case o.spawnSem <- struct{}{}:
		case <-ctx.Done():
			o.tracker.remove(tracked.Name)
			o.cfg.Metrics.SetTrackedInstances(o.tracker.size())
			return
		}
		defer func() { <-o.spawnSem }()

		if o.tracker.terminationPending(tracked.Name) {
			o.cfg.Logger.Info("aborting launch; termination requested before create",
				"runner_name", tracked.Name,
			)
			o.tracker.remove(tracked.Name)
			o.cfg.Metrics.SetTrackedInstances(o.tracker.size())
			o.removeRunnerByID(ctx, tracked.RunnerID, tracked.Name, "termination during launch")
			return
		}

		start := o.cfg.Now()
		inst, err := o.cfg.IncusClient.Launch(ctx, req)
		o.cfg.Metrics.LaunchDuration(o.cfg.Now().Sub(start).Seconds())
		if err != nil {
			o.cfg.Metrics.LaunchFail()
			o.cfg.Logger.Error("launch failed",
				"runner_name", tracked.Name,
				"error", err,
			)
			o.tracker.remove(tracked.Name)
			o.cfg.Metrics.SetTrackedInstances(o.tracker.size())
			o.removeRunnerByID(ctx, tracked.RunnerID, tracked.Name, "launch failed")
			return
		}

		o.cfg.Metrics.LaunchOK()
		terminationRequested, _ := o.tracker.markIdle(tracked.Name, o.cfg.Now())
		if terminationRequested {
			o.cfg.Logger.Info("launch ok but termination requested mid-flight; tearing down",
				"runner_name", tracked.Name,
			)
			o.terminateInstance(ctx, tracked.Name, "deferred from launching")
			o.removeRunnerByID(ctx, tracked.RunnerID, tracked.Name, "termination during launch")
			return
		}
		o.cfg.Logger.Info("launch ok",
			"runner_name", tracked.Name,
			"status", inst.Status,
		)
	}()
}

// terminateInstance stops + deletes a managed instance and removes
// it from the tracker. Errors are logged + swallowed; the reaper
// retries on the next sweep.
func (o *Orchestrator) terminateInstance(ctx context.Context, runnerName, reason string) {
	if state, ok := o.tracker.markForTermination(runnerName); ok && state == statusLaunching {
		o.cfg.Logger.Info("deferring termination until launch completes",
			"runner_name", runnerName,
			"reason", reason,
		)
		return
	}
	if r, ok := o.tracker.get(runnerName); ok {
		o.cfg.Metrics.RunnerLifetime(o.cfg.Now().Sub(r.LaunchedAt).Seconds())
	}
	o.tracker.remove(runnerName)
	o.cfg.Metrics.SetTrackedInstances(o.tracker.size())
	if err := o.cfg.IncusClient.Stop(ctx, runnerName); err != nil {
		o.cfg.Logger.Warn("stop failed (will retry via reaper drift sweep)",
			"runner_name", runnerName,
			"reason", reason,
			"error", err,
		)
	}
	if err := o.cfg.IncusClient.Delete(ctx, runnerName); err != nil {
		o.cfg.Logger.Warn("delete failed (will retry via reaper drift sweep)",
			"runner_name", runnerName,
			"reason", reason,
			"error", err,
		)
	}
}

// removeRunnerBestEffort tells GitHub to drop the JIT-minted runner
// when we can't make use of it (e.g. cloud-init render failed before
// we ever launched the VM).
func (o *Orchestrator) removeRunnerBestEffort(ctx context.Context, ref *ssapi.RunnerReference, runnerName, reason string) {
	if ref == nil {
		return
	}
	o.removeRunnerByID(ctx, int64(ref.ID), runnerName, reason)
}

func (o *Orchestrator) removeRunnerByID(ctx context.Context, runnerID int64, runnerName, reason string) {
	if runnerID == 0 {
		return
	}
	if err := o.cfg.ScaleSet.RemoveRunner(ctx, runnerID); err != nil {
		o.cfg.Logger.Warn("remove runner from github failed",
			"runner_name", runnerName,
			"runner_id", runnerID,
			"reason", reason,
			"error", err,
		)
	}
}

// defaultSpec returns the smallest-tier runner shape for new idle
// runners. The HostArch defaults to the orchestrator's runtime.GOARCH;
// future multi-arch support would parse a label-driven spec, but for
// MVP we mint at a single shape.
func defaultSpec(rc config.RunnerConfig, hostArch string) config.RunnerSpec {
	tier := rc.VCPUTiers[0]
	for _, t := range rc.VCPUTiers {
		if t < tier {
			tier = t
		}
	}
	return config.RunnerSpec{
		VCPUs:    tier,
		MemoryMB: tier * rc.MemoryPerVCPUMiB,
		DiskGB:   rc.RootDiskGiB,
		Arch:     hostArch,
	}
}

// makeRunnerName returns "<scaleset>-<suffix>", lowercased and
// truncated to 63 chars (Incus instance name limit).
func makeRunnerName(scaleSet, suffix string) string {
	name := strings.ToLower(scaleSet + "-" + suffix)
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

func randomNameSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b[:])
}

func runnerRefID(ref *ssapi.RunnerReference) int64 {
	if ref == nil {
		return 0
	}
	return int64(ref.ID)
}
