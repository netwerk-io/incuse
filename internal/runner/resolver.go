// Package runner builds the cloud-init payload that turns a fresh
// Ubuntu cloud image into a one-shot GitHub Actions runner: download
// actions/runner, write a systemd unit that runs it once with a JIT
// config, then power off when the unit exits.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Release is the resolved actions/runner release that cloud-init will
// install. Populated by LatestResolver from the GitHub releases API.
type Release struct {
	// Version is the runner version with the leading "v" stripped
	// (e.g. "2.328.0"). Used for log lines and the unpacked directory
	// path inside the VM.
	Version string

	// DownloadURL is the linux-x64 tarball URL — the value from the
	// GitHub release asset's browser_download_url field.
	DownloadURL string
}

// LatestResolver resolves the latest actions/runner release for the
// architecture incuse hands out (amd64 / linux-x64 in the MVP). It
// caches the result and refreshes on a ticker so a 100-runner burst
// hits api.github.com once, not 100 times.
type LatestResolver struct {
	httpClient *http.Client
	endpoint   string
	ttl        time.Duration

	mu       sync.RWMutex
	cached   *Release
	cachedAt time.Time
}

// NewLatestResolver returns a resolver pointing at the public GitHub
// API. ttl is how long a successful resolution stays in the cache
// before the next call refreshes; zero means "never cache" (used in
// tests).
func NewLatestResolver(ttl time.Duration) *LatestResolver {
	return &LatestResolver{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		endpoint:   "https://api.github.com/repos/actions/runner/releases/latest",
		ttl:        ttl,
	}
}

// Resolve returns the latest release, fetching from GitHub if the
// cache is empty or expired. Concurrent callers serialise on the
// network fetch — the second-arrival fast-path returns the cached
// value once the first arrival populates it.
func (r *LatestResolver) Resolve(ctx context.Context) (Release, error) {
	if cached, ok := r.fromCache(); ok {
		return cached, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-check inside the lock: another goroutine may have populated
	// while we were waiting on Lock().
	if r.cached != nil && (r.ttl == 0 || time.Since(r.cachedAt) < r.ttl) {
		return *r.cached, nil
	}

	rel, err := r.fetch(ctx)
	if err != nil {
		return Release{}, err
	}
	r.cached = &rel
	r.cachedAt = time.Now()
	return rel, nil
}

func (r *LatestResolver) fromCache() (Release, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.cached == nil {
		return Release{}, false
	}
	if r.ttl > 0 && time.Since(r.cachedAt) >= r.ttl {
		return Release{}, false
	}
	return *r.cached, true
}

type githubReleaseAsset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
}

type githubReleaseResponse struct {
	TagName string               `json:"tag_name"`
	Assets  []githubReleaseAsset `json:"assets"`
}

func (r *LatestResolver) fetch(ctx context.Context) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.endpoint, nil)
	if err != nil {
		return Release{}, fmt.Errorf("building releases request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	// The endpoint is unauthenticated for public repos; an Authorization
	// header would just consume rate-limit budget on the wrong account.

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("calling github releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("github releases returned %s", resp.Status)
	}

	var body githubReleaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Release{}, fmt.Errorf("decoding releases response: %w", err)
	}
	if body.TagName == "" {
		return Release{}, fmt.Errorf("github releases response missing tag_name")
	}
	version := strings.TrimPrefix(body.TagName, "v")

	wantSuffix := fmt.Sprintf("linux-x64-%s.tar.gz", version)
	for _, a := range body.Assets {
		if strings.HasSuffix(a.Name, wantSuffix) && a.DownloadURL != "" {
			return Release{Version: version, DownloadURL: a.DownloadURL}, nil
		}
	}
	return Release{}, fmt.Errorf("no linux-x64 asset found for tag %s", body.TagName)
}
