// Package scaleset wires incuse into the github.com/actions/scaleset
// long-poll listener. A ScaleSet handles bootstrap (resolve runner
// group, get-or-create the scale set, reconcile labels, open a message
// session) and delegates the steady-state poll loop to the upstream
// listener via a thin decorator that eagerly mints a JIT runner config
// for each JobAssigned event.
package scaleset

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	ssapi "github.com/actions/scaleset"
	sslistener "github.com/actions/scaleset/listener"

	"github.com/vegardx/incuse/internal/config"
)

// JITMinter mints a JIT runner configuration for a JobAssigned event.
// Invoked by the wrapping listener.Client decorator before the upstream
// library observes the message. Implementations pick the right runner
// shape from event.RequestLabels via config.ResolveRunnerSpec, call
// ScaleSet.GenerateJITConfig, render cloud-init, and hand a populated
// LaunchRequest to the orchestrator.
//
// Errors are logged but not returned to the listener — failing a mint
// must not poison the message queue, because the upstream library will
// call DeleteMessage regardless. GitHub's re-queue mechanics are the
// retry path.
type JITMinter interface {
	Mint(ctx context.Context, event *ssapi.JobAssigned) error
}

// Options configures a new ScaleSet.
type Options struct {
	// Spec is the scale-set config from config.yaml — Name,
	// RunnerGroup, BaseLabels, MaxRunners.
	Spec config.ScaleSetConfig

	// VCPUTiers is the list of vcpu sizes incuse advertises on the
	// scale set. Drives label generation in unionLabels.
	VCPUTiers []int

	// ConfigureURL is the GitHub config URL (org/repo/enterprise scope)
	// passed to the scaleset library.
	ConfigureURL string

	// Auth selects between PAT and GitHub App. Exactly one of PAT or
	// {AppClientID, AppPrivateKeyPEM, AppInstallationID} must be set.
	PAT               string
	AppClientID       string
	AppPrivateKeyPEM  string
	AppInstallationID int64

	// Logger is required.
	Logger *slog.Logger

	// MetricsRecorder is optional; nil installs the upstream no-op
	// recorder.
	MetricsRecorder sslistener.MetricsRecorder

	// Version is the incuse build version, included in the User-Agent.
	Version string
}

// ScaleSet owns the upstream scale-set client and message session for
// one configured scale set.
type ScaleSet struct {
	opts Options

	client     *ssapi.Client
	msgClient  *ssapi.MessageSessionClient
	scaleSetID int

	mu       sync.Mutex
	listener *sslistener.Listener
}

// New constructs a ScaleSet. Call Bootstrap before Run.
func New(opts Options) (*ScaleSet, error) {
	if opts.Logger == nil {
		return nil, errors.New("logger is required")
	}
	if opts.ConfigureURL == "" {
		return nil, errors.New("configure_url is required")
	}
	if opts.Spec.Name == "" {
		return nil, errors.New("scale_set.name is required")
	}
	if opts.Spec.RunnerGroup == "" {
		return nil, errors.New("scale_set.runner_group is required")
	}
	if len(opts.VCPUTiers) == 0 {
		return nil, errors.New("vcpu_tiers must contain at least one tier")
	}
	if opts.PAT == "" {
		if opts.AppPrivateKeyPEM == "" {
			return nil, errors.New("PAT or App private key is required")
		}
		if opts.AppClientID == "" {
			return nil, errors.New("github.auth.app.client_id is required when using App auth")
		}
		if opts.AppInstallationID == 0 {
			return nil, errors.New("github.auth.app.installation_id is required when using App auth")
		}
	}
	return &ScaleSet{opts: opts}, nil
}

