package config

import (
	"reflect"
	"runtime"
	"testing"
)

func defaultRunnerCfg() RunnerConfig {
	return RunnerConfig{
		VCPUTiers:        []int{1, 2, 4},
		MemoryPerVCPUMiB: 4096,
		RootDiskGiB:      40,
	}
}

func TestValidRunnerLabels_DedupesAndCovers(t *testing.T) {
	got := ValidRunnerLabels([]string{"incuse", "Incuse", ""}, []int{1, 2, 4})

	want := []string{
		"incuse",
		"vcpu=1", "vcpu=2", "vcpu=4",
		"arch=amd64", "arch=arm64",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ValidRunnerLabels mismatch:\n got=%v\nwant=%v", got, want)
	}
}

func TestResolveRunnerSpec_FallsBackToSmallestTier(t *testing.T) {
	spec, err := ResolveRunnerSpec(defaultRunnerCfg(), runtime.GOARCH, []string{"incuse"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if spec.VCPUs != 1 {
		t.Errorf("vcpus: want 1 (smallest tier), got %d", spec.VCPUs)
	}
	if spec.MemoryMB != 4096 {
		t.Errorf("memory: want 4096, got %d", spec.MemoryMB)
	}
}

func TestResolveRunnerSpec_PicksSmallestTierAtLeastN(t *testing.T) {
	spec, err := ResolveRunnerSpec(defaultRunnerCfg(), "amd64", []string{"vcpu=3"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if spec.VCPUs != 4 {
		t.Errorf("vcpus: want 4 (next tier above 3), got %d", spec.VCPUs)
	}
}

func TestResolveRunnerSpec_ExactTierMatches(t *testing.T) {
	spec, err := ResolveRunnerSpec(defaultRunnerCfg(), "amd64", []string{"vcpu=2", "arch=arm64"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if spec.VCPUs != 2 {
		t.Errorf("vcpus: want 2, got %d", spec.VCPUs)
	}
	if spec.Arch != "arm64" {
		t.Errorf("arch: want arm64, got %q", spec.Arch)
	}
	if spec.DebugID != "vcpu=2/arm64" {
		t.Errorf("debug id: want vcpu=2/arm64, got %q", spec.DebugID)
	}
}

func TestResolveRunnerSpec_ConflictsAreErrors(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   string
	}{
		{"conflicting vcpu", []string{"vcpu=2", "vcpu=4"}, "conflicting vcpu labels"},
		{"conflicting arch", []string{"arch=amd64", "arch=arm64"}, "conflicting arch labels"},
		{"unsupported arch", []string{"arch=riscv64"}, "unsupported arch label"},
		{"vcpu above largest tier", []string{"vcpu=16"}, "exceeds largest tier"},
		{"non-numeric vcpu", []string{"vcpu=big"}, "invalid vcpu label"},
		{"vcpu zero", []string{"vcpu=0"}, "invalid vcpu label"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveRunnerSpec(defaultRunnerCfg(), "amd64", tc.labels)
			if err == nil {
				t.Fatalf("want error, got nil")
			}
			if !contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %q", tc.want, err.Error())
			}
		})
	}
}

func TestResolveRunnerSpec_HostArchFallback(t *testing.T) {
	spec, err := ResolveRunnerSpec(defaultRunnerCfg(), "ARM64", []string{"vcpu=1"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if spec.Arch != "arm64" {
		t.Errorf("arch fallback: want arm64, got %q", spec.Arch)
	}
}

func TestResolveRunnerSpec_RejectsUnsupportedHostArch(t *testing.T) {
	_, err := ResolveRunnerSpec(defaultRunnerCfg(), "ppc64le", []string{"vcpu=1"})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !contains(err.Error(), "host arch") {
		t.Fatalf("want error mentioning host arch, got %q", err.Error())
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
