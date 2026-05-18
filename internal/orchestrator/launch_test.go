package orchestrator

import (
	"testing"
	"time"

	"github.com/netwerk-io/incuse/internal/config"
	"github.com/netwerk-io/incuse/internal/incus"
)

func baseInputs() launchInputs {
	return launchInputs{
		runnerName: "incuse-test-abc",
		spec:       config.RunnerSpec{VCPUs: 2, MemoryMB: 8192, DiskGB: 40},
		incusCfg:   config.IncusConfig{Project: "incuse", DefaultProfile: "incuse-runner"},
		runnerCfg: config.RunnerConfig{
			ImageAlias: "ubuntu/24.04/cloud",
		},
		cloudInit:  []byte("#cloud-config\n"),
		scaleSetID: 1,
		mintedAt:   time.Unix(0, 0),
	}
}

func TestBuildLaunchRequest_VMDefault(t *testing.T) {
	in := baseInputs()
	req := buildLaunchRequest(in)

	if req.Type != incus.InstanceTypeVM {
		t.Fatalf("Type = %q, want %q", req.Type, incus.InstanceTypeVM)
	}
	for _, k := range []string{"security.nesting", "security.privileged", "security.syscalls.intercept.mknod"} {
		if _, ok := req.Config[k]; ok {
			t.Errorf("VM launch should not set %q (got %q)", k, req.Config[k])
		}
	}
}

func TestBuildLaunchRequest_Container(t *testing.T) {
	in := baseInputs()
	in.runnerCfg.InstanceType = config.InstanceTypeContainer
	req := buildLaunchRequest(in)

	if req.Type != incus.InstanceTypeContainer {
		t.Fatalf("Type = %q, want %q", req.Type, incus.InstanceTypeContainer)
	}
	want := map[string]string{
		"security.nesting":                     "true",
		"security.syscalls.intercept.mknod":    "true",
		"security.syscalls.intercept.setxattr": "true",
	}
	for k, v := range want {
		if got := req.Config[k]; got != v {
			t.Errorf("Config[%q] = %q, want %q", k, got, v)
		}
	}
	if _, ok := req.Config["security.privileged"]; ok {
		t.Errorf("non-privileged container should not set security.privileged")
	}
}

func TestBuildLaunchRequest_ContainerPrivileged(t *testing.T) {
	in := baseInputs()
	in.runnerCfg.InstanceType = config.InstanceTypeContainer
	in.runnerCfg.Privileged = true
	req := buildLaunchRequest(in)

	if got := req.Config["security.privileged"]; got != "true" {
		t.Fatalf("security.privileged = %q, want \"true\"", got)
	}
	// Non-privileged-container flags should still be set: privileged
	// implies nesting+intercepts plus host-root, not exclusion.
	if got := req.Config["security.nesting"]; got != "true" {
		t.Errorf("security.nesting = %q, want \"true\"", got)
	}
}