// Bootstrap resolves the runner group, gets-or-creates the scale set,
// reconciles labels, and opens a long-poll message session.
func (s *ScaleSet) Bootstrap(ctx context.Context) error {
	sysInfo := ssapi.SystemInfo{
		System:    "incuse",
		Version:   s.opts.Version,
		Subsystem: "listener",
	}

	client, err := s.buildClient(sysInfo)
	if err != nil {
		return fmt.Errorf("creating scaleset client: %w", err)
	}
	s.client = client

	grp, err := client.GetRunnerGroupByName(ctx, s.opts.Spec.RunnerGroup)
	if err != nil {
		return fmt.Errorf("resolving runner group %q: %w", s.opts.Spec.RunnerGroup, err)
	}

	wantNames := config.ValidRunnerLabels(s.opts.Spec.BaseLabels, s.opts.VCPUTiers)

	existing, err := client.GetRunnerScaleSet(ctx, grp.ID, s.opts.Spec.Name)
	if err != nil {
		return fmt.Errorf("looking up scale set %q: %w", s.opts.Spec.Name, err)
	}
	if existing != nil {
		s.opts.Logger.Info("existing scale set on GitHub",
			"scale_set_id", existing.ID,
			"existing_labels", labelNames(existing.Labels),
		)
	}

	ss, err := reconcileScaleSet(ctx, client, s.opts.Spec.Name, grp.ID, existing, wantNames, s.opts.Logger)
	if err != nil {
		return err
	}

	owner, err := sessionOwner()
	if err != nil {
		return fmt.Errorf("determining session owner: %w", err)
	}

	opener := func(ctx context.Context, scaleSetID int, owner string) (*ssapi.MessageSessionClient, error) {
		return client.MessageSessionClient(ctx, scaleSetID, owner)
	}
	msg, err := openSessionWithRetry(ctx, opener, ss.ID, owner, s.opts.Logger)
	if err != nil {
		return err
	}
	s.msgClient = msg
	s.scaleSetID = ss.ID

	s.opts.Logger.Info("scale set bootstrapped",
		"scale_set_id", ss.ID,
		"runner_group_id", grp.ID,
		"owner", owner,
	)
	return nil
}

func (s *ScaleSet) buildClient(sysInfo ssapi.SystemInfo) (*ssapi.Client, error) {
	if s.opts.PAT != "" {
		return ssapi.NewClientWithPersonalAccessToken(ssapi.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     s.opts.ConfigureURL,
			PersonalAccessToken: s.opts.PAT,
			SystemInfo:          sysInfo,
		})
	}
	return ssapi.NewClientWithGitHubApp(ssapi.ClientWithGitHubAppConfig{
		GitHubConfigURL: s.opts.ConfigureURL,
		GitHubAppAuth: ssapi.GitHubAppAuth{
			ClientID:       s.opts.AppClientID,
			InstallationID: s.opts.AppInstallationID,
			PrivateKey:     s.opts.AppPrivateKeyPEM,
		},
		SystemInfo: sysInfo,
	})
}

// scaleSetAPI is the slice of *ssapi.Client that reconcileScaleSet
// drives. Pulled out as an interface so the bootstrap state-machine
// tests can fake the upstream client without standing up the full
// GitHub-auth httptest dance.
type scaleSetAPI interface {
	CreateRunnerScaleSet(ctx context.Context, ss *ssapi.RunnerScaleSet) (*ssapi.RunnerScaleSet, error)
	DeleteRunnerScaleSet(ctx context.Context, id int) error
}

// sessionOpener is the slice of *ssapi.Client that openSessionWithRetry
// uses. Production wires this to client.MessageSessionClient; tests
// supply a stub that emits 409s on schedule.
type sessionOpener func(ctx context.Context, scaleSetID int, owner string) (*ssapi.MessageSessionClient, error)

// sessionRetryBackoff schedules the wait between 409 retries. var so
// tests can shrink it without inflating the test runtime by minutes.
var sessionRetryBackoff = func(attempt int) time.Duration {
	wait := time.Duration(attempt*30) * time.Second
	if wait > 5*time.Minute {
		wait = 5 * time.Minute
	}
	return wait
}

