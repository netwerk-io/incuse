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
	// Release is the actions/runner version + download URL the VM
	// will install when Baked is false. Ignored when Baked is true.
	Release Release

	// JITConfig is the encoded JIT runner configuration from the
	// scaleset library — the same opaque blob that
	// `./run.sh --jitconfig <blob>` accepts on the runner side.
	JITConfig string

	// WorkFolder is the runner's _work directory (relative to
	// /opt/runner). Used only when Baked is false (the baked image
	// already has /opt/runner/_work pre-created).
	WorkFolder string

	// RunnerName is the name the runner registers with. Used for the
	// hostname inside the VM and in log lines so an operator can
	// correlate `incus list` output against runner names.
	RunnerName string

	// Baked switches the template to assume actions/runner, the
	// runner user, packages, and the systemd unit are pre-installed
	// on the image. cloud-init only drops the per-launch JIT and
	// starts the unit. Cuts pickup latency by ~70s on a 1-vCPU VM.
	Baked bool
}

// Validate reports the first missing field. Called by Render to fail
// loudly rather than emit a half-baked cloud-config that boots into
// a broken state.
func (s CloudInitSpec) Validate() error {
	if s.JITConfig == "" {
		return errors.New("jit_config is required")
	}
	if s.RunnerName == "" {
		return errors.New("runner_name is required")
	}
	if s.Baked {
		// Baked mode: Release/WorkFolder pre-installed in the image.
		return nil
	}
	switch {
	case s.Release.Version == "":
		return errors.New("release version is required")
	case s.Release.DownloadURL == "":
		return errors.New("release download_url is required")
	case s.WorkFolder == "":
		return errors.New("work_folder is required")
	}
	return nil
}

// Render produces the #cloud-config payload to hand to Incus as the
// `cloud-init.user-data` config key on the launched VM. Picks the
// vanilla or baked template based on spec.Baked.
func Render(spec CloudInitSpec) ([]byte, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	tmpl := cloudInitTemplate
	if spec.Baked {
		tmpl = cloudInitBakedTemplate
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, spec); err != nil {
		return nil, fmt.Errorf("rendering cloud-init template: %w", err)
	}
	return buf.Bytes(), nil
}

// cloudInitTemplate is the vanilla-image template — every VM does a
// full install of packages and the actions/runner tarball at first
// boot. Use this when runner.use_baked_image is false. Notes:
//
//   - JIT config goes into /etc/incuse/jit.env (mode 0600, owned by
//     root — systemd reads EnvironmentFile as PID 1 before dropping
//     to User=runner, so a root-owned file is both sufficient and
//     keeps cloud-init's write_files step independent of the
//     users-groups module ordering.
//
//   - The runner unit is Type=oneshot. After ExecStart returns, we
//     sleep briefly so the actions/runner process has time to flush
//     its final job-completion HTTP write to GitHub before the VM
//     dies. The poweroff itself uses the systemd `+` prefix so it
//     runs as root regardless of the unit's User=runner setting.
//
//   - Docker is mandatory: jobs that use docker actions assume it.
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
      ExecStopPost=+/bin/sleep 15
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

// cloudInitBakedTemplate is the minimal template used when
// runner.use_baked_image is true. The image has the runner user,
// /opt/runner/run.sh, packages, docker, and the
// /etc/systemd/system/incuse-runner.service unit already installed
// (see scripts/build-runner-image.sh). Cloud-init only drops the
// per-launch JIT and starts the unit.
var cloudInitBakedTemplate = template.Must(template.New("cloudinit-baked").Parse(`#cloud-config
hostname: {{.RunnerName}}
preserve_hostname: false

write_files:
  - path: /etc/incuse/jit.env
    permissions: "0600"
    content: |
      INCUSE_JIT={{.JITConfig}}

runcmd:
  - systemctl start incuse-runner.service
`))
