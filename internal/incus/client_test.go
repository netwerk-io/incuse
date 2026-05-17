package incus

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lxc/incus/v6/shared/api"
)

// fakeServer wraps an httptest.TLSServer and routes the upstream
// client's "https://custom.socket/..." requests to it. It records
// every inbound request so tests can assert on path, method, query,
// and JSON body shape.
//
// Test handlers register routes via Handle("/path", fn). The
// MUX is keyed on the method + the parsed URL path (no query,
// no host) so test setup stays terse.
type fakeServer struct {
	ts     *httptest.Server
	client *http.Client

	mu       sync.Mutex
	calls    []recordedCall
	handlers map[string]http.HandlerFunc // "METHOD /path"
}

type recordedCall struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{handlers: map[string]http.HandlerFunc{}}
	f.ts = httptest.NewTLSServer(http.HandlerFunc(f.dispatch))
	t.Cleanup(f.ts.Close)

	tsURL, err := url.Parse(f.ts.URL)
	if err != nil {
		t.Fatalf("parse httptest url: %v", err)
	}

	// Production code reaches the daemon at https://custom.socket/...
	// (a magic placeholder used by ConnectIncusHTTPWithContext).
	// Override DialContext so every connection lands on the httptest
	// server regardless of the URL host.
	f.client = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, tsURL.Host)
			},
			TLSClientConfig: &tls.Config{
				// Cleared by test cleanup; trust the httptest cert.
				InsecureSkipVerify: true, //nolint:gosec // test-only
			},
		},
	}
	return f
}

// Handle registers a handler for an exact method+path combination.
func (f *fakeServer) Handle(method, path string, fn http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method+" "+path] = fn
}

// Calls returns a copy of the recorded calls.
func (f *fakeServer) Calls() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeServer) dispatch(w http.ResponseWriter, r *http.Request) {
	body, _ := readAll(r)
	f.mu.Lock()
	f.calls = append(f.calls, recordedCall{
		Method: r.Method,
		Path:   r.URL.Path,
		Query:  r.URL.Query(),
		Body:   body,
	})
	h, ok := f.handlers[r.Method+" "+r.URL.Path]
	f.mu.Unlock()
	if !ok {
		http.Error(w, "no test handler for "+r.Method+" "+r.URL.Path, http.StatusNotImplemented)
		return
	}
	h(w, r)
}

func readAll(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer func() { _ = r.Body.Close() }()
	const cap = 1 << 20
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) > cap {
				return buf, nil
			}
		}
		if err != nil {
			return buf, nil
		}
	}
}

