// Package observability owns the metrics + health surfaces for
// incuse. The package exposes one type, Recorder, that:
//
//   - implements sslistener.MetricsRecorder so the upstream listener
//     populates GitHub-side gauges/counters automatically;
//   - exposes incuse-specific Record* methods the orchestrator calls
//     from Mint, dispatchLaunch, terminateInstance, and the reaper;
//   - holds a *prometheus.Registry so the http server can scrape it
//     without touching globals.
//
// All metric names live under the `incuse_` prefix. Labels are kept
// minimal to avoid runaway cardinality on a host that may launch
// thousands of VMs over its lifetime.
package observability

import (
	"github.com/prometheus/client_golang/prometheus"

	ssapi "github.com/actions/scaleset"
)

// Recorder bundles every collector incuse emits. Build via New, and
// pass to scaleset.Options.MetricsRecorder + orchestrator.Config.Recorder.
type Recorder struct {
	registry *prometheus.Registry

	// Job lifecycle counters.
	runnersSpawned prometheus.Counter
	jobsStarted    prometheus.Counter
	jobsCompleted  *prometheus.CounterVec

	// Launch + reap.
	launches *prometheus.CounterVec
	reaps    *prometheus.CounterVec

	// Latency histograms.
	launchDuration prometheus.Histogram
	runnerLifetime prometheus.Histogram

	// Live state.
	trackedInstances prometheus.Gauge
	desiredRunners   prometheus.Gauge

	// GitHub-side state mirrored from RunnerScaleSetStatistic.
	statTotalAvailableJobs     prometheus.Gauge
	statTotalAcquiredJobs      prometheus.Gauge
	statTotalAssignedJobs      prometheus.Gauge
	statTotalRunningJobs       prometheus.Gauge
	statTotalRegisteredRunners prometheus.Gauge
	statTotalBusyRunners       prometheus.Gauge
	statTotalIdleRunners       prometheus.Gauge

	// Build-info gauge — convenient for "what version is running".
	buildInfo *prometheus.GaugeVec
}

// New wires every collector to a fresh registry and returns a ready
// Recorder. Callers may pass Recorder.Registry() to the http server.
func New(version, commit string) *Recorder {
	r := prometheus.NewRegistry()
	rec := &Recorder{
		registry: r,
		runnersSpawned: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "incuse",
			Name:      "runners_spawned_total",
			Help:      "Number of idle runners incuse has spawned to satisfy GitHub's desired-runner-count.",
		}),
		jobsStarted: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "incuse",
			Name:      "jobs_started_total",
			Help:      "Number of GitHub JobStarted messages observed.",
		}),
		jobsCompleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "incuse",
			Name:      "jobs_completed_total",
			Help:      "Number of GitHub JobCompleted messages observed.",
		}, []string{"result"}),
		launches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "incuse",
			Name:      "launches_total",
			Help:      "Incus VM launch attempts.",
		}, []string{"result"}),
		reaps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "incuse",
			Name:      "reaps_total",
			Help:      "Reaper terminations bucketed by reason.",
		}, []string{"reason"}),
		launchDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "incuse",
			Name:      "launch_duration_seconds",
			Help:      "Time spent inside IncusClient.Launch (create+start operation).",
			// VM cold-boot on commodity hardware lands in 5-30s; widen
			// past that to catch image pulls and slow daemons.
			Buckets: []float64{1, 2, 5, 10, 20, 30, 60, 120, 300},
		}),
		runnerLifetime: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "incuse",
			Name:      "runner_lifetime_seconds",
			Help:      "End-to-end VM lifetime: JobAssigned to terminate.",
			// Job durations are bimodal: <2 min for unit tests, hours
			// for builds. Buckets cover both.
			Buckets: []float64{30, 60, 120, 300, 600, 1800, 3600, 7200, 21600},
		}),
		trackedInstances: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "incuse",
			Name:      "tracked_instances",
			Help:      "Instances currently in the orchestrator's in-memory tracker.",
		}),
		desiredRunners: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "incuse",
			Name:      "desired_runners",
			Help:      "Most recent desired-runner-count GitHub asked for.",
		}),
		statTotalAvailableJobs:     newStatGauge("total_available_jobs"),
		statTotalAcquiredJobs:      newStatGauge("total_acquired_jobs"),
		statTotalAssignedJobs:      newStatGauge("total_assigned_jobs"),
		statTotalRunningJobs:       newStatGauge("total_running_jobs"),
		statTotalRegisteredRunners: newStatGauge("total_registered_runners"),
		statTotalBusyRunners:       newStatGauge("total_busy_runners"),
		statTotalIdleRunners:       newStatGauge("total_idle_runners"),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "incuse",
			Name:      "build_info",
			Help:      "Always 1; labels carry version + commit.",
		}, []string{"version", "commit"}),
	}
	rec.buildInfo.WithLabelValues(version, commit).Set(1)

	r.MustRegister(
		rec.runnersSpawned,
		rec.jobsStarted,
		rec.jobsCompleted,
		rec.launches,
		rec.reaps,
		rec.launchDuration,
		rec.runnerLifetime,
		rec.trackedInstances,
		rec.desiredRunners,
		rec.statTotalAvailableJobs,
		rec.statTotalAcquiredJobs,
		rec.statTotalAssignedJobs,
		rec.statTotalRunningJobs,
		rec.statTotalRegisteredRunners,
		rec.statTotalBusyRunners,
		rec.statTotalIdleRunners,
		rec.buildInfo,
	)
	return rec
}

