package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Preflight checks that every file referenced by the config is present
// and not world- or group-readable. Run after Validate, before the
// orchestrator dials anything. Suitable for `ExecStartPre=` in the
// systemd unit so the service refuses to start with a missing or
// world-readable secret.
//
// What it checks:
//   - PAT file (mode=pat) exists and is mode <= 0600.
//   - App private-key file (mode=app) exists and is mode <= 0600.
//   - Incus client cert/key (when url set) exist and are mode <= 0600.
//   - Incus server cert (when set) exists and is mode <= 0644
//     (server cert is a public artefact; we just require it's there).
//
// What it deliberately doesn't do:
//   - Open the Incus daemon connection. That's a runtime concern
//     and produces clearer errors at the call site.
//   - Talk to GitHub. Same reason.
func Preflight(cfg *Config) error {
	if cfg == nil {
		return errors.New("preflight: nil config")
	}
	var errs []string
	check := func(path, what string, maxMode os.FileMode) {
		if path == "" {
			return
		}
		fi, err := os.Stat(path)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s %q: %v", what, path, err))
			return
		}
		if fi.Mode().IsDir() {
			errs = append(errs, fmt.Sprintf("%s %q: is a directory", what, path))
			return
		}
		// Mask off type bits — we only care about permission bits.
		perm := fi.Mode().Perm()
		if perm&^maxMode != 0 {
			errs = append(errs, fmt.Sprintf("%s %q: permissions %#o exceed required %#o (chmod %#o %s)",
				what, path, perm, maxMode, maxMode, path))
		}
	}

	switch cfg.GitHub.Auth.Mode {
	case AuthModePAT:
		check(cfg.GitHub.Auth.PATFile, "github.auth.pat_file", 0o600)
	case AuthModeApp:
		check(cfg.GitHub.Auth.App.PrivateKeyFile, "github.auth.app.private_key_file", 0o600)
	}

	if cfg.Incus.URL != "" {
		check(cfg.Incus.CertFile, "incus.cert_file", 0o600)
		check(cfg.Incus.KeyFile, "incus.key_file", 0o600)
		check(cfg.Incus.ServerCertFile, "incus.server_cert_file", 0o644)
	}

	if len(errs) > 0 {
		return errors.New("preflight failed:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}
