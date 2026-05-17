package runner

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newReleaseServer(t *testing.T, body string, callCount *int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if callCount != nil {
			atomic.AddInt64(callCount, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

const sampleReleaseJSON = `{
  "tag_name": "v2.328.0",
  "assets": [
    {"name": "actions-runner-linux-arm64-2.328.0.tar.gz", "browser_download_url": "https://example/arm64.tgz"},
    {"name": "actions-runner-linux-x64-2.328.0.tar.gz", "browser_download_url": "https://example/x64.tgz"},
    {"name": "actions-runner-osx-x64-2.328.0.tar.gz", "browser_download_url": "https://example/macos.tgz"}
  ]
}`

func TestResolver_PicksLinuxX64(t *testing.T) {
	srv := newReleaseServer(t, sampleReleaseJSON, nil)
	t.Cleanup(srv.Close)

	r := NewLatestResolver(0)
	r.endpoint = srv.URL

	rel, err := r.Resolve(t.Context())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if rel.Version != "2.328.0" {
		t.Errorf("version: want 2.328.0, got %q", rel.Version)
	}
	if rel.DownloadURL != "https://example/x64.tgz" {
		t.Errorf("download_url: want x64.tgz, got %q", rel.DownloadURL)
	}
}

func TestResolver_CachesWithinTTL(t *testing.T) {
	var hits int64
	srv := newReleaseServer(t, sampleReleaseJSON, &hits)
	t.Cleanup(srv.Close)

	r := NewLatestResolver(time.Hour)
	r.endpoint = srv.URL

	for i := 0; i < 5; i++ {
		if _, err := r.Resolve(t.Context()); err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("network calls: want 1, got %d", got)
	}
}

func TestResolver_RefetchesAfterTTL(t *testing.T) {
	var hits int64
	srv := newReleaseServer(t, sampleReleaseJSON, &hits)
	t.Cleanup(srv.Close)

	r := NewLatestResolver(time.Millisecond)
	r.endpoint = srv.URL

	if _, err := r.Resolve(t.Context()); err != nil {
		t.Fatalf("first: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := r.Resolve(t.Context()); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Errorf("network calls: want 2, got %d", got)
	}
}

func TestResolver_ErrorsWithoutLinuxX64Asset(t *testing.T) {
	body := `{"tag_name":"v1.2.3","assets":[{"name":"actions-runner-linux-arm64-1.2.3.tar.gz","browser_download_url":"https://example/arm64.tgz"}]}`
	srv := newReleaseServer(t, body, nil)
	t.Cleanup(srv.Close)

	r := NewLatestResolver(0)
	r.endpoint = srv.URL

	_, err := r.Resolve(t.Context())
	if err == nil || !strings.Contains(err.Error(), "no linux-x64 asset") {
		t.Fatalf("want missing-asset error, got %v", err)
	}
}

func TestResolver_ErrorsOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	r := NewLatestResolver(0)
	r.endpoint = srv.URL

	if _, err := r.Resolve(t.Context()); err == nil {
		t.Fatal("want HTTP error")
	}
}

func TestResolver_ErrorsOnEmptyTag(t *testing.T) {
	srv := newReleaseServer(t, `{"tag_name":"","assets":[]}`, nil)
	t.Cleanup(srv.Close)

	r := NewLatestResolver(0)
	r.endpoint = srv.URL

	if _, err := r.Resolve(t.Context()); err == nil {
		t.Fatal("want missing-tag error")
	}
}
