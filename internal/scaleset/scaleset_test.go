package scaleset

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	ssapi "github.com/actions/scaleset"

	"github.com/netwerk-io/incuse/internal/config"
)

// fakeAPI is the slice of *ssapi.Client that reconcileScaleSet drives.
// State is recorded so tests can assert on the call sequence.
type fakeAPI struct {
	mu        sync.Mutex
	created   []*ssapi.RunnerScaleSet
	deleted   []int
	createErr error
	deleteErr error
	createOut *ssapi.RunnerScaleSet
}

func (f *fakeAPI) CreateRunnerScaleSet(_ context.Context, ss *ssapi.RunnerScaleSet) (*ssapi.RunnerScaleSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, ss)
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createOut != nil {
		return f.createOut, nil
	}
	out := *ss
	out.ID = 99
	return &out, nil
}

func (f *fakeAPI) DeleteRunnerScaleSet(_ context.Context, id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

var _ scaleSetAPI = (*fakeAPI)(nil)

func TestReconcile_CreatesWhenAbsent(t *testing.T) {
	api := &fakeAPI{}
	want := []string{"incuse", "vcpu=2", "arch=arm64"}
	got, err := reconcileScaleSet(t.Context(), api, "incuse", 7, nil, want, discardLogger())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.ID != 99 {
		t.Errorf("returned scale set: want ID=99, got %d", got.ID)
	}
	if len(api.created) != 1 {
		t.Fatalf("create call count: want 1, got %d", len(api.created))
	}
	if len(api.deleted) != 0 {
		t.Errorf("must not delete when no existing scale set; got %v", api.deleted)
	}
	gotNames := make([]string, len(api.created[0].Labels))
	for i, l := range api.created[0].Labels {
		gotNames[i] = l.Name
	}
	if !reflect.DeepEqual(gotNames, want) {
		t.Errorf("create labels: want %v, got %v", want, gotNames)
	}
	if api.created[0].RunnerGroupID != 7 {
		t.Errorf("runner group id: want 7, got %d", api.created[0].RunnerGroupID)
	}
}

func TestReconcile_NoOpWhenLabelsMatch(t *testing.T) {
	api := &fakeAPI{}
	existing := &ssapi.RunnerScaleSet{
		ID:   42,
		Name: "incuse",
		Labels: []ssapi.Label{
			{Name: "incuse"}, {Name: "vcpu=2"}, {Name: "arch=arm64"},
		},
	}
	want := []string{"vcpu=2", "Arch=arm64", "INCUSE"} // different order/casing on purpose
	got, err := reconcileScaleSet(t.Context(), api, "incuse", 7, existing, want, discardLogger())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got.ID != 42 {
		t.Errorf("returned scale set: want existing ID=42, got %d", got.ID)
	}
	if len(api.created) != 0 || len(api.deleted) != 0 {
		t.Errorf("no-op path must not call create/delete; created=%v deleted=%v", api.created, api.deleted)
	}
}

func TestReconcile_DeletesAndRecreatesOnLabelDrift(t *testing.T) {
	api := &fakeAPI{}
	existing := &ssapi.RunnerScaleSet{
		ID:   42,
		Name: "incuse",
		Labels: []ssapi.Label{
			{Name: "incuse"}, {Name: "vcpu=8"}, // stale: tier we no longer advertise
		},
	}
	want := []string{"incuse", "vcpu=2", "vcpu=4"}
	if _, err := reconcileScaleSet(t.Context(), api, "incuse", 7, existing, want, discardLogger()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(api.deleted) != 1 || api.deleted[0] != 42 {
		t.Errorf("expected delete of stale id=42, got %v", api.deleted)
	}
	if len(api.created) != 1 {
		t.Fatalf("expected 1 recreate, got %d", len(api.created))
	}
	gotNames := make([]string, len(api.created[0].Labels))
	for i, l := range api.created[0].Labels {
		gotNames[i] = l.Name
	}
	sort.Strings(gotNames)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if !reflect.DeepEqual(gotNames, wantSorted) {
		t.Errorf("recreate labels: want %v, got %v", wantSorted, gotNames)
	}
}

func TestReconcile_PropagatesDeleteError(t *testing.T) {
	api := &fakeAPI{deleteErr: errors.New("nope")}
	existing := &ssapi.RunnerScaleSet{ID: 42, Labels: []ssapi.Label{{Name: "old"}}}
	_, err := reconcileScaleSet(t.Context(), api, "incuse", 7, existing, []string{"new"}, discardLogger())
	if err == nil {
		t.Fatal("want delete error to propagate")
	}
}

func TestOpenSession_RetriesOn409(t *testing.T) {
	prev := sessionRetryBackoff
	sessionRetryBackoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { sessionRetryBackoff = prev })

	var attempts int
	opener := func(_ context.Context, _ int, _ string) (*ssapi.MessageSessionClient, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("HTTP 409 conflict: stale session")
		}
		return nil, nil // success — concrete value left nil since the test doesn't dereference it
	}
	if _, err := openSessionWithRetry(t.Context(), opener, 1, "host", discardLogger()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts: want 3, got %d", attempts)
	}
}