func newStatGauge(name string) prometheus.Gauge {
	return prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "incuse",
		Subsystem: "scaleset",
		Name:      name,
		Help:      "Mirrors the corresponding RunnerScaleSetStatistic field from the upstream API.",
	})
}

// Registry returns the underlying registry so the http server can hand
// it to promhttp.HandlerFor.
func (r *Recorder) Registry() *prometheus.Registry {
	return r.registry
}

// --- sslistener.MetricsRecorder ---------------------------------------

// RecordStatistics is invoked by the upstream listener with every
// poll-cycle statistics payload.
func (r *Recorder) RecordStatistics(s *ssapi.RunnerScaleSetStatistic) {
	if s == nil {
		return
	}
	r.statTotalAvailableJobs.Set(float64(s.TotalAvailableJobs))
	r.statTotalAcquiredJobs.Set(float64(s.TotalAcquiredJobs))
	r.statTotalAssignedJobs.Set(float64(s.TotalAssignedJobs))
	r.statTotalRunningJobs.Set(float64(s.TotalRunningJobs))
	r.statTotalRegisteredRunners.Set(float64(s.TotalRegisteredRunners))
	r.statTotalBusyRunners.Set(float64(s.TotalBusyRunners))
	r.statTotalIdleRunners.Set(float64(s.TotalIdleRunners))
}

// RecordJobStarted bumps the started counter.
func (r *Recorder) RecordJobStarted(_ *ssapi.JobStarted) {
	r.jobsStarted.Inc()
}

// RecordJobCompleted bumps the completed counter. The upstream
// listener doesn't surface a success/failure flag in the message, so
// we bucket every completion as `result="seen"` and rely on
// orchestrator-side reap counters for failure modes.
func (r *Recorder) RecordJobCompleted(_ *ssapi.JobCompleted) {
	r.jobsCompleted.WithLabelValues("seen").Inc()
}

// RecordDesiredRunners records the most recent runner-count target.
func (r *Recorder) RecordDesiredRunners(count int) {
	r.desiredRunners.Set(float64(count))
}

// --- incuse-side hooks ------------------------------------------------

// RunnerSpawned increments when the orchestrator has decided to mint
// a new idle runner in response to GitHub's desired-runner-count.
func (r *Recorder) RunnerSpawned() {
	r.runnersSpawned.Inc()
}

// LaunchOK / LaunchFail bucket Incus launch outcomes.
func (r *Recorder) LaunchOK()   { r.launches.WithLabelValues("ok").Inc() }
func (r *Recorder) LaunchFail() { r.launches.WithLabelValues("fail").Inc() }

// LaunchDuration observes the wall-clock cost of one launch.
func (r *Recorder) LaunchDuration(seconds float64) {
	r.launchDuration.Observe(seconds)
}

// RunnerLifetime observes total VM lifetime from JobAssigned to
// terminate (whatever the cause).
func (r *Recorder) RunnerLifetime(seconds float64) {
	r.runnerLifetime.Observe(seconds)
}

// Reap buckets a reaper termination by reason. Reasons used by the
// orchestrator: "registration_timeout", "max_job_duration",
// "drift_sweep", "job_completed".
func (r *Recorder) Reap(reason string) {
	r.reaps.WithLabelValues(reason).Inc()
}

// SetTrackedInstances overwrites the tracker-size gauge.
func (r *Recorder) SetTrackedInstances(n int) {
	r.trackedInstances.Set(float64(n))
}

// Discard returns a Recorder-shaped value that drops every observation
// on the floor. Useful in tests and when ListenAddr is empty.
func Discard() *Recorder {
	// A "discard" recorder still satisfies the interface; we just
	// never expose its registry. New() with empty version/commit is
	// fine — nothing scrapes it.
	return New("", "")
}
