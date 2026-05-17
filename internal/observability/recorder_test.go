package observability

import (
	"testing"

	ssapi "github.com/actions/scaleset"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecorder_RecordStatistics(t *testing.T) {
	rec := New("v0", "abc")
	rec.RecordStatistics(&ssapi.RunnerScaleSetStatistic{
		TotalAvailableJobs:     5,
		TotalAcquiredJobs:      4,
		TotalAssignedJobs:      3,
		TotalRunningJobs:       2,
		TotalRegisteredRunners: 7,
		TotalBusyRunners:       2,
		TotalIdleRunners:       5,
	})
	if got := testutil.ToFloat64(rec.statTotalAvailableJobs); got != 5 {
		t.Errorf("available: want 5, got %v", got)
	}
	if got := testutil.ToFloat64(rec.statTotalRegisteredRunners); got != 7 {
		t.Errorf("registered: want 7, got %v", got)
	}
}

func TestRecorder_NilStatisticsIsNoop(t *testing.T) {
	rec := New("", "")
	rec.RecordStatistics(nil) // must not panic
}

func TestRecorder_JobLifecycleCounters(t *testing.T) {
	rec := New("", "")

	rec.RunnerSpawned()
	rec.RunnerSpawned()
	rec.RecordJobStarted(nil)
	rec.RecordJobCompleted(nil)
	rec.RecordJobCompleted(nil)
	rec.RecordJobCompleted(nil)
	rec.RecordDesiredRunners(8)

	if got := testutil.ToFloat64(rec.runnersSpawned); got != 2 {
		t.Errorf("jobs_assigned: want 2, got %v", got)
	}
	if got := testutil.ToFloat64(rec.jobsStarted); got != 1 {
		t.Errorf("jobs_started: want 1, got %v", got)
	}
	if got := testutil.ToFloat64(rec.jobsCompleted.WithLabelValues("seen")); got != 3 {
		t.Errorf("jobs_completed: want 3, got %v", got)
	}
	if got := testutil.ToFloat64(rec.desiredRunners); got != 8 {
		t.Errorf("desired_runners: want 8, got %v", got)
	}
}

func TestRecorder_LaunchCounters(t *testing.T) {
	rec := New("", "")
	rec.LaunchOK()
	rec.LaunchOK()
	rec.LaunchFail()
	rec.LaunchDuration(12.5)

	if got := testutil.ToFloat64(rec.launches.WithLabelValues("ok")); got != 2 {
		t.Errorf("launches ok: %v", got)
	}
	if got := testutil.ToFloat64(rec.launches.WithLabelValues("fail")); got != 1 {
		t.Errorf("launches fail: %v", got)
	}
	// Histogram count is the only thing testutil exposes simply.
	if got := testutil.CollectAndCount(rec.launchDuration); got != 1 {
		t.Errorf("launch_duration sample-count: want 1, got %v", got)
	}
}

func TestRecorder_ReapBuckets(t *testing.T) {
	rec := New("", "")
	rec.Reap("registration_timeout")
	rec.Reap("registration_timeout")
	rec.Reap("drift_sweep")

	if got := testutil.ToFloat64(rec.reaps.WithLabelValues("registration_timeout")); got != 2 {
		t.Errorf("reg timeout reaps: %v", got)
	}
	if got := testutil.ToFloat64(rec.reaps.WithLabelValues("drift_sweep")); got != 1 {
		t.Errorf("drift sweep reaps: %v", got)
	}
}

func TestRecorder_BuildInfoLabelled(t *testing.T) {
	rec := New("v1.2.3", "deadbee")
	got := testutil.ToFloat64(rec.buildInfo.WithLabelValues("v1.2.3", "deadbee"))
	if got != 1 {
		t.Errorf("build_info: want 1, got %v", got)
	}
}

func TestRecorder_RegistersAllMetrics(t *testing.T) {
	rec := New("v0", "abc")
	rec.RunnerSpawned()
	rec.LaunchOK()
	rec.SetTrackedInstances(3)

	metrics, err := rec.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := make(map[string]bool, len(metrics))
	for _, m := range metrics {
		names[m.GetName()] = true
	}
	for _, want := range []string{
		"incuse_runners_spawned_total",
		"incuse_launches_total",
		"incuse_tracked_instances",
		"incuse_build_info",
		"incuse_scaleset_total_available_jobs",
	} {
		if !names[want] {
			t.Errorf("registry missing metric %q (got %v)", want, names)
		}
	}
}
