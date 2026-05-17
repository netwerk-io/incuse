package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreflight_PATModeOK(t *testing.T) {
	dir := t.TempDir()
	pat := filepath.Join(dir, "github.pat")
	writeFileMode(t, pat, "ghp_abc", 0o600)

	cfg := minimalCfg(t)
	cfg.GitHub.Auth.Mode = AuthModePAT
	cfg.GitHub.Auth.PATFile = pat

	if err := Preflight(cfg); err != nil {
		t.Fatalf("preflight: %v", err)
	}
}

func TestPreflight_PATFileMissing(t *testing.T) {
	cfg := minimalCfg(t)
	cfg.GitHub.Auth.Mode = AuthModePAT
	cfg.GitHub.Auth.PATFile = "/nonexistent/github.pat"

	err := Preflight(cfg)
	if err == nil {
		t.Fatal("want error for missing PAT file")
	}
	if !strings.Contains(err.Error(), "pat_file") {
		t.Errorf("error %q: want pat_file mention", err)
	}
}

func TestPreflight_PATFileTooPermissive(t *testing.T) {
	dir := t.TempDir()
	pat := filepath.Join(dir, "github.pat")
	writeFileMode(t, pat, "ghp_abc", 0o644)

	cfg := minimalCfg(t)
	cfg.GitHub.Auth.Mode = AuthModePAT
	cfg.GitHub.Auth.PATFile = pat

	err := Preflight(cfg)
	if err == nil {
		t.Fatal("want error for 0644 PAT file")
	}
	if !strings.Contains(err.Error(), "permissions") {
		t.Errorf("error %q: want permissions mention", err)
	}
}

func TestPreflight_AppKeyChecked(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "app.pem")
	writeFileMode(t, key, "-----BEGIN", 0o600)

	cfg := minimalCfg(t)
	cfg.GitHub.Auth.Mode = AuthModeApp
	cfg.GitHub.Auth.PATFile = ""
	cfg.GitHub.Auth.App.ClientID = "Iv1.x"
	cfg.GitHub.Auth.App.PrivateKeyFile = key
	cfg.GitHub.Auth.App.InstallationID = 1

	if err := Preflight(cfg); err != nil {
		t.Fatalf("preflight: %v", err)
	}

	if err := os.Chmod(key, 0o660); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := Preflight(cfg); err == nil {
		t.Error("want error for 0660 app key")
	}
}

func TestPreflight_IncusCertsCheckedOnlyWhenURLSet(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "client.crt")
	key := filepath.Join(dir, "client.key")
	srv := filepath.Join(dir, "server.crt")
	writeFileMode(t, cert, "x", 0o600)
	writeFileMode(t, key, "x", 0o600)
	writeFileMode(t, srv, "x", 0o644)

	pat := filepath.Join(dir, "github.pat")
	writeFileMode(t, pat, "ghp_abc", 0o600)

	cfg := minimalCfg(t)
	cfg.GitHub.Auth.Mode = AuthModePAT
	cfg.GitHub.Auth.PATFile = pat

	// URL empty: cert paths ignored even if missing.
	cfg.Incus.URL = ""
	cfg.Incus.CertFile = "/nonexistent/c"
	if err := Preflight(cfg); err != nil {
		t.Fatalf("preflight should ignore certs when url empty: %v", err)
	}

	// URL set: certs checked.
	cfg.Incus.URL = "https://incus.example:8443"
	cfg.Incus.CertFile = cert
	cfg.Incus.KeyFile = key
	cfg.Incus.ServerCertFile = srv
	if err := Preflight(cfg); err != nil {
		t.Fatalf("preflight: %v", err)
	}

	// Server cert is allowed to be 0644 (public artefact).
	// Client cert at 0644 must fail.
	if err := os.Chmod(cert, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	err := Preflight(cfg)
	if err == nil || !strings.Contains(err.Error(), "incus.cert_file") {
		t.Errorf("want incus.cert_file error, got %v", err)
	}
}

func TestPreflight_NilConfig(t *testing.T) {
	if err := Preflight(nil); err == nil {
		t.Fatal("want error on nil config")
	}
}

// minimalCfg returns a config that passes Validate but with empty
// file paths the test then fills in. We can't call Load because the
// test config is constructed in-memory.
func minimalCfg(t *testing.T) *Config {
	t.Helper()
	c := &Config{}
	c.GitHub.ConfigURL = "https://github.com/example"
	c.GitHub.Auth.Mode = AuthModePAT
	c.ScaleSet.Name = "x"
	c.ScaleSet.MaxRunners = 1
	c.Runner.RunnerVersion = "0.0.0"
	c.applyDefaults()
	return c
}

func writeFileMode(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	// WriteFile honours umask; chmod explicitly to defeat it.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}
