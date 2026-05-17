// Package orchestrator wires the GitHub scale-set listener, JIT
// runner config minting, and Incus VM launches into one supervised
// loop. It implements the contracts upstream expects:
//
//   - scaleset.JITMinter — invoked synchronously by the listener
//     decorator on every JobAssigned. The orchestrator picks a runner
//     shape from the request labels, mints a JIT config, renders
//     cloud-init, and dispatches an Incus launch in the background so
//     the listener's poll loop is not blocked on hypervisor work.
//
//   - sslistener.Scaler — invoked by the upstream listener with
//     JobStarted, JobCompleted, and DesiredRunnerCount events. The
//     orchestrator transitions instances through the registration ->
//     running -> completed lifecycle and reports remaining capacity
//     so GitHub stops assigning past max_runners.
//
// The reaper goroutine catches what the event stream misses:
// registration-timeout victims (cloud-init or image broken), max-job-
// duration runaways (callbacks dropped), and drift between our
// in-memory map and the actual instance set on the Incus host (covers
// crash recovery on systemd restart).
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
	"time"

	ssapi "github.com/actions/scaleset"

	"github.com/netwerk-io/incuse/internal/config"
	"github.com/netwerk-io/incuse/internal/incus"
	"github.com/netwerk-io/incuse/internal/runner"
)

// MetricsHook is the slice of *observability.Recorder the
// orchestrator drives. Pulled out as a package-local interface so
// tests don't have to spin up a Prometheus registry, and so a
// no-metrics build path is a one-liner default.
type MetricsHook interface {
	JobAssigned()
	LaunchOK()
	LaunchFail()
	LaunchDuration(seconds float64)
	RunnerLifetime(seconds float64)
	Reap(reason string)
	SetTrackedInstances(n int)
}

type noopMetrics struct{}

func (noopMetrics) JobAssigned()              {}
func (noopMetrics) LaunchOK()                 {}
func (noopMetrics) LaunchFail()               {}
func (noopMetrics) LaunchDuration(_ float64)  {}
func (noopMetrics) RunnerLifetime(_ float64)  {}
func (noopMetrics) Reap(_ string)             {}
func (noopMetrics) SetTrackedInstances(_ int) {}

// IncusClient is the slice of internal/incus.Client the orchestrator
// drives. Pulled out as a package-local interface so tests can wire a
// fake without depending on the upstream Incus REST shapes.
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

// CloudInitRenderer renders the per-launch #cloud-config payload. var
// so tests can override; production points at runner.Render.
var CloudInitRenderer = runner.Render

// Config bundles the orchestrator's collaborators and tunables.
type Config struct {
	// IncusClient is the wrapper around the local Incus daemon.
	IncusClient IncusClient
	// ScaleSet is the bootstrapped scaleset.ScaleSet that owns the
	// listener + JIT minting.
	ScaleSet ScaleSetClient
	// ReleaseResolver feeds runner version + download URL into the
	// cloud-init template.
	ReleaseResolver ReleaseResolver

	// IncusCfg holds project + default profile + image alias.
	IncusCfg config.IncusConfig
	// RunnerCfg holds vCPU tiers, memory ratio, disk size, timeouts.
	RunnerCfg config.RunnerConfig

	// HostArch is runtime.GOARCH on the orchestrator process. Falls
	// back to runtime.GOARCH if empty. Test override.
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

	// launchSem caps in-flight Incus launches. Buffer size = MaxRunners
	// so a Mint call only blocks when we are at capacity.
	launchSem chan struct{}
}

// New validates the config and returns a ready orchestrator. The
// returned value implements scaleset.JITMinter and sslistener.Scaler.
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
		cfg:       cfg,
		tracker:   newInstanceTracker(),
		launchSem: make(chan struct{}, max),
	}, nil
}

// Run blocks until ctx is cancelled, ticking the reaper on
// cfg.ReapInterval. Listener events arrive concurrently via the
// JITMinter / Scaler methods; nothing here serialises with them.
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

