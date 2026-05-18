// Package config carries the on-disk YAML schema, Load+validate plumbing,
// and the label-resolver that maps a GitHub job's RequestLabels onto a
// concrete RunnerSpec (vcpu count, arch, memory, disk).
//
// Schema lives in /etc/incuse/config.yaml on a deployed host. Defaults
// match the plan's MVP target — Unix-socket Incus access, ubuntu/24.04
// VMs, the runner-group named "Default" — so a minimal config only has
// to set github.config_url, github.auth, and scale_set.name.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

// Config is the top-level schema. Loaded with Load.
type Config struct {
	GitHub        GitHubConfig        `yaml:"github"`
	ScaleSet      ScaleSetConfig      `yaml:"scale_set"`
	Incus         IncusConfig         `yaml:"incus"`
	Runner        RunnerConfig        `yaml:"runner"`
	Observability ObservabilityConfig `yaml:"observability"`
}

// GitHubConfig points incuse at one GitHub org/repo/enterprise scope and
// names the auth strategy.
type GitHubConfig struct {
	// ConfigURL is the per-org / per-repo / per-enterprise URL the
	// scaleset client expects, e.g. https://github.com/netwerk-io.
	ConfigURL string     `yaml:"config_url"`
	Auth      AuthConfig `yaml:"auth"`
}

// AuthConfig selects between PAT and GitHub App authentication. Both
// code paths exist; the operator picks one in YAML.
type AuthConfig struct {
	// Mode is "pat" or "app".
	Mode    string         `yaml:"mode"`
	PATFile string         `yaml:"pat_file"`
	App     AppCredentials `yaml:"app"`
}

// AppCredentials is the GitHub App input. PrivateKeyFile is read from
// disk at startup and the contents fed to the upstream client.
type AppCredentials struct {
	ClientID       string `yaml:"client_id"`
	PrivateKeyFile string `yaml:"private_key_file"`
	InstallationID int64  `yaml:"installation_id"`
}

// ScaleSetConfig describes the single GitHub Runner Scale Set incuse
// owns. Labels are reconciled at bootstrap from BaseLabels +
// ValidRunnerLabels(VCPUTiers).
type ScaleSetConfig struct {
	Name        string   `yaml:"name"`
	RunnerGroup string   `yaml:"runner_group"`
	BaseLabels  []string `yaml:"base_labels"`
	MaxRunners  int      `yaml:"max_runners"`
}

