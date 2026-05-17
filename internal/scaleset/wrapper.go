package scaleset

import (
	"context"
	"log/slog"

	ssapi "github.com/actions/scaleset"
	sslistener "github.com/actions/scaleset/listener"
)

// mintingClient decorates an upstream listener.Client. GetMessage
// dispatches every JobAssigned event to the JITMinter before passing
// the message back to the upstream listener unchanged. The listener
// itself does nothing with JobAssigned — it only pipes JobStarted /
// JobCompleted to the Scaler and drives capacity from the message
// statistics — so hooking the mint here is the ordered, synchronous
// path with no extra goroutines.
//
// Mint errors are logged and swallowed. The alternative would be to
// fail GetMessage, but the upstream code has no retry path that would
// re-deliver the same JobAssigned — so surfacing the error there just
// kills the listener. GitHub re-queues unacquired jobs on its own; a
// failed mint surfaces as "the JIT config never arrived at the
// instance", which the orchestrator's reaper observes as a runner
// that never registered and reaps after the registration timeout.
type mintingClient struct {
	inner  sslistener.Client
	minter JITMinter
	logger *slog.Logger
}

var _ sslistener.Client = (*mintingClient)(nil)

func (m *mintingClient) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*ssapi.RunnerScaleSetMessage, error) {
	msg, err := m.inner.GetMessage(ctx, lastMessageID, maxCapacity)
	if err != nil || msg == nil {
		return msg, err
	}

	if s := msg.Statistics; s != nil {
		m.logger.Info("scale set message received",
			"max_capacity_sent", maxCapacity,
			"total_available", s.TotalAvailableJobs,
			"total_acquired", s.TotalAcquiredJobs,
			"total_assigned", s.TotalAssignedJobs,
			"total_running", s.TotalRunningJobs,
			"job_available_count", len(msg.JobAvailableMessages),
			"job_assigned_count", len(msg.JobAssignedMessages),
		)
	}
	for _, ja := range msg.JobAvailableMessages {
		if ja != nil {
			m.logger.Info("job available", "runner_request_id", ja.RunnerRequestID)
		}
	}
	for _, ja := range msg.JobAssignedMessages {
		if ja == nil {
			continue
		}
		m.logger.Info("job assigned",
			"runner_request_id", ja.RunnerRequestID,
			"request_labels", ja.RequestLabels,
		)
		if mintErr := m.minter.Mint(ctx, ja); mintErr != nil {
			m.logger.Error("jit mint failed",
				"runner_request_id", ja.RunnerRequestID,
				"request_labels", ja.RequestLabels,
				"error", mintErr,
			)
		}
	}
	return msg, nil
}

func (m *mintingClient) DeleteMessage(ctx context.Context, messageID int) error {
	return m.inner.DeleteMessage(ctx, messageID)
}

func (m *mintingClient) AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error) {
	m.logger.Info("acquiring jobs", "request_ids", requestIDs)
	acquired, err := m.inner.AcquireJobs(ctx, requestIDs)
	m.logger.Info("jobs acquired", "requested", len(requestIDs), "acquired", len(acquired))
	return acquired, err
}

func (m *mintingClient) Session() ssapi.RunnerScaleSetSession {
	return m.inner.Session()
}
