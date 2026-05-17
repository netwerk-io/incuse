package observability

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestServer_HealthAndReady(t *testing.T) {
	rec := New("", "")
	srv := NewServer("127.0.0.1:0", rec)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	addr := waitForAddr(t, srv)

	// /healthz starts unhealthy.
	if got := getStatus(t, "http://"+addr+"/healthz"); got != http.StatusServiceUnavailable {
		t.Errorf("healthz pre-mark: want 503, got %d", got)
	}
	srv.MarkHealthy()
	if got := getStatus(t, "http://"+addr+"/healthz"); got != http.StatusOK {
		t.Errorf("healthz post-mark: want 200, got %d", got)
	}

	// /readyz starts not ready.
	if got := getStatus(t, "http://"+addr+"/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("readyz pre-mark: want 503, got %d", got)
	}
	srv.MarkReady()
	if got := getStatus(t, "http://"+addr+"/readyz"); got != http.StatusOK {
		t.Errorf("readyz post-mark: want 200, got %d", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}

func TestServer_MetricsEndpoint(t *testing.T) {
	rec := New("v0", "abc")
	rec.RunnerSpawned()
	rec.LaunchOK()

	srv := NewServer("127.0.0.1:0", rec)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	addr := waitForAddr(t, srv)

	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("get /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics: want 200, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "incuse_runners_spawned_total") {
		t.Errorf("scrape did not include jobs_assigned counter; body=%q", string(body))
	}
}

func TestServer_ListenError(t *testing.T) {
	rec := New("", "")
	srv := NewServer("127.0.0.1:0", rec)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	waitForAddr(t, srv)

	// Try to bind a second server to the same port — must fail.
	dup := NewServer(srv.Addr(), rec)
	err := dup.Run(t.Context())
	if err == nil {
		t.Fatal("want listen error on duplicate addr")
	}
}

func waitForAddr(t *testing.T, s *Server) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a := s.Addr(); a != "" {
			return a
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("server never bound")
	return ""
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // test-only loopback URL
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}
