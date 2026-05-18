package runner

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// updateGolden flips on with `go test ./internal/runner -update`. Used
// when an intentional template change makes the golden file stale.
var updateGolden = flag.Bool("update", false, "rewrite golden files")

func sampleSpec() CloudInitSpec {
	return CloudInitSpec{
		Release: Release{
			Version:     "2.328.0",
			DownloadURL: "https://github.com/actions/runner/releases/download/v2.328.0/actions-runner-linux-x64-2.328.0.tar.gz",
		},
		JITConfig:  "ZmFrZS1qaXQtY29uZmlnLWJsb2I=",
		WorkFolder: "_work",
		RunnerName: "incuse-runner-abc123",
	}
}

func TestRender_GoldenBasic(t *testing.T) {
	got, err := Render(sampleSpec())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	goldenPath := filepath.Join("testdata", "cloudinit-basic.golden")

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("cloud-init output drifted from golden file. Diff:\n--- want\n%s\n--- got\n%s", want, got)
	}
}

func TestRender_ValidationFailures(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CloudInitSpec)
		want   string
	}{
		{"missing version", func(s *CloudInitSpec) { s.Release.Version = "" }, "version"},
		{"missing download url", func(s *CloudInitSpec) { s.Release.DownloadURL = "" }, "download_url"},
		{"missing jit", func(s *CloudInitSpec) { s.JITConfig = "" }, "jit_config"},
		{"missing work folder", func(s *CloudInitSpec) { s.WorkFolder = "" }, "work_folder"},
		{"missing runner name", func(s *CloudInitSpec) { s.RunnerName = "" }, "runner_name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := sampleSpec()
			tc.mutate(&s)
			_, err := Render(s)
			if err == nil {
				t.Fatalf("want error containing %q", tc.want)
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("error message: want contains %q, got %q", tc.want, err.Error())
			}
		})
	}
}

func TestRender_EmbedsCriticalDirectives(t *testing.T) {
	got, err := Render(sampleSpec())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	checks := []string{
		"#cloud-config",
		"hostname: incuse-runner-abc123",
		"docker.io",
		"INCUSE_JIT=ZmFrZS1qaXQtY29uZmlnLWJsb2I=",
		"ExecStopPost=+/sbin/poweroff",
		"https://github.com/actions/runner/releases/download/v2.328.0/actions-runner-linux-x64-2.328.0.tar.gz",
		"/opt/runner/_work",
	}
	for _, c := range checks {
		if !contains(string(got), c) {
			t.Errorf("rendered cloud-init missing %q", c)
		}
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

func TestRender_GoldenBaked(t *testing.T) {
	spec := CloudInitSpec{
		JITConfig:  "ZmFrZS1qaXQtY29uZmlnLWJsb2I=",
		RunnerName: "incuse-runner-abc123",
		Baked:      true,
	}
	got, err := Render(spec)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	goldenPath := filepath.Join("testdata", "cloudinit-baked.golden")
	if *updateGolden {
		if err := os.WriteFile(goldenPath, got, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("baked cloud-init drifted from golden:\n--- want\n%s\n--- got\n%s", want, got)
	}
}

func TestRender_BakedRequiresOnlyJITAndName(t *testing.T) {
	// Baked mode skips Release/WorkFolder validation.
	spec := CloudInitSpec{
		JITConfig:  "x",
		RunnerName: "n",
		Baked:      true,
	}
	if _, err := Render(spec); err != nil {
		t.Errorf("baked render with minimal fields should succeed; got %v", err)
	}
	// But still requires JITConfig + RunnerName.
	for _, tc := range []struct {
		name   string
		mutate func(*CloudInitSpec)
		want   string
	}{
		{"missing jit", func(s *CloudInitSpec) { s.JITConfig = "" }, "jit_config"},
		{"missing runner name", func(s *CloudInitSpec) { s.RunnerName = "" }, "runner_name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := CloudInitSpec{JITConfig: "x", RunnerName: "n", Baked: true}
			tc.mutate(&s)
			if _, err := Render(s); err == nil || !contains(err.Error(), tc.want) {
				t.Errorf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestRender_BakedDoesNotIncludeInstallSteps(t *testing.T) {
	got, err := Render(CloudInitSpec{
		JITConfig:  "x",
		RunnerName: "n",
		Baked:      true,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	body := string(got)
	// Heavyweight items that MUST NOT appear in baked mode.
	for _, forbidden := range []string{
		"package_update",
		"docker.io",
		"actions/runner/releases",
		"useradd",
		"systemctl enable docker",
	} {
		if contains(body, forbidden) {
			t.Errorf("baked cloud-init should not contain %q; body=%s", forbidden, body)
		}
	}
	// Minimal items that MUST appear.
	for _, required := range []string{
		"INCUSE_JIT=x",
		"systemctl start incuse-runner.service",
	} {
		if !contains(body, required) {
			t.Errorf("baked cloud-init missing %q", required)
		}
	}
}