// reconcileScaleSet creates the scale set when missing, recreates it
// when labels drift, and otherwise returns the existing one. Recreate
// (delete+create) is required because PATCH (UpdateRunnerScaleSet)
// silently drops labels on at least one GHE version that kindling
// hit — switching to delete+create is the upstream-known workaround.
func reconcileScaleSet(
	ctx context.Context,
	client scaleSetAPI,
	name string,
	runnerGroupID int,
	existing *ssapi.RunnerScaleSet,
	wantNames []string,
	logger *slog.Logger,
) (*ssapi.RunnerScaleSet, error) {
	switch {
	case existing == nil:
		logger.Info("creating scale set",
			"name", name,
			"runner_group_id", runnerGroupID,
			"labels", wantNames,
		)
		ss, err := client.CreateRunnerScaleSet(ctx, &ssapi.RunnerScaleSet{
			Name:          name,
			RunnerGroupID: runnerGroupID,
			Labels:        toLabels(wantNames),
		})
		if err != nil {
			return nil, fmt.Errorf("creating scale set: %w", err)
		}
		return ss, nil

	case !labelNamesMatch(existing.Labels, wantNames):
		logger.Info("reconciling scale set labels (delete+recreate)",
			"scale_set_id", existing.ID,
			"old_labels", labelNamesPlain(existing.Labels),
			"new_labels", wantNames,
		)
		if err := client.DeleteRunnerScaleSet(ctx, existing.ID); err != nil {
			return nil, fmt.Errorf("deleting stale scale set: %w", err)
		}
		ss, err := client.CreateRunnerScaleSet(ctx, &ssapi.RunnerScaleSet{
			Name:          existing.Name,
			RunnerGroupID: runnerGroupID,
			Labels:        toLabels(wantNames),
		})
		if err != nil {
			return nil, fmt.Errorf("recreating scale set with updated labels: %w", err)
		}
		logger.Info("scale set recreated",
			"scale_set_id", ss.ID,
			"persisted_labels", labelNames(ss.Labels),
			"sent", wantNames,
		)
		return ss, nil

	default:
		return existing, nil
	}
}

// openSessionWithRetry opens a MessageSessionClient, backing off on
// 409 Conflict. A 409 means a stale session from a previous incuse
// instance is still active at GitHub; it clears once GitHub's TTL
// fires. Retrying with backoff is the upstream-recommended pattern.
func openSessionWithRetry(
	ctx context.Context,
	open sessionOpener,
	scaleSetID int,
	owner string,
	logger *slog.Logger,
) (*ssapi.MessageSessionClient, error) {
	for attempt := 1; ; attempt++ {
		msg, err := open(ctx, scaleSetID, owner)
		if err == nil {
			return msg, nil
		}
		if !strings.Contains(err.Error(), "409") || attempt >= 20 {
			return nil, fmt.Errorf("opening message session: %w", err)
		}
		wait := sessionRetryBackoff(attempt)
		logger.Warn("session conflict (409); stale session from previous instance — retrying",
			"attempt", attempt,
			"retry_in", wait,
		)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// Run starts the listener loop. Blocks until ctx is cancelled or the
// upstream library returns an unrecoverable error. minter is invoked
// synchronously for each JobAssigned the listener observes.
func (s *ScaleSet) Run(ctx context.Context, scaler sslistener.Scaler, minter JITMinter) error {
	if s.msgClient == nil {
		return errors.New("Bootstrap must succeed before Run")
	}
	if minter == nil {
		return errors.New("minter is required")
	}

	wrapped := &mintingClient{
		inner:  s.msgClient,
		minter: minter,
		logger: s.opts.Logger,
	}

	var lopts []sslistener.Option
	if s.opts.MetricsRecorder != nil {
		lopts = append(lopts, sslistener.WithMetricsRecorder(s.opts.MetricsRecorder))
	}

	l, err := sslistener.New(wrapped, sslistener.Config{
		ScaleSetID: s.scaleSetID,
		MaxRunners: s.opts.Spec.MaxRunners,
		Logger:     s.opts.Logger,
	}, lopts...)
	if err != nil {
		return fmt.Errorf("creating listener: %w", err)
	}

	s.mu.Lock()
	s.listener = l
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.listener = nil
		s.mu.Unlock()
	}()

	return l.Run(ctx, scaler)
}

// SetMaxRunners updates the X-ScaleSetMaxCapacity header GitHub sees on
// the next long-poll. Safe to call from multiple goroutines. No-op when
// Run has not started or has already returned.
func (s *ScaleSet) SetMaxRunners(count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return
	}
	s.listener.SetMaxRunners(count)
}

