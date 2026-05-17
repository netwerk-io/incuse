package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validYAML() string {
	return `
github:
  config_url: https://github.com/netwerk-io
  auth:
    mode: pat
    pat_file: /etc/incuse/github.pat
scale_set:
  name: incuse
  runner_group: Default
  base_labels: ["incuse"]
  max_runners: 4
incus:
  socket_path: /var/lib/incus/unix.socket
  project: incuse
  default_profile: incuse-runner
runner:
  runner_version: 2.328.0
  vcpu_tiers: [1, 2, 4]
`
}

func TestParse_AppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(validYAML()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if cfg.ScaleSet.RunnerGroup != "Default" {
		t.Errorf("runner group default: got %q", cfg.ScaleSet.RunnerGroup)
	}
	if cfg.Incus.Project != "incuse" {
		t.Errorf("incus project default: got %q", cfg.Incus.Project)
	}
	if cfg.Runner.ImageAlias != "ubuntu/24.04/cloud" {
		t.Errorf("image alias default: got %q", cfg.Runner.ImageAlias)
	}
	if cfg.Runner.MemoryPerVCPUMiB != 4096 {
		t.Errorf("memory default: got %d", cfg.Runner.MemoryPerVCPUMiB)
	}
	if cfg.Runner.RegistrationTimeout != 10*time.Minute {
		t.Errorf("registration timeout default: got %v", cfg.Runner.RegistrationTimeout)
	}
	if cfg.Runner.MaxJobDuration != 6*time.Hour {
		t.Errorf("max job duration default: got %v", cfg.Runner.MaxJobDuration)
	}
}

func TestParse_RejectsUnknownKeys(t *testing.T) {
	bad := strings.Replace(validYAML(), "max_runners: 4", "max_runers: 4", 1) // typo
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("want strict-mode error on unknown key, got nil")
	}
}

func TestValidate_RequiresAuthConfig(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			"missing config_url",
			strings.Replace(validYAML(), "config_url: https://github.com/netwerk-io", "config_url: \"\"", 1),
			"github.config_url is required",
		},
		{
			"unknown auth mode",
			strings.Replace(validYAML(), "mode: pat", "mode: oauth", 1),
			"github.auth.mode \"oauth\" must be pat or app",
		},
		{
			"missing pat file",
			strings.Replace(validYAML(), "pat_file: /etc/incuse/github.pat", "pat_file: \"\"", 1),
			"github.auth.pat_file is required when mode=pat",
		},
		{
			"app mode missing all fields",
			strings.NewReplacer(
				"mode: pat", "mode: app",
				"pat_file: /etc/incuse/github.pat", "",
			).Replace(validYAML()),
			"github.auth.app.client_id is required",
		},
		{
			"max_runners zero",
			strings.Replace(validYAML(), "max_runners: 4", "max_runners: 0", 1),
			"scale_set.max_runners must be > 0",
		},
		{
			"https without cert",
			strings.Replace(validYAML(),
				"socket_path: /var/lib/incus/unix.socket",
				"url: https://incus.example.com:8443", 1),
			"incus.cert_file is required when incus.url is set",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("want validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLoad_FromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(validYAML()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ScaleSet.Name != "incuse" {
		t.Fatalf("name: got %q", cfg.ScaleSet.Name)
	}
}
