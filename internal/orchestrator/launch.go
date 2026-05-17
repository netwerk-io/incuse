package orchestrator

import (
	"fmt"
	"strconv"
	"time"

	"github.com/netwerk-io/incuse/internal/config"
	"github.com/netwerk-io/incuse/internal/incus"
)

// metadata-key prefix for incuse-managed instances. The reaper's
// drift sweep keys off `user.incuse.managed=true` to identify which
// instances belong to us — so it's safe to share the project with
// other tooling, as long as the other tooling doesn't reuse this
// prefix.
const (
	metaManaged         = "user.incuse.managed"
	metaRunnerName      = "user.incuse.runner_name"
	metaJobID           = "user.incuse.job_id"
	metaWorkflowRunID   = "user.incuse.workflow_run_id"
	metaRunnerRequestID = "user.incuse.runner_request_id"
	metaScaleSetID      = "user.incuse.scale_set_id"
	metaMintedAt        = "user.incuse.minted_at"
)

// launchInputs bundles the per-launch context for buildLaunchRequest.
// Keeping it in one struct keeps the call site readable.
type launchInputs struct {
	runnerName      string
	spec            config.RunnerSpec
	incusCfg        config.IncusConfig
	runnerCfg       config.RunnerConfig
	cloudInit       []byte
	jobID           string
	workflowRunID   int64
	runnerRequestID int64
	scaleSetID      int
	mintedAt        time.Time
	description     string
}

// buildLaunchRequest assembles the incus.LaunchRequest from a resolved
// runner spec and the rendered cloud-init payload. Sets Ephemeral=true
// so a stuck VM that never powers itself off is still cleaned up by
// Incus on the next stop.
func buildLaunchRequest(in launchInputs) incus.LaunchRequest {
	cfgMap := map[string]string{
		// Per-launch resource limits override anything the profile
		// supplies. Profile is for shared shape (network, secureboot),
		// per-launch is for the size dial.
		"limits.cpu":           strconv.Itoa(in.spec.VCPUs),
		"limits.memory":        fmt.Sprintf("%dMiB", in.spec.MemoryMB),
		"cloud-init.user-data": string(in.cloudInit),
		// Tag every instance so the reaper's drift sweep can spot
		// orphans, and so an operator can grep for ours.
		metaManaged:         "true",
		metaRunnerName:      in.runnerName,
		metaJobID:           in.jobID,
		metaWorkflowRunID:   strconv.FormatInt(in.workflowRunID, 10),
		metaRunnerRequestID: strconv.FormatInt(in.runnerRequestID, 10),
		metaScaleSetID:      strconv.Itoa(in.scaleSetID),
		metaMintedAt:        in.mintedAt.UTC().Format(time.RFC3339),
	}

	// Override the profile's root device size only — leave pool, path,
	// and any other fields the profile sets alone. Incus merges
	// per-instance device entries on top of profile entries by key.
	devices := map[string]map[string]string{
		"root": {
			"type": "disk",
			"path": "/",
			"pool": "default",
			"size": fmt.Sprintf("%dGiB", in.spec.DiskGB),
		},
	}

	imageServer := in.runnerCfg.ImageServer
	imageProtocol := in.runnerCfg.ImageProtocol
	if imageProtocol == "" {
		imageProtocol = "simplestreams"
	}

	return incus.LaunchRequest{
		Name:        in.runnerName,
		Type:        incus.InstanceTypeVM,
		Profiles:    []string{in.incusCfg.DefaultProfile},
		Image:       incus.ImageSource{Server: imageServer, Protocol: imageProtocol, Alias: in.runnerCfg.ImageAlias},
		Config:      cfgMap,
		Devices:     devices,
		Ephemeral:   true,
		Description: in.description,
	}
}
