package incus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"
)

// TestIntegration_LaunchStopDelete drives the wrapper against a real
// Incus daemon. Skipped unless one of:
//
//   - INCUSE_TEST_SOCKET=/var/lib/incus/unix.socket   (Unix socket, default path)
//   - INCUSE_TEST_URL=https://host:8443
//     INCUSE_TEST_CERT=/path/to/client.crt
//     INCUSE_TEST_KEY=/path/to/client.key
//     INCUSE_TEST_SERVER_CERT=/path/to/server.crt   (optional pin)
//
// is set. The test launches a tiny VM (alpine cloud image — small and
// boots fast even unbaked), waits for it, stops it, and deletes it.
// INCUSE_TEST_PROJECT defaults to "default"; an alternative project
// must already exist on the daemon.
//
// CI does not run this test. It is a developer ergonomics check only.
func TestIntegration_LaunchStopDelete(t *testing.T) {
	cfg, ok := integrationConfig()
	if !ok {
		t.Skip("integration test: set INCUSE_TEST_SOCKET or INCUSE_TEST_URL to enable")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	c, err := Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()

	name := "incuse-it-" + randomSuffix(t)

	t.Cleanup(func() {
		// Best-effort teardown so a panicking subtest does not leave
		// a VM running on the test daemon.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = c.Stop(ctx, name)
		_ = c.Delete(ctx, name)
	})

	if _, err := c.Launch(ctx, LaunchRequest{
		Name: name,
		Type: InstanceTypeVM,
		Image: ImageSource{
			Server:   "https://images.linuxcontainers.org",
			Protocol: "simplestreams",
			Alias:    "alpine/edge/cloud",
		},
		Config: map[string]string{
			"user.incuse.managed":     "true",
			"user.incuse.runner_name": name,
		},
		Description: "incuse integration test",
		Ephemeral:   false,
	}); err != nil {
		t.Fatalf("launch: %v", err)
	}

	got, err := c.Get(ctx, name)
	if err != nil || got == nil {
		t.Fatalf("get after launch: got=%+v err=%v", got, err)
	}
	if got.Status != "Running" {
		t.Fatalf("status after launch: want Running, got %q", got.Status)
	}

	if err := c.Stop(ctx, name); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := c.Delete(ctx, name); err != nil {
		t.Fatalf("delete: %v", err)
	}

	gone, err := c.Get(ctx, name)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if gone != nil {
		t.Fatalf("get after delete: want nil, got %+v", gone)
	}
}

func integrationConfig() (Config, bool) {
	if u := os.Getenv("INCUSE_TEST_URL"); u != "" {
		return Config{
			URL:            u,
			CertFile:       os.Getenv("INCUSE_TEST_CERT"),
			KeyFile:        os.Getenv("INCUSE_TEST_KEY"),
			ServerCertFile: os.Getenv("INCUSE_TEST_SERVER_CERT"),
			Project:        envOrDefault("INCUSE_TEST_PROJECT", "default"),
		}, true
	}
	if s := os.Getenv("INCUSE_TEST_SOCKET"); s != "" {
		return Config{
			SocketPath: s,
			Project:    envOrDefault("INCUSE_TEST_PROJECT", "default"),
		}, true
	}
	return Config{}, false
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("randomSuffix: %v", err)
	}
	return hex.EncodeToString(b[:])
}