// IncusConfig is the input to internal/incus.Connect. URL and
// SocketPath are mutually exclusive — empty URL selects Unix socket.
type IncusConfig struct {
	URL                string `yaml:"url"`
	SocketPath         string `yaml:"socket_path"`
	CertFile           string `yaml:"cert_file"`
	KeyFile            string `yaml:"key_file"`
	ServerCertFile     string `yaml:"server_cert_file"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	Project            string `yaml:"project"`
	DefaultProfile     string `yaml:"default_profile"`
}

// RunnerConfig describes the per-instance VM shape and the
// actions/runner tarball the cloud-init template installs.
type RunnerConfig struct {
	ImageServer         string        `yaml:"image_server"`
	ImageProtocol       string        `yaml:"image_protocol"`
	ImageAlias          string        `yaml:"image_alias"`
	RunnerVersion       string        `yaml:"runner_version"`
	RunnerSHA256        string        `yaml:"runner_sha256"`
	WorkFolder          string        `yaml:"work_folder"`
	VCPUTiers           []int         `yaml:"vcpu_tiers"`
	MemoryPerVCPUMiB    int           `yaml:"memory_per_vcpu_mib"`
	RootDiskGiB         int           `yaml:"root_disk_gib"`
	RegistrationTimeout time.Duration `yaml:"registration_timeout"`
	MaxJobDuration      time.Duration `yaml:"max_job_duration"`

	// UseBakedImage tells the orchestrator to use the minimal
	// cloud-init template that assumes actions/runner, the runner
	// user, packages, and the systemd unit are pre-installed on the
	// image. Build the image with scripts/build-runner-image.sh.
	UseBakedImage bool `yaml:"use_baked_image"`
}

// Auth modes.
const (
	AuthModePAT = "pat"
	AuthModeApp = "app"
)

// ObservabilityConfig turns on the HTTP server that exposes /healthz,
// /readyz, and /metrics. ListenAddr empty disables the server
// entirely — incuse runs perfectly fine without metrics, the unit
// just gets harder to monitor.
type ObservabilityConfig struct {
	// ListenAddr is a Go net.Listen address (e.g. ":9090",
	// "127.0.0.1:9090"). Empty disables the server.
	ListenAddr string `yaml:"listen_addr"`
}

// Load reads the YAML file at path, populates defaults, validates, and
// returns the result. ENOENT is reported as a typed error so callers
// can distinguish "no config" from "broken config".
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("config: parse %q: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes raw YAML, applies defaults, and validates. Exposed so
// tests don't have to round-trip through the filesystem.
func Parse(raw []byte) (*Config, error) {
	cfg := &Config{}
	if err := yaml.UnmarshalStrict(raw, cfg); err != nil {
		return nil, fmt.Errorf("decode yaml: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.ScaleSet.RunnerGroup == "" {
		c.ScaleSet.RunnerGroup = "Default"
	}
	if c.Incus.SocketPath == "" && c.Incus.URL == "" {
		c.Incus.SocketPath = "/var/lib/incus/unix.socket"
	}
	if c.Incus.Project == "" {
		c.Incus.Project = "incuse"
	}
	if c.Incus.DefaultProfile == "" {
		c.Incus.DefaultProfile = "incuse-runner"
	}
	if c.Runner.UseBakedImage {
		// Baked-image flow: alias resolves locally on the Incus daemon.
		// Leave ImageServer/Protocol empty so the daemon doesn't try a
		// remote simplestreams lookup.
		if c.Runner.ImageAlias == "" {
			c.Runner.ImageAlias = "incuse-runner"
		}
	} else {
		if c.Runner.ImageServer == "" {
			c.Runner.ImageServer = "https://images.linuxcontainers.org"
		}
		if c.Runner.ImageProtocol == "" {
			c.Runner.ImageProtocol = "simplestreams"
		}
		if c.Runner.ImageAlias == "" {
			c.Runner.ImageAlias = "ubuntu/24.04/cloud"
		}
	}
	if c.Runner.WorkFolder == "" {
		c.Runner.WorkFolder = "_work"
	}
	if len(c.Runner.VCPUTiers) == 0 {
		c.Runner.VCPUTiers = []int{1, 2, 4}
	}
	if c.Runner.MemoryPerVCPUMiB == 0 {
		c.Runner.MemoryPerVCPUMiB = 4096
	}
	if c.Runner.RootDiskGiB == 0 {
		c.Runner.RootDiskGiB = 40
	}
	if c.Runner.RegistrationTimeout == 0 {
		c.Runner.RegistrationTimeout = 10 * time.Minute
	}
	if c.Runner.MaxJobDuration == 0 {
		c.Runner.MaxJobDuration = 6 * time.Hour
	}
}

// Validate is exported so callers can re-check after programmatic
// mutation (the systemd `--validate` preflight, primarily).
func (c *Config) Validate() error {
	var errs []string
	check := func(cond bool, msg string) {
		if !cond {
			errs = append(errs, msg)
		}
	}

	check(c.GitHub.ConfigURL != "", "github.config_url is required")
	if c.GitHub.ConfigURL != "" {
		if u, err := url.Parse(c.GitHub.ConfigURL); err != nil || u.Scheme == "" || u.Host == "" {
			errs = append(errs, fmt.Sprintf("github.config_url %q is not a valid URL", c.GitHub.ConfigURL))
		}
	}

	switch c.GitHub.Auth.Mode {
	case AuthModePAT:
		check(c.GitHub.Auth.PATFile != "", "github.auth.pat_file is required when mode=pat")
	case AuthModeApp:
		check(c.GitHub.Auth.App.ClientID != "", "github.auth.app.client_id is required when mode=app")
		check(c.GitHub.Auth.App.PrivateKeyFile != "", "github.auth.app.private_key_file is required when mode=app")
		check(c.GitHub.Auth.App.InstallationID != 0, "github.auth.app.installation_id is required when mode=app")
	case "":
		errs = append(errs, "github.auth.mode is required (pat|app)")
	default:
		errs = append(errs, fmt.Sprintf("github.auth.mode %q must be pat or app", c.GitHub.Auth.Mode))
	}

	check(c.ScaleSet.Name != "", "scale_set.name is required")
	check(c.ScaleSet.MaxRunners > 0, "scale_set.max_runners must be > 0")

	if c.Incus.URL != "" {
		check(c.Incus.CertFile != "", "incus.cert_file is required when incus.url is set")
		check(c.Incus.KeyFile != "", "incus.key_file is required when incus.url is set")
	}

	check(len(c.Runner.VCPUTiers) > 0, "runner.vcpu_tiers must contain at least one tier")
	for i, n := range c.Runner.VCPUTiers {
		if n <= 0 {
			errs = append(errs, fmt.Sprintf("runner.vcpu_tiers[%d] (%d) must be > 0", i, n))
		}
	}
	check(c.Runner.MemoryPerVCPUMiB > 0, "runner.memory_per_vcpu_mib must be > 0")
	check(c.Runner.RootDiskGiB > 0, "runner.root_disk_gib must be > 0")
	check(c.Runner.RunnerVersion != "", "runner.runner_version is required (pin the actions/runner release)")
	check(c.Runner.RegistrationTimeout > 0, "runner.registration_timeout must be > 0")
	check(c.Runner.MaxJobDuration > 0, "runner.max_job_duration must be > 0")

	if len(errs) > 0 {
		return errors.New("config invalid:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}
