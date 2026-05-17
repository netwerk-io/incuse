// Package incus is a thin wrapper around github.com/lxc/incus/v6/client
// that exposes only the lifecycle surface incuse needs (Launch, Stop,
// Delete, Get, List). Keeping the surface small means the orchestrator
// can be tested against a fake without depending on upstream types.
package incus

import "context"

// InstanceType is a typed enum so callers cannot pass an arbitrary
// string. Mirrors api.InstanceTypeContainer / api.InstanceTypeVM
// without leaking that import outside this package.
type InstanceType string

const (
	// InstanceTypeVM launches a virtual machine. The MVP target.
	InstanceTypeVM InstanceType = "virtual-machine"
	// InstanceTypeContainer launches a system container. Reserved for a
	// future phase; not exercised by the orchestrator yet.
	InstanceTypeContainer InstanceType = "container"
)

// ImageSource describes where to pull the base image from. Use the
// public images.linuxcontainers.org server with Protocol="simplestreams"
// for stock distros — Incus pulls and caches the image itself.
type ImageSource struct {
	Server   string // e.g. https://images.linuxcontainers.org
	Protocol string // simplestreams | incus
	Alias    string // e.g. ubuntu/24.04/cloud
}

// LaunchRequest is the per-instance input to Client.Launch. Fields map
// directly onto the relevant subset of api.InstancesPost.
type LaunchRequest struct {
	// Name is the Incus instance name. Must be unique within the
	// target project.
	Name string
	// Type is the instance type. MVP is always InstanceTypeVM.
	Type InstanceType
	// Profiles lists the Incus profiles to apply, in order. Empty falls
	// back to the upstream default ("default").
	Profiles []string
	// Image identifies the base image to clone.
	Image ImageSource
	// Config carries instance-level config keys. The orchestrator uses
	// this for cloud-init.user-data and the user.incuse.* tag set.
	Config map[string]string
	// Devices lets callers attach disks, NICs, etc. when the profile
	// alone is not enough.
	Devices map[string]map[string]string
	// Ephemeral instances are deleted automatically on stop. Useful as
	// a defence-in-depth backstop to cloud-init's poweroff.
	Ephemeral bool
	// Description is surfaced by `incus list` and helps operators
	// identify managed instances at a glance.
	Description string
}

// Instance is the read-side projection of an Incus instance, scoped to
// the fields the orchestrator cares about.
type Instance struct {
	Name        string
	Description string
	Type        InstanceType
	Status      string // "Running", "Stopped", ...
	Project     string
	Config      map[string]string
}

// Client is the lifecycle surface the orchestrator depends on. Backed
// by the upstream Incus client in production, fake-able in tests.
type Client interface {
	// Launch creates the instance and starts it, blocking until the
	// start operation finishes. Equivalent to `incus launch`.
	Launch(ctx context.Context, req LaunchRequest) (*Instance, error)
	// Stop issues a stop with force=true. Returns nil if the instance
	// is already stopped or absent — the orchestrator's reaper calls
	// Stop unconditionally, so idempotence simplifies the call sites.
	Stop(ctx context.Context, name string) error
	// Delete removes the instance. Caller must Stop first unless the
	// instance is Ephemeral. Returns nil if the instance is absent.
	Delete(ctx context.Context, name string) error
	// Get returns nil with no error when the instance is absent.
	Get(ctx context.Context, name string) (*Instance, error)
	// List returns instances in the named project. An empty
	// projectFilter falls back to the connection's default project.
	List(ctx context.Context, projectFilter string) ([]Instance, error)
	// Close releases the underlying connection.
	Close()
}
