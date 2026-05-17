package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Architectures incuse advertises to GitHub. Jobs that don't specify
// an arch label fall back to the host's runtime arch.
const (
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"
)

// supportedArches is the closed set advertised on the scale set. Keep
// in sync with the cloud-init template (phase 4) — adding an arch here
// without a runner tarball for it gives a confusing 404 at boot.
var supportedArches = []string{ArchAMD64, ArchARM64}

// RunnerSpec is the resolved per-job VM shape. Produced by
// ResolveRunnerSpec from a job's RequestLabels and the operator's
// RunnerConfig. Drives the LaunchRequest the orchestrator hands the
// Incus wrapper.
type RunnerSpec struct {
	VCPUs    int
	MemoryMB int
	DiskGB   int
	Arch     string
	// DebugID is for logs: "vcpu=2/arm64".
	DebugID string
	// MatchedLabels lists the labels we keyed off (vcpu=N, arch=X).
	// Useful when chasing "why did this job get this size".
	MatchedLabels []string
}

// ValidRunnerLabels returns the labels incuse registers on the scale
// set. A job's runs-on must intersect this set for GitHub to route it
// to us.
//
// Composition: the operator-supplied BaseLabels (typically the scale-
// set name and any operator tag), one vcpu=<N> label per configured
// tier, and one arch=<X> label per supported arch. There is
// intentionally no type=container|vm label — MVP is VM-only and a
// stale type=container label would just yield a confusing failure.
func ValidRunnerLabels(base []string, vcpuTiers []int) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(l string) {
		key := strings.ToLower(l)
		if l == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, l)
	}
	for _, l := range base {
		add(l)
	}
	for _, n := range vcpuTiers {
		add(fmt.Sprintf("vcpu=%d", n))
	}
	for _, a := range supportedArches {
		add("arch=" + a)
	}
	return out
}

// ResolveRunnerSpec turns a job's RequestLabels into a RunnerSpec. The
// rule set is intentionally tiny:
//
//   - vcpu=<N>: pick the smallest tier in cfg.VCPUTiers that is >= N.
//     If no qualifier is present, fall back to the smallest tier.
//   - arch=<x>: must be in supportedArches. If absent, fall back to
//     hostArch (the orchestrator's runtime.GOARCH).
//
// Conflicts (multiple vcpu=, multiple arch=, unknown arch, vcpu above
// the largest tier) return an error rather than guessing — the
// orchestrator surfaces these as a job-level fault and the reaper
// removes the runner registration so GitHub re-queues.
func ResolveRunnerSpec(cfg RunnerConfig, hostArch string, requestLabels []string) (RunnerSpec, error) {
	if len(cfg.VCPUTiers) == 0 {
		return RunnerSpec{}, errors.New("config has no vcpu tiers")
	}
	tiers := append([]int(nil), cfg.VCPUTiers...)
	sort.Ints(tiers)
	smallest := tiers[0]
	largest := tiers[len(tiers)-1]

	var (
		matched     []string
		vcpuRequest int
		arch        string
		archSeen    bool
	)
	for _, raw := range requestLabels {
		key, val, ok := splitLabel(raw)
		if !ok {
			continue
		}
		switch strings.ToLower(key) {
		case "vcpu":
			n, err := parsePositiveInt(val)
			if err != nil {
				return RunnerSpec{}, fmt.Errorf("invalid vcpu label %q: %w", raw, err)
			}
			if vcpuRequest != 0 && vcpuRequest != n {
				return RunnerSpec{}, fmt.Errorf("conflicting vcpu labels: %d and %d", vcpuRequest, n)
			}
			vcpuRequest = n
			matched = append(matched, raw)
		case "arch":
			a := strings.ToLower(val)
			if !archSupported(a) {
				return RunnerSpec{}, fmt.Errorf("unsupported arch label %q (supported: %s)", raw, strings.Join(supportedArches, ","))
			}
			if archSeen && a != arch {
				return RunnerSpec{}, fmt.Errorf("conflicting arch labels: %s and %s", arch, a)
			}
			arch = a
			archSeen = true
			matched = append(matched, raw)
		}
	}

	picked := smallest
	if vcpuRequest > 0 {
		if vcpuRequest > largest {
			return RunnerSpec{}, fmt.Errorf("vcpu=%d exceeds largest tier vcpu=%d", vcpuRequest, largest)
		}
		picked = smallestTierAtLeast(tiers, vcpuRequest)
	}

	if !archSeen {
		arch = strings.ToLower(hostArch)
		if !archSupported(arch) {
			return RunnerSpec{}, fmt.Errorf("host arch %q is not supported (need one of %s)", hostArch, strings.Join(supportedArches, ","))
		}
	}

	return RunnerSpec{
		VCPUs:         picked,
		MemoryMB:      picked * cfg.MemoryPerVCPUMiB,
		DiskGB:        cfg.RootDiskGiB,
		Arch:          arch,
		DebugID:       fmt.Sprintf("vcpu=%d/%s", picked, arch),
		MatchedLabels: matched,
	}, nil
}

func splitLabel(raw string) (key, val string, ok bool) {
	idx := strings.IndexByte(raw, '=')
	if idx <= 0 || idx == len(raw)-1 {
		return "", "", false
	}
	return raw[:idx], raw[idx+1:], true
}

func parsePositiveInt(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty value")
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a positive integer: %q", s)
		}
		n = n*10 + int(r-'0')
		if n > 1024 { // arbitrary upper bound — keeps a malicious label from overflowing
			return 0, fmt.Errorf("value too large: %q", s)
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("not a positive integer: %q", s)
	}
	return n, nil
}

func archSupported(a string) bool {
	for _, s := range supportedArches {
		if s == a {
			return true
		}
	}
	return false
}

func smallestTierAtLeast(sorted []int, want int) int {
	for _, n := range sorted {
		if n >= want {
			return n
		}
	}
	return sorted[len(sorted)-1]
}