func TestOpenSession_GivesUpAfterMaxAttempts(t *testing.T) {
	prev := sessionRetryBackoff
	sessionRetryBackoff = func(int) time.Duration { return time.Microsecond }
	t.Cleanup(func() { sessionRetryBackoff = prev })

	opener := func(_ context.Context, _ int, _ string) (*ssapi.MessageSessionClient, error) {
		return nil, errors.New("HTTP 409")
	}
	_, err := openSessionWithRetry(t.Context(), opener, 1, "host", discardLogger())
	if err == nil {
		t.Fatal("want error after exhausting retries")
	}
}

func TestOpenSession_NonRetryableErrorReturnsImmediately(t *testing.T) {
	var attempts int
	opener := func(_ context.Context, _ int, _ string) (*ssapi.MessageSessionClient, error) {
		attempts++
		return nil, errors.New("HTTP 500 internal")
	}
	_, err := openSessionWithRetry(t.Context(), opener, 1, "host", discardLogger())
	if err == nil {
		t.Fatal("want error")
	}
	if attempts != 1 {
		t.Errorf("non-409 must not retry; got %d attempts", attempts)
	}
}

func TestOpenSession_RespectsContextCancel(t *testing.T) {
	prev := sessionRetryBackoff
	sessionRetryBackoff = func(int) time.Duration { return time.Hour }
	t.Cleanup(func() { sessionRetryBackoff = prev })

	ctx, cancel := context.WithCancel(t.Context())
	opener := func(_ context.Context, _ int, _ string) (*ssapi.MessageSessionClient, error) {
		cancel()
		return nil, errors.New("HTTP 409")
	}
	_, err := openSessionWithRetry(ctx, opener, 1, "host", discardLogger())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func validOptions() Options {
	return Options{
		Spec: config.ScaleSetConfig{
			Name:        "incuse",
			RunnerGroup: "Default",
			BaseLabels:  []string{"incuse"},
			MaxRunners:  4,
		},
		VCPUTiers:    []int{1, 2, 4},
		ConfigureURL: "https://github.com/netwerk-io",
		PAT:          "ghp_test",
		Logger:       discardLogger(),
		Version:      "test",
	}
}

func TestNew_ValidationFailures(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Options)
		want   string
	}{
		{"missing logger", func(o *Options) { o.Logger = nil }, "logger is required"},
		{"missing configure_url", func(o *Options) { o.ConfigureURL = "" }, "configure_url is required"},
		{"missing scale set name", func(o *Options) { o.Spec.Name = "" }, "scale_set.name is required"},
		{"missing runner group", func(o *Options) { o.Spec.RunnerGroup = "" }, "scale_set.runner_group is required"},
		{"missing vcpu tiers", func(o *Options) { o.VCPUTiers = nil }, "vcpu_tiers"},
		{
			"app mode without client_id",
			func(o *Options) { o.PAT = ""; o.AppPrivateKeyPEM = "pem"; o.AppInstallationID = 1 },
			"client_id is required",
		},
		{
			"app mode without installation",
			func(o *Options) { o.PAT = ""; o.AppPrivateKeyPEM = "pem"; o.AppClientID = "cid" },
			"installation_id is required",
		},
		{"no auth at all", func(o *Options) { o.PAT = "" }, "PAT or App private key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := validOptions()
			tc.mutate(&opts)
			if _, err := New(opts); err == nil {
				t.Fatalf("want error containing %q, got nil", tc.want)
			} else if !contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %q", tc.want, err.Error())
			}
		})
	}
}

func TestNew_AcceptsValidPATMode(t *testing.T) {
	ss, err := New(validOptions())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if ss.ScaleSetID() != 0 {
		t.Errorf("ScaleSetID before Bootstrap: want 0, got %d", ss.ScaleSetID())
	}
	if ss.Spec().Name != "incuse" {
		t.Errorf("Spec().Name: want incuse, got %q", ss.Spec().Name)
	}
}

func TestRun_RequiresBootstrap(t *testing.T) {
	ss, _ := New(validOptions())
	err := ss.Run(t.Context(), nil, &recordingMinter{})
	if err == nil || !contains(err.Error(), "Bootstrap must succeed") {
		t.Fatalf("want pre-bootstrap guard, got %v", err)
	}
}

func TestGenerateJITConfig_RequiresBootstrap(t *testing.T) {
	ss, _ := New(validOptions())
	if _, _, err := ss.GenerateJITConfig(t.Context(), "n", "_work"); err == nil {
		t.Fatal("want pre-bootstrap guard")
	}
}

func TestRemoveRunner_RequiresBootstrap(t *testing.T) {
	ss, _ := New(validOptions())
	if err := ss.RemoveRunner(t.Context(), 1); err == nil {
		t.Fatal("want pre-bootstrap guard")
	}
}

func TestClose_NilSessionIsNoop(t *testing.T) {
	ss, _ := New(validOptions())
	if err := ss.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestLabelNamesMatch_CaseInsensitiveSetEquality(t *testing.T) {
	existing := []ssapi.Label{
		{Name: "incuse"}, {Name: "vcpu=2"},
	}
	if !labelNamesMatch(existing, []string{"VCPU=2", "Incuse"}) {
		t.Errorf("case-insensitive match should be true")
	}
	if labelNamesMatch(existing, []string{"incuse"}) {
		t.Errorf("different cardinality must not match")
	}
	if labelNamesMatch(existing, []string{"incuse", "vcpu=4"}) {
		t.Errorf("different members must not match")
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