// Mint implements scaleset.JITMinter. Called by the listener
// decorator on every JobAssigned. Returns nil even on partial failure
// so a single bad job cannot poison the message queue — the reaper is
// the ultimate cleanup path.
func (o *Orchestrator) Mint(ctx context.Context, event *ssapi.JobAssigned) error {
	if event == nil {
		return nil
	}
	logger := o.cfg.Logger.With(
		"job_id", event.JobID,
		"workflow_run_id", event.WorkflowRunID,
		"runner_request_id", event.RunnerRequestID,
		"request_labels", event.RequestLabels,
	)

	spec, err := config.ResolveRunnerSpec(o.cfg.RunnerCfg, o.cfg.HostArch, event.RequestLabels)
	if err != nil {
		logger.Error("resolve runner spec", "error", err)
		return nil
	}

	release, err := o.cfg.ReleaseResolver.Resolve(ctx)
	if err != nil {
		logger.Error("resolve runner release", "error", err)
		return nil
	}

	runnerName := makeRunnerName(o.cfg.ScaleSet.Spec().Name, o.cfg.NameSuffix())
	jit, ref, err := o.cfg.ScaleSet.GenerateJITConfig(ctx, runnerName, o.cfg.RunnerCfg.WorkFolder)
	if err != nil {
		logger.Error("mint jit config", "error", err, "runner_name", runnerName)
		return nil
	}

	cloudInit, err := CloudInitRenderer(runner.CloudInitSpec{
		Release:    release,
		JITConfig:  string(jit),
		WorkFolder: o.cfg.RunnerCfg.WorkFolder,
		RunnerName: runnerName,
	})
	if err != nil {
		logger.Error("render cloud-init", "error", err, "runner_name", runnerName)
		o.removeRunnerBestEffort(ctx, ref, runnerName, "cloud-init render failed")
		return nil
	}

	mintedAt := o.cfg.Now()
	req := buildLaunchRequest(launchInputs{
		runnerName:      runnerName,
		spec:            spec,
		incusCfg:        o.cfg.IncusCfg,
		runnerCfg:       o.cfg.RunnerCfg,
		cloudInit:       cloudInit,
		jobID:           event.JobID,
		workflowRunID:   event.WorkflowRunID,
		runnerRequestID: event.RunnerRequestID,
		scaleSetID:      o.cfg.ScaleSet.ScaleSetID(),
		mintedAt:        mintedAt,
		description:     fmt.Sprintf("incuse runner %s (job %s)", runnerName, event.JobID),
	})

	tracked := &trackedInstance{
		RunnerName:      runnerName,
		JobID:           event.JobID,
		RunnerRequestID: event.RunnerRequestID,
		WorkflowRunID:   event.WorkflowRunID,
		RunnerID:        runnerRefID(ref),
		LaunchedAt:      mintedAt,
		Spec:            spec,
		ScaleSetID:      o.cfg.ScaleSet.ScaleSetID(),
		Status:          statusLaunching,
	}
	o.tracker.add(tracked)
	o.cfg.Metrics.JobAssigned()
	o.cfg.Metrics.SetTrackedInstances(o.tracker.size())

	logger.Info("launching runner",
		"runner_name", runnerName,
		"vcpu", spec.VCPUs,
		"mem_mb", spec.MemoryMB,
		"disk_gb", spec.DiskGB,
		"arch", spec.Arch,
	)

	o.dispatchLaunch(ctx, req, tracked, logger)
	return nil
}

// HandleJobStarted implements sslistener.Scaler. Stamps when the
// runner picked up the job so the reaper can switch from registration-
// timeout to max-job-duration semantics.
func (o *Orchestrator) HandleJobStarted(_ context.Context, event *ssapi.JobStarted) error {
	if event == nil {
		return nil
	}
	now := o.cfg.Now()
	matched := o.tracker.markStartedByJobID(event.JobID, now)
	if matched == "" {
		o.cfg.Logger.Warn("job started for unknown job id",
			"job_id", event.JobID,
			"runner_name", event.RunnerName,
			"runner_request_id", event.RunnerRequestID,
		)
		return nil
	}
	o.cfg.Logger.Info("job started",
		"runner_name", matched,
		"job_id", event.JobID,
		"runner_request_id", event.RunnerRequestID,
	)
	return nil
}

// HandleJobCompleted implements sslistener.Scaler. Stops + deletes the
// instance immediately. We don't wait for the cloud-init poweroff path
// to take effect because GitHub already considers the job done.
func (o *Orchestrator) HandleJobCompleted(ctx context.Context, event *ssapi.JobCompleted) error {
	if event == nil {
		return nil
	}
	matched := o.tracker.getByJobID(event.JobID)
	if matched == nil {
		o.cfg.Logger.Warn("job completed for unknown job id",
			"job_id", event.JobID,
			"runner_name", event.RunnerName,
			"runner_request_id", event.RunnerRequestID,
		)
		return nil
	}
	o.cfg.Logger.Info("job completed",
		"runner_name", matched.RunnerName,
		"job_id", event.JobID,
		"runner_request_id", event.RunnerRequestID,
		"result", event.Result,
	)
	o.cfg.Metrics.Reap("job_completed")
	o.terminateInstance(ctx, matched.RunnerName, "job completed")
	return nil
}

