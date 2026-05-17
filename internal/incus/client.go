package incus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	upstream "github.com/lxc/incus/v6/client"
	"github.com/lxc/incus/v6/shared/api"
)

// Config controls how Connect reaches the Incus daemon. Exactly one of
// SocketPath or URL must be set:
//   - SocketPath  → local Unix socket (the MVP path).
//   - URL         → HTTPS, with TLSClientCert/TLSClientKey loaded from
//     CertFile / KeyFile, and (optionally) the daemon's cert pinned via
//     ServerCertFile.
//
// The HTTPS path is supported so the orchestrator can move off-host
// later without code changes; the Unix socket path stays the default
// for systemd deployments on the same host as Incus.
type Config struct {
	// SocketPath is the path to the Incus Unix socket. Default
	// "/var/lib/incus/unix.socket". Used when URL is empty.
	SocketPath string
	// URL is the HTTPS endpoint, e.g. https://incus.example.com:8443.
	// Selecting HTTPS requires CertFile + KeyFile.
	URL string
	// CertFile / KeyFile are PEM paths for TLS client auth. Required
	// when URL is set.
	CertFile string
	KeyFile  string
	// ServerCertFile pins the daemon's TLS cert. Recommended in
	// production. Empty falls back to the system trust store.
	ServerCertFile string
	// InsecureSkipVerify disables daemon-cert validation. Local-dev
	// only; never set in a deployed unit.
	InsecureSkipVerify bool
	// Project is the Incus project name to operate in. Empty defaults
	// to "default" — the orchestrator should always set this to
	// "incuse" so we never collide with hand-managed instances.
	Project string
	// UserAgent is reported on every request. Defaults to "incuse".
	UserAgent string
	// HTTPClient is an optional override used in tests to point the
	// upstream client at a httptest.Server. Production callers leave
	// this nil.
	HTTPClient *http.Client
}

// realClient is the production implementation. Its zero value is not
// usable — construct via Connect.
type realClient struct {
	server  upstream.InstanceServer
	project string
}

// Connect dials the Incus daemon per cfg and returns a ready Client.
// The returned Client must be Close()d when the orchestrator shuts
// down so the upstream event listener does not leak.
func Connect(ctx context.Context, cfg Config) (Client, error) {
	args := &upstream.ConnectionArgs{
		UserAgent:          chooseUserAgent(cfg.UserAgent),
		HTTPClient:         cfg.HTTPClient,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
		// We do not use the websocket events stream — Wait() falls back
		// to /operations/<id>/wait polling, which is sufficient for
		// per-instance lifecycle calls.
		SkipGetEvents: true,
	}

	server, err := dial(ctx, cfg, args)
	if err != nil {
		return nil, err
	}

	if cfg.Project != "" {
		server = server.UseProject(cfg.Project)
	}

	return &realClient{server: server, project: cfg.Project}, nil
}

func chooseUserAgent(ua string) string {
	if ua == "" {
		return "incuse"
	}
	return ua
}

func dial(ctx context.Context, cfg Config, args *upstream.ConnectionArgs) (upstream.InstanceServer, error) {
	switch {
	case cfg.HTTPClient != nil:
		// Test path: skip the upstream's GetServer probe so the test
		// fake doesn't have to implement /1.0 server-info routing
		// unless it wants to.
		args.SkipGetServer = true
		return upstream.ConnectIncusHTTPWithContext(ctx, args, cfg.HTTPClient)
	case cfg.URL != "":
		if cfg.CertFile == "" || cfg.KeyFile == "" {
			return nil, errors.New("incus: HTTPS connection requires CertFile and KeyFile")
		}
		cert, err := os.ReadFile(cfg.CertFile)
		if err != nil {
			return nil, fmt.Errorf("incus: read cert %q: %w", cfg.CertFile, err)
		}
		key, err := os.ReadFile(cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("incus: read key %q: %w", cfg.KeyFile, err)
		}
		args.TLSClientCert = string(cert)
		args.TLSClientKey = string(key)
		if cfg.ServerCertFile != "" {
			srv, err := os.ReadFile(cfg.ServerCertFile)
			if err != nil {
				return nil, fmt.Errorf("incus: read server cert %q: %w", cfg.ServerCertFile, err)
			}
			args.TLSServerCert = string(srv)
		}
		return upstream.ConnectIncusWithContext(ctx, cfg.URL, args)
	default:
		path := cfg.SocketPath
		if path == "" {
			path = "/var/lib/incus/unix.socket"
		}
		return upstream.ConnectIncusUnixWithContext(ctx, path, args)
	}
}

// Close disconnects the upstream client.
func (c *realClient) Close() {
	if c.server != nil {
		c.server.Disconnect()
	}
}