// asyncOp writes the standard 202 Operation Created envelope, which is
// what the upstream client expects after CreateInstance,
// UpdateInstanceState, and DeleteInstance.
func asyncOp(t *testing.T, w http.ResponseWriter, opID string) {
	t.Helper()
	op := api.Operation{
		ID:         opID,
		Class:      api.OperationClassTask,
		Status:     "Running",
		StatusCode: api.Running,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	rawMeta, err := json.Marshal(op)
	if err != nil {
		t.Fatalf("encode op: %v", err)
	}
	resp := api.Response{
		Type:       api.AsyncResponse,
		Status:     "Operation created",
		StatusCode: int(api.OperationCreated),
		Operation:  "/1.0/operations/" + opID,
		Metadata:   rawMeta,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(resp)
}

// successWait writes the operation-wait response with status_code=200,
// which makes upstream's op.Wait() return nil.
func successWait(t *testing.T, w http.ResponseWriter, opID string) {
	t.Helper()
	op := api.Operation{
		ID:         opID,
		Class:      api.OperationClassTask,
		Status:     "Success",
		StatusCode: api.Success,
		UpdatedAt:  time.Now(),
	}
	rawMeta, err := json.Marshal(op)
	if err != nil {
		t.Fatalf("encode op: %v", err)
	}
	resp := api.Response{
		Type:       api.SyncResponse,
		Status:     "Success",
		StatusCode: int(api.Success),
		Metadata:   rawMeta,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// syncInstance writes a sync response carrying the given api.Instance.
func syncInstance(t *testing.T, w http.ResponseWriter, inst api.Instance) {
	t.Helper()
	rawMeta, err := json.Marshal(inst)
	if err != nil {
		t.Fatalf("encode instance: %v", err)
	}
	resp := api.Response{
		Type:       api.SyncResponse,
		Status:     "Success",
		StatusCode: int(api.Success),
		Metadata:   rawMeta,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// syncList writes a sync response carrying a slice of instances.
func syncList(t *testing.T, w http.ResponseWriter, instances []api.Instance) {
	t.Helper()
	rawMeta, err := json.Marshal(instances)
	if err != nil {
		t.Fatalf("encode list: %v", err)
	}
	resp := api.Response{
		Type:       api.SyncResponse,
		Status:     "Success",
		StatusCode: int(api.Success),
		Metadata:   rawMeta,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func errorResp(t *testing.T, w http.ResponseWriter, code int, message string) {
	t.Helper()
	resp := api.Response{
		Type:   api.ErrorResponse,
		Code:   code,
		Error:  message,
		Status: http.StatusText(code),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}

// connect builds a Client over the fakeServer.
func (f *fakeServer) connect(t *testing.T, project string) Client {
	t.Helper()
	c, err := Connect(t.Context(), Config{
		HTTPClient: f.client,
		Project:    project,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestLaunch_BuildsExpectedRequest(t *testing.T) {
	f := newFakeServer(t)
	const opID = "op-launch"

	f.Handle(http.MethodPost, "/1.0/instances", func(w http.ResponseWriter, _ *http.Request) {
		asyncOp(t, w, opID)
	})
	f.Handle(http.MethodGet, "/1.0/operations/"+opID+"/wait", func(w http.ResponseWriter, _ *http.Request) {
		successWait(t, w, opID)
	})
	f.Handle(http.MethodGet, "/1.0/instances/runner-foo", func(w http.ResponseWriter, _ *http.Request) {
		syncInstance(t, w, api.Instance{
			Name:    "runner-foo",
			Status:  "Running",
			Type:    "virtual-machine",
			Project: "incuse",
		})
	})

	c := f.connect(t, "incuse")
	got, err := c.Launch(t.Context(), LaunchRequest{
		Name: "runner-foo",
		Type: InstanceTypeVM,
		Image: ImageSource{
			Server:   "https://images.linuxcontainers.org",
			Protocol: "simplestreams",
			Alias:    "ubuntu/24.04/cloud",
		},
		Profiles:    []string{"incuse-runner"},
		Description: "incuse runner",
		Ephemeral:   true,
		Config: map[string]string{
			"cloud-init.user-data":     "#cloud-config\n",
			"user.incuse.managed":      "true",
			"user.incuse.runner_name":  "runner-foo",
			"user.incuse.scale_set_id": "1",
		},
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if got.Name != "runner-foo" || got.Status != "Running" || got.Type != InstanceTypeVM {
		t.Fatalf("unexpected returned instance: %+v", got)
	}

	calls := f.Calls()
	if len(calls) < 1 {
		t.Fatalf("expected at least one call, got 0")
	}
	create := calls[0]
	if create.Method != http.MethodPost || create.Path != "/1.0/instances" {
		t.Fatalf("first call: want POST /1.0/instances, got %s %s", create.Method, create.Path)
	}
	if got := create.Query.Get("project"); got != "incuse" {
		t.Fatalf("project query param: want incuse, got %q", got)
	}

	var body api.InstancesPost
	if err := json.Unmarshal(create.Body, &body); err != nil {
		t.Fatalf("decode create body: %v", err)
	}
	if body.Name != "runner-foo" {
		t.Fatalf("body.Name: want runner-foo, got %q", body.Name)
	}
	if string(body.Type) != "virtual-machine" {
		t.Fatalf("body.Type: want virtual-machine, got %q", body.Type)
	}
	if !body.Start {
		t.Fatalf("body.Start: want true so the daemon does create+start in one op")
	}
	if !body.Ephemeral {
		t.Fatalf("body.Ephemeral: want true")
	}
	if body.Source.Type != "image" || body.Source.Alias != "ubuntu/24.04/cloud" ||
		body.Source.Protocol != "simplestreams" || !strings.HasSuffix(body.Source.Server, "images.linuxcontainers.org") {
		t.Fatalf("body.Source mismatch: %+v", body.Source)
	}
	if body.Config["cloud-init.user-data"] != "#cloud-config\n" {
		t.Fatalf("cloud-init.user-data not threaded: %q", body.Config["cloud-init.user-data"])
	}
	if body.Config["user.incuse.managed"] != "true" {
		t.Fatalf("user.incuse.managed not threaded: %q", body.Config["user.incuse.managed"])
	}
	if len(body.Profiles) != 1 || body.Profiles[0] != "incuse-runner" {
		t.Fatalf("body.Profiles: %+v", body.Profiles)
	}
}

func TestLaunch_ValidatesRequest(t *testing.T) {
	f := newFakeServer(t)
	c := f.connect(t, "incuse")

	cases := []struct {
		name string
		req  LaunchRequest
	}{
		{"missing name", LaunchRequest{Type: InstanceTypeVM, Image: ImageSource{Alias: "x"}}},
		{"missing type", LaunchRequest{Name: "n", Image: ImageSource{Alias: "x"}}},
		{"missing image", LaunchRequest{Name: "n", Type: InstanceTypeVM}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Launch(t.Context(), tc.req); err == nil {
				t.Fatalf("want validation error")
			}
		})
	}
	if calls := f.Calls(); len(calls) != 0 {
		t.Fatalf("validation should not reach the wire, but got %d calls", len(calls))
	}
}

func TestStop_IdempotentOnNotFound(t *testing.T) {
	f := newFakeServer(t)
	f.Handle(http.MethodPut, "/1.0/instances/missing/state", func(w http.ResponseWriter, _ *http.Request) {
		errorResp(t, w, http.StatusNotFound, "Instance not found")
	})

	c := f.connect(t, "incuse")
	if err := c.Stop(t.Context(), "missing"); err != nil {
		t.Fatalf("stop on missing: want nil, got %v", err)
	}
}

func TestStop_ForcesAndWaits(t *testing.T) {
	f := newFakeServer(t)
	const opID = "op-stop"

	f.Handle(http.MethodPut, "/1.0/instances/r/state", func(w http.ResponseWriter, _ *http.Request) {
		asyncOp(t, w, opID)
	})
	f.Handle(http.MethodGet, "/1.0/operations/"+opID+"/wait", func(w http.ResponseWriter, _ *http.Request) {
		successWait(t, w, opID)
	})

	c := f.connect(t, "incuse")
	if err := c.Stop(t.Context(), "r"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	var put api.InstanceStatePut
	for _, call := range f.Calls() {
		if call.Method == http.MethodPut && strings.HasSuffix(call.Path, "/state") {
			if err := json.Unmarshal(call.Body, &put); err != nil {
				t.Fatalf("decode put: %v", err)
			}
		}
	}
	if put.Action != "stop" {
		t.Fatalf("action: want stop, got %q", put.Action)
	}
	if !put.Force {
		t.Fatalf("force: want true so a hung guest does not pin capacity")
	}
}

func TestDelete_IdempotentOnNotFound(t *testing.T) {
	f := newFakeServer(t)
	f.Handle(http.MethodDelete, "/1.0/instances/gone", func(w http.ResponseWriter, _ *http.Request) {
		errorResp(t, w, http.StatusNotFound, "Instance not found")
	})

	c := f.connect(t, "incuse")
	if err := c.Delete(t.Context(), "gone"); err != nil {
		t.Fatalf("delete on missing: want nil, got %v", err)
	}
}

func TestGet_NotFoundReturnsNilWithoutError(t *testing.T) {
	f := newFakeServer(t)
	f.Handle(http.MethodGet, "/1.0/instances/ghost", func(w http.ResponseWriter, _ *http.Request) {
		errorResp(t, w, http.StatusNotFound, "Instance not found")
	})

	c := f.connect(t, "incuse")
	got, err := c.Get(t.Context(), "ghost")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestList_ReturnsInstances(t *testing.T) {
	f := newFakeServer(t)
	f.Handle(http.MethodGet, "/1.0/instances", func(w http.ResponseWriter, _ *http.Request) {
		syncList(t, w, []api.Instance{
			{Name: "a", Status: "Running", Type: "virtual-machine", Project: "incuse",
				InstancePut: api.InstancePut{Config: map[string]string{"user.incuse.managed": "true"}}},
			{Name: "b", Status: "Stopped", Type: "virtual-machine", Project: "incuse"},
		})
	})

	c := f.connect(t, "incuse")
	out, err := c.List(t.Context(), "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out) != 2 || out[0].Name != "a" || out[1].Name != "b" {
		t.Fatalf("unexpected list: %+v", out)
	}
	if out[0].Config["user.incuse.managed"] != "true" {
		t.Fatalf("config tags not threaded through: %+v", out[0].Config)
	}
}

func TestList_ProjectFilterAddsQueryParam(t *testing.T) {
	f := newFakeServer(t)
	var seen url.Values
	f.Handle(http.MethodGet, "/1.0/instances", func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query()
		syncList(t, w, []api.Instance{})
	})

	// Connect with a default project so that asking for a different
	// one really does have to flip the query string.
	c := f.connect(t, "default")
	if _, err := c.List(t.Context(), "incuse"); err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := seen.Get("project"); got != "incuse" {
		t.Fatalf("project filter: want incuse, got %q", got)
	}
}