// Close releases the message session.
func (s *ScaleSet) Close(ctx context.Context) error {
	if s.msgClient == nil {
		return nil
	}
	return s.msgClient.Close(ctx)
}

// GenerateJITConfig mints a JIT runner configuration. Labels and
// runner group are inherited from the scale set — the caller only
// picks the name and work folder.
func (s *ScaleSet) GenerateJITConfig(ctx context.Context, runnerName, workFolder string) ([]byte, *ssapi.RunnerReference, error) {
	if s.client == nil {
		return nil, nil, errors.New("Bootstrap must succeed before GenerateJITConfig")
	}
	cfg, err := s.client.GenerateJitRunnerConfig(ctx, &ssapi.RunnerScaleSetJitRunnerSetting{
		Name:       runnerName,
		WorkFolder: workFolder,
	}, s.scaleSetID)
	if err != nil {
		return nil, nil, fmt.Errorf("generating JIT config: %w", err)
	}
	return []byte(cfg.EncodedJITConfig), cfg.Runner, nil
}

// RemoveRunner deletes a runner registration from GitHub by ID. Used
// by the orchestrator's reaper to release JIT-minted runners that
// never finished registering — without this call GitHub keeps the
// assignment in total_assigned and re-emits JobAssigned for the same
// matrix entry until its own (multi-hour) session timeout fires.
func (s *ScaleSet) RemoveRunner(ctx context.Context, runnerID int64) error {
	if s.client == nil {
		return errors.New("Bootstrap must succeed before RemoveRunner")
	}
	return s.client.RemoveRunner(ctx, runnerID)
}

// ScaleSetID is the GitHub-assigned ID of the bootstrapped scale set.
// Zero before Bootstrap succeeds.
func (s *ScaleSet) ScaleSetID() int { return s.scaleSetID }

// Spec returns the scale-set config the instance was constructed with.
func (s *ScaleSet) Spec() config.ScaleSetConfig { return s.opts.Spec }

// toLabels converts label names to the upstream Label shape. Type is
// left empty so the library fills "System" on Create/Update.
func toLabels(names []string) []ssapi.Label {
	out := make([]ssapi.Label, len(names))
	for i, n := range names {
		out[i] = ssapi.Label{Name: n}
	}
	return out
}

// labelNamesMatch reports whether existing covers exactly the same
// names as want, case-insensitively and order-independently.
func labelNamesMatch(existing []ssapi.Label, want []string) bool {
	if len(existing) != len(want) {
		return false
	}
	a := make([]string, len(existing))
	for i, l := range existing {
		a[i] = strings.ToLower(l.Name)
	}
	b := make([]string, len(want))
	for i, n := range want {
		b[i] = strings.ToLower(n)
	}
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// labelNames returns "name(type=...)" strings — the verbose form used
// in log lines so an operator can tell apart user / system labels.
func labelNames(labels []ssapi.Label) []string {
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i] = fmt.Sprintf("%s(type=%s)", l.Name, l.Type)
	}
	return out
}

// labelNamesPlain returns just the names, used in the recreate log so
// the diff against want_labels stays readable.
func labelNamesPlain(labels []ssapi.Label) []string {
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i] = l.Name
	}
	return out
}

// sessionOwner returns a stable identifier for the poll session. Uses
// hostname so multiple incuse instances sharing a scale set (unusual
// but possible during failover) are distinguishable in GitHub's UI.
func sessionOwner() (string, error) {
	h, err := os.Hostname()
	if err != nil {
		return "", err
	}
	if h == "" {
		return "incuse", nil
	}
	return h, nil
}