// HandleDesiredRunnerCount implements sslistener.Scaler. Returns the
// capacity GitHub should advertise on the next poll. Capped at the
// configured max_runners — we never run more VMs than the host can
// take.
func (o *Orchestrator) HandleDesiredRunnerCount(_ context.Context, count int) (int, error) {
	max := o.cfg.ScaleSet.Spec().MaxRunners
	cap := count
	if cap > max {
		cap = max
	}
	if cap < 0 {
		cap = 0
	}
	o.cfg.Logger.Debug("desired runner count",
		"requested", count,
		"granted", cap,
		"in_flight", o.tracker.size(),
	)
	return cap, nil
}

// dispatchLaunch starts the Launch goroutine. The semaphore caps
// concurrency so a 100-job burst doesn't fork-bomb Incus.
func (o *Orchestrator) dispatchLaunch(ctx context.Context, req incus.LaunchRequest, tracked *trackedInstance, logger *slog.Logger) {
	go func() {
		select {
		case o.launchSem <- struct{}{}:
		case <-ctx.Done():
			o.tracker.remove(tracked.RunnerName)
			o.cfg.Metrics.SetTrackedInstances(o.tracker.size())
			return
		}
		defer func() { <-o.launchSem }()

		// Bail early if termination was requested while we waited on
		// the semaphore. Skips a wasted CreateInstance round-trip.
		if o.tracker.terminationPending(tracked.RunnerName) {
			logger.Info("aborting launch; termination requested before create",
				"runner_name", tracked.RunnerName,
			)
			o.tracker.remove(tracked.RunnerName)
			o.cfg.Metrics.SetTrackedInstances(o.tracker.size())
			o.removeRunnerByID(ctx, tracked.RunnerID, tracked.RunnerName, "termination during launch")
			return
		}

		start := o.cfg.Now()
		inst, err := o.cfg.IncusClient.Launch(ctx, req)
		o.cfg.Metrics.LaunchDuration(o.cfg.Now().Sub(start).Seconds())
		if err != nil {
			o.cfg.Metrics.LaunchFail()
			logger.Error("launch failed",
				"runner_name", tracked.RunnerName,
				"error", err,
			)
			o.tracker.remove(tracked.RunnerName)
			o.cfg.Metrics.SetTrackedInstances(o.tracker.size())
			o.removeRunnerByID(ctx, tracked.RunnerID, tracked.RunnerName, "launch failed")
			return
		}

		o.cfg.Metrics.LaunchOK()
		terminationRequested, _ := o.tracker.markLaunched(tracked.RunnerName, o.cfg.Now())
		if terminationRequested {
			logger.Info("launch ok but termination requested mid-flight; tearing down",
				"runner_name", tracked.RunnerName,
			)
			o.terminateInstance(ctx, tracked.RunnerName, "deferred from launching")
			o.removeRunnerByID(ctx, tracked.RunnerID, tracked.RunnerName, "termination during launch")
			return
		}
		logger.Info("launch ok",
			"runner_name", tracked.RunnerName,
			"status", inst.Status,
		)
	}()
}

// terminateInstance stops + deletes a managed instance and removes it
// from the tracker. Errors are logged + swallowed; the reaper will
// retry on the next sweep.
//
// Termination during launch is deferred: Incus refuses
// delete-during-create, so if the entry is still in statusLaunching
// we set TerminationPending and let the launch goroutine handle the
// teardown after CreateInstance returns. The runner ID stays in the
// tracker entry so the launch goroutine can also clean up the
// GitHub-side registration.
func (o *Orchestrator) terminateInstance(ctx context.Context, runnerName, reason string) {
	if status, ok := o.tracker.markForTermination(runnerName); ok && status == statusLaunching {
		o.cfg.Logger.Info("deferring termination until launch completes",
			"runner_name", runnerName,
			"reason", reason,
		)
		return
	}
	if inst, ok := o.tracker.get(runnerName); ok {
		o.cfg.Metrics.RunnerLifetime(o.cfg.Now().Sub(inst.LaunchedAt).Seconds())
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
// we ever launched the VM). Otherwise GitHub would keep
// total_assigned high and re-emit JobAssigned in a few minutes.
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

// makeRunnerName returns "<scaleset>-<suffix>", lowercased and
// truncated to 63 chars (Incus instance name limit). Suffix is 8 hex
// chars in production — collision risk on a single host is negligible.
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
		// crypto/rand failing is catastrophic; fall back to nanos so we
		// at least keep moving.
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
