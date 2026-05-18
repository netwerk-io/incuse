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
// instances belong to us.
//
// We deliberately don't store per-job metadata (job_id,
// workflow_run_id, etc.) on the Incus instance config: with the
// upstream Scaler pattern, runners are minted as idle members of a
// pool and only assigned to a specific job by GitHub's broker after
// the VM is up. The runner-name plus scaleSetID is enough for ops
// debugging via `incus list -c name,config:user.incuse.runner_name`.
const (
	metaManaged    = "user.incuse.managed"
	metaRunnerName = "user.incuse.runner_name"
	metaScaleSetID = "user.incuse.scale_set_id"
	metaMintedAt   = "user.incuse.minted_at"
)

// launchInputs bundles the per-launch context for buildLaunchRequest.
type launchInputs struct {
	runnerName  string
	spec        config.RunnerSpec
	incusCfg    config.IncusConfig
	runnerCfg   config.RunnerConfig
	cloudInit   []byte
	scaleSetID  int
	mintedAt    time.Time
	description string
}

// buildLaunchRequest assembles the incus.LaunchRequest from a
// resolved runner spec and the rendered cloud-init payload. Sets
// Ephemeral=true so a stuck instance that never powers itself off is
// still cleaned up by Incus on the next stop.
func buildLaunchRequest(in launchInputs) incus.LaunchRequest {
	cfgMap := map[string]string{
		"limits.cpu":           strconv.Itoa(in.spec.VCPUs),
		"limits.memory":        fmt.Sprintf("%dMiB", in.spec.MemoryMB),
		"cloud-init.user-data": string(in.cloudInit),
		metaManaged:            "true",
		metaRunnerName:         in.runnerName,
		metaScaleSetID:         strconv.Itoa(in.scaleSetID),
		metaMintedAt:           in.mintedAt.UTC().Format(time.RFC3339),
	}

	instanceType := incus.InstanceTypeVM
	if in.runnerCfg.InstanceType == config.InstanceTypeContainer {
		instanceType = incus.InstanceTypeContainer
		// security.nesting lets the container run nested user
		// namespaces, which Docker (and most container runtimes
		// inside the runner) need. The two syscall intercepts are
		// the standard "docker-in-LXC" pair documented by Incus.
		cfgMap["security.nesting"] = "true"
		cfgMap["security.syscalls.intercept.mknod"] = "true"
		cfgMap["security.syscalls.intercept.setxattr"] = "true"
		if in.runnerCfg.Privileged {
			// Nuclear option: the container runs as host root.
			// Required for some kernel-level test harnesses but
			// loses the isolation that made containers attractive.
			cfgMap["security.privileged"] = "true"
		}
	}

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
	switch {
	case imageServer == "":
		// Local image alias: leave Protocol blank so Incus does a
		// local lookup instead of trying simplestreams against an
		// empty server URL.
		imageProtocol = ""
	case imageProtocol == "":
		imageProtocol = "simplestreams"
	}

	return incus.LaunchRequest{
		Name:        in.runnerName,
		Type:        instanceType,
		Profiles:    []string{in.incusCfg.DefaultProfile},
		Image:       incus.ImageSource{Server: imageServer, Protocol: imageProtocol, Alias: in.runnerCfg.ImageAlias},
		Config:      cfgMap,
		Devices:     devices,
		Ephemeral:   true,
		Description: in.description,
	}
}
