package runner

import (
	"bytes"
	"errors"
	"fmt"
	"text/template"
)

// CloudInitSpec is the per-launch input to the cloud-init template.
// Everything the renderer needs to produce a finished #cloud-config
// payload — the orchestrator builds this from config + the resolved
// release + the scaleset-minted JIT config.
type CloudInitSpec struct {
	// Release is the actions/runner version + download URL the VM will
	// install. Resolved by LatestResolver at orchestrator startup.
	Release Release

	// JITConfig is the encoded JIT runner configuration from the
	// scaleset library — the same opaque blob that
	// `./run.sh --jitconfig <blob>` accepts on the runner side.
	JITConfig string

	// WorkFolder is the runner's _work directory (relative to
	// /opt/runner). Comes from config.runner.work_folder.
	WorkFolder string

	// RunnerName is the name the runner registers with. Used for the
	// hostname inside the VM and in log lines so an operator can
	// correlate `incus list` output against runner names.
	RunnerName string
}

// Validate reports the first missing field. Called by Render to fail
// loudly rather than emit a half-baked cloud-config that boots into a
// broken state.
func (s CloudInitSpec) Validate() error {
	switch {
	case s.Release.Version == "":
		return errors.New("release version is required")
	case s.Release.DownloadURL == "":
		return errors.New("release download_url is required")
	case s.JITConfig == "":
		return errors.New("jit_config is required")
	case s.WorkFolder == "":
		return errors.New("work_folder is required")
	case s.RunnerName == "":
		return errors.New("runner_name is required")
	}
	return nil
}

// Render produces the #cloud-config payload to hand to Incus as the
// `cloud-init.user-data` config key on the launched VM.
func Render(spec CloudInitSpec) ([]byte, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := cloudInitTemplate.Execute(&buf, spec); err != nil {
		return nil, fmt.Errorf("rendering cloud-init template: %w", err)
	}
	return buf.Bytes(), nil
}

// cloudInitTemplate is the single source of truth for what runs at
// first-boot. Notes on the design:
//
//   - The runner tarball is downloaded from GitHub directly inside the
//     VM. Trust comes from HTTPS to github.com. Sha256 is intentionally
//     not pinned because we always-track the latest release; pinning
//     would require fetching a separate sha256 manifest. Follow-up if
//     supply-chain hardening becomes a priority.
//
//   - JIT config goes into /etc/incuse/jit.env (mode 0600, owned by
//     root — systemd reads EnvironmentFile as PID 1 before dropping
//     to User=runner, so a root-owned file is both sufficient and
//     keeps cloud-init's write_files step independent of the
//     users-groups module ordering.
//
//     runner user) and the systemd unit reads it via EnvironmentFile.
//     Keeps it off the kernel command line and out of `ps`.
//
//   - The runner unit is Type=oneshot. ExecStopPost calls poweroff
//     with the systemd `+` prefix so the command runs as root
//     regardless of the unit's User=runner setting; without the
//     prefix, /sbin/poweroff fails with "Interactive authentication
//     required" and the VM survives the runner exit, defeating the
//     ephemeral cleanup path.
//     When run.sh exits (job done, or job-cancelled), systemd fires
//     poweroff, the VM stops, the orchestrator sees the stopped state
//     and deletes the instance.
//
//   - Docker is mandatory: jobs that use docker actions assume it; we
//     run on a VM precisely so docker-in-VM works without nesting
//     headaches.
var cloudInitTemplate = template.Must(template.New("cloudinit").Parse(`#cloud-config
hostname: {{.RunnerName}}
preserve_hostname: false

users:
  - name: runner
    groups: [sudo, docker]
    shell: /bin/bash
    sudo: "ALL=(ALL) NOPASSWD:ALL"
    lock_passwd: true

package_update: true
package_upgrade: false
packages:
  - curl
  - tar
  - git
  - jq
  - ca-certificates
  - docker.io

write_files:
  - path: /etc/incuse/jit.env
    permissions: "0600"
    content: |
      INCUSE_JIT={{.JITConfig}}
  - path: /etc/systemd/system/incuse-runner.service
    permissions: "0644"
    content: |
      [Unit]
      Description=GitHub Actions runner (one-shot)
      After=network-online.target docker.service
      Wants=network-online.target docker.service

      [Service]
      Type=oneshot
      User=runner
      Group=runner
      WorkingDirectory=/opt/runner
      EnvironmentFile=/etc/incuse/jit.env
      ExecStart=/opt/runner/run.sh --jitconfig ${INCUSE_JIT}
      ExecStopPost=+/sbin/poweroff
      RemainAfterExit=no
      StandardOutput=journal
      StandardError=journal

      [Install]
      WantedBy=multi-user.target

runcmd:
  - install -d -o runner -g runner -m 0755 /opt/runner /opt/runner/{{.WorkFolder}}
  - curl -fsSL "{{.Release.DownloadURL}}" -o /tmp/runner.tgz
  - tar -xzf /tmp/runner.tgz -C /opt/runner
  - chown -R runner:runner /opt/runner
  - rm -f /tmp/runner.tgz
  - systemctl enable docker.service
  - systemctl start docker.service
  - systemctl daemon-reload
  - systemctl enable --now incuse-runner.service
`))