// Launch creates the instance with Start=true so the daemon performs
// create+start in a single operation, then blocks until Wait returns.
func (c *realClient) Launch(ctx context.Context, req LaunchRequest) (*Instance, error) {
	if err := validateLaunch(req); err != nil {
		return nil, err
	}

	post := api.InstancesPost{
		Name: req.Name,
		Type: api.InstanceType(req.Type),
		Source: api.InstanceSource{
			Type:     "image",
			Protocol: req.Image.Protocol,
			Server:   req.Image.Server,
			Alias:    req.Image.Alias,
		},
		Start: true,
		InstancePut: api.InstancePut{
			Description: req.Description,
			Config:      cloneStringMap(req.Config),
			Devices:     cloneDevices(req.Devices),
			Profiles:    append([]string(nil), req.Profiles...),
			Ephemeral:   req.Ephemeral,
		},
	}

	op, err := c.server.CreateInstance(post)
	if err != nil {
		return nil, fmt.Errorf("incus: create instance %q: %w", req.Name, err)
	}
	if err := op.WaitContext(ctx); err != nil {
		return nil, fmt.Errorf("incus: launch instance %q: %w", req.Name, err)
	}

	got, err := c.Get(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	if got == nil {
		return nil, fmt.Errorf("incus: instance %q not found after launch", req.Name)
	}
	return got, nil
}

// Stop issues a forced stop. Idempotent: a missing instance is treated
// as success so the reaper does not need to pre-check.
func (c *realClient) Stop(ctx context.Context, name string) error {
	op, err := c.server.UpdateInstanceState(name, api.InstanceStatePut{
		Action:  "stop",
		Force:   true,
		Timeout: 30,
	}, "")
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("incus: stop instance %q: %w", name, err)
	}
	if err := op.WaitContext(ctx); err != nil {
		// "stopped already" surfaces here on some versions; let it pass.
		if isAlreadyStopped(err) {
			return nil
		}
		return fmt.Errorf("incus: wait stop %q: %w", name, err)
	}
	return nil
}

// Delete removes the instance. Idempotent on missing names.
func (c *realClient) Delete(ctx context.Context, name string) error {
	op, err := c.server.DeleteInstance(name)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("incus: delete instance %q: %w", name, err)
	}
	if err := op.WaitContext(ctx); err != nil {
		return fmt.Errorf("incus: wait delete %q: %w", name, err)
	}
	return nil
}

// Get returns nil with no error when the instance is absent — that
// shape lets the orchestrator's reaper express "is this still alive?"
// without juggling typed errors.
func (c *realClient) Get(_ context.Context, name string) (*Instance, error) {
	got, _, err := c.server.GetInstance(name)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("incus: get instance %q: %w", name, err)
	}
	return toInstance(got), nil
}

// List returns instances in the named project, falling back to the
// connection's default when projectFilter is empty.
func (c *realClient) List(_ context.Context, projectFilter string) ([]Instance, error) {
	server := c.server
	if projectFilter != "" && projectFilter != c.project {
		server = c.server.UseProject(projectFilter)
	}

	got, err := server.GetInstances(api.InstanceTypeAny)
	if err != nil {
		return nil, fmt.Errorf("incus: list instances: %w", err)
	}
	out := make([]Instance, 0, len(got))
	for i := range got {
		out = append(out, *toInstance(&got[i]))
	}
	return out, nil
}

func validateLaunch(req LaunchRequest) error {
	switch {
	case req.Name == "":
		return errors.New("incus: LaunchRequest.Name is required")
	case req.Type == "":
		return errors.New("incus: LaunchRequest.Type is required")
	case req.Image.Alias == "" && req.Image.Server == "":
		return errors.New("incus: LaunchRequest.Image is required")
	}
	return nil
}

func toInstance(in *api.Instance) *Instance {
	if in == nil {
		return nil
	}
	return &Instance{
		Name:        in.Name,
		Description: in.Description,
		Type:        InstanceType(in.Type),
		Status:      in.Status,
		Project:     in.Project,
		Config:      cloneStringMap(in.Config),
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneDevices(in map[string]map[string]string) map[string]map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]map[string]string, len(in))
	for k, v := range in {
		out[k] = cloneStringMap(v)
	}
	return out
}

// isNotFound matches the upstream's typed 404s. The shared/api package
// emits StatusError values that wrap an HTTP status; we use the helper
// rather than string-matching so this stays robust across releases.
func isNotFound(err error) bool {
	return api.StatusErrorCheck(err, http.StatusNotFound)
}

// isAlreadyStopped covers the cosmetic error the daemon returns when
// asked to stop an instance that has already exited (e.g. cloud-init
// poweroff raced our stop). The exact wording has shifted between
// releases — match on the stable substring.
func isAlreadyStopped(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "already stopped") || strings.Contains(msg, "is not running")
}
