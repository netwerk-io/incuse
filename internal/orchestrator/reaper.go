package orchestrator

import (
	"context"
	"time"
)

// reapOnce is one sweep. Cheap to call: O(tracked) + O(remote
// instances in project). Three responsibilities:
//
//  1. Registration timeout: instance launched but the runner never
//     registered (cloud-init broken, image broken, network broken).
//     Catches: no JobStarted, no JobCompleted, just silence.
//
//  2. Max-job-duration: instance is running but JobCompleted callback
//     was missed (broker hiccup, listener restart). Defence-in-depth.
//
//  3. Drift sweep: any incuse-managed instance the local tracker has
//     no record of is an orphan from a previous orchestrator process.
//     Recover by deleting it. Covers crash-recovery on systemd
//     restart.
func (o *Orchestrator) reapOnce(ctx context.Context) {
	now := o.cfg.Now()
	tracked := o.tracker.snapshot()

	for _, inst := range tracked {
		select {
		case <-ctx.Done():
			return
		default:
		}
		o.reapTracked(ctx, inst, now)
	}

	o.driftSweep(ctx)
}

// reapTracked applies the timeout rules for one in-tracker instance.
func (o *Orchestrator) reapTracked(ctx context.Context, inst trackedInstance, now time.Time) {
	switch {
	case inst.RunnerStartedAt.IsZero():
		// Pre-registration — bound by registration_timeout.
		if now.Sub(inst.LaunchedAt) > o.cfg.RunnerCfg.RegistrationTimeout {
			o.cfg.Logger.Warn("reaping runner that never registered",
				"runner_name", inst.RunnerName,
				"runner_request_id", inst.JobID,
				"age", now.Sub(inst.LaunchedAt),
			)
			o.terminateInstance(ctx, inst.RunnerName, "registration timeout")
			o.removeRunnerByID(ctx, inst.RunnerID, inst.RunnerName, "registration timeout")
		}
	default:
		// Post-registration — bound by max_job_duration. JobCompleted
		// would normally have removed the instance from the tracker
		// already, so anything still here past the limit means we
		// missed the callback.
		if now.Sub(inst.RunnerStartedAt) > o.cfg.RunnerCfg.MaxJobDuration {
			o.cfg.Logger.Warn("reaping runner past max_job_duration",
				"runner_name", inst.RunnerName,
				"runner_request_id", inst.JobID,
				"running_for", now.Sub(inst.RunnerStartedAt),
			)
			o.terminateInstance(ctx, inst.RunnerName, "max job duration")
			o.removeRunnerByID(ctx, inst.RunnerID, inst.RunnerName, "max job duration")
		}
	}
}

// driftSweep enumerates every instance in the incuse project, filters
// to ones tagged user.incuse.managed=true, and deletes any whose name
// is not in the local tracker. This is how we recover from orchestrator
// crashes — without it, restart would orphan every previously-launched
// instance.
func (o *Orchestrator) driftSweep(ctx context.Context) {
	remote, err := o.cfg.IncusClient.List(ctx, o.cfg.IncusCfg.Project)
	if err != nil {
		o.cfg.Logger.Warn("drift sweep list failed", "error", err)
		return
	}
	known := o.tracker.names()
	for _, inst := range remote {
		if inst.Config[metaManaged] != "true" {
			continue
		}
		if _, tracked := known[inst.Name]; tracked {
			continue
		}
		o.cfg.Logger.Warn("reaping orphan instance",
			"runner_name", inst.Name,
			"status", inst.Status,
		)
		// We don't know the GitHub runner ID for orphans (we lost the
		// in-memory state on restart). The runner registration on
		// GitHub will eventually expire on its own; the cost is a few
		// stale "Idle" entries until then. The Incus instance is the
		// expensive resource so that's the priority.
		if err := o.cfg.IncusClient.Stop(ctx, inst.Name); err != nil {
			o.cfg.Logger.Warn("orphan stop failed", "runner_name", inst.Name, "error", err)
		}
		if err := o.cfg.IncusClient.Delete(ctx, inst.Name); err != nil {
			o.cfg.Logger.Warn("orphan delete failed", "runner_name", inst.Name, "error", err)
		}
	}
}
