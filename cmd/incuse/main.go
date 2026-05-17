// Command incuse is a single-host orchestrator that runs ephemeral
// GitHub Actions runners on Incus VMs. This file is the wiring
// entrypoint; the real work lives under internal/.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/vegardx/incuse/internal/config"
	"github.com/vegardx/incuse/internal/incus"
	"github.com/vegardx/incuse/internal/orchestrator"
	"github.com/vegardx/incuse/internal/runner"
	"github.com/vegardx/incuse/internal/scaleset"
)

// Stamped by the Makefile via -ldflags.
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	var (
		configPath  = flag.String("config", "/etc/incuse/config.yaml", "path to YAML config file")
		showVersion = flag.Bool("version", false, "print version and exit")
		validate    = flag.Bool("validate", false, "validate config and referenced files, then exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("incuse %s (%s)\n", version, commit)
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config", "path", *configPath, "error", err)
		os.Exit(1)
	}

	if *validate {
		if err := config.Preflight(cfg); err != nil {
			logger.Error("preflight", "path", *configPath, "error", err)
			os.Exit(1)
		}
		logger.Info("config ok", "path", *configPath)
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := run(ctx, logger, cfg); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("incuse exited with error", "error", err)
		os.Exit(1)
	}
	logger.Info("incuse stopped")
}

// run owns the dependency graph: build the Incus client, build the
// scale-set client, build the release resolver, build the
// orchestrator, then start scaleset.Run + orchestrator.Run as siblings
// in an errgroup. Either side returning unwinds the other.
func run(ctx context.Context, logger *slog.Logger, cfg *config.Config) error {
	incusClient, err := incus.Connect(ctx, incus.Config{
		SocketPath:         cfg.Incus.SocketPath,
		URL:                cfg.Incus.URL,
		CertFile:           cfg.Incus.CertFile,
		KeyFile:            cfg.Incus.KeyFile,
		ServerCertFile:     cfg.Incus.ServerCertFile,
		InsecureSkipVerify: cfg.Incus.InsecureSkipVerify,
		Project:            cfg.Incus.Project,
		UserAgent:          fmt.Sprintf("incuse/%s", version),
	})
	if err != nil {
		return fmt.Errorf("incus connect: %w", err)
	}
	defer incusClient.Close()

	pat, appKey, err := readAuthCreds(cfg.GitHub.Auth)
	if err != nil {
		return fmt.Errorf("read github auth: %w", err)
	}

	ss, err := scaleset.New(scaleset.Options{
		Spec:              cfg.ScaleSet,
		VCPUTiers:         cfg.Runner.VCPUTiers,
		ConfigureURL:      cfg.GitHub.ConfigURL,
		PAT:               pat,
		AppClientID:       cfg.GitHub.Auth.App.ClientID,
		AppPrivateKeyPEM:  appKey,
		AppInstallationID: cfg.GitHub.Auth.App.InstallationID,
		Logger:            logger,
		Version:           version,
	})
	if err != nil {
		return fmt.Errorf("scaleset new: %w", err)
	}
	if err := ss.Bootstrap(ctx); err != nil {
		return fmt.Errorf("scaleset bootstrap: %w", err)
	}
	defer func() {
		// Use a fresh context for shutdown — the parent ctx is already
		// cancelled by the time defers run.
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer closeCancel()
		if err := ss.Close(closeCtx); err != nil {
			logger.Warn("scaleset close failed", "error", err)
		}
	}()

	resolver := runner.NewLatestResolver(time.Hour)

	orch, err := orchestrator.New(orchestrator.Config{
		IncusClient:     incusClient,
		ScaleSet:        ss,
		ReleaseResolver: resolver,
		IncusCfg:        cfg.Incus,
		RunnerCfg:       cfg.Runner,
		Logger:          logger,
	})
	if err != nil {
		return fmt.Errorf("orchestrator new: %w", err)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return ss.Run(gctx, orch, orch) })
	g.Go(func() error { return orch.Run(gctx) })
	return g.Wait()
}

// readAuthCreds resolves the on-disk PAT or App private-key file into
// the strings the scaleset constructor expects.
func readAuthCreds(auth config.AuthConfig) (pat, appKey string, err error) {
	switch auth.Mode {
	case config.AuthModePAT:
		b, err := os.ReadFile(auth.PATFile)
		if err != nil {
			return "", "", fmt.Errorf("read pat_file %q: %w", auth.PATFile, err)
		}
		return string(trimNewlines(b)), "", nil
	case config.AuthModeApp:
		b, err := os.ReadFile(auth.App.PrivateKeyFile)
		if err != nil {
			return "", "", fmt.Errorf("read private_key_file %q: %w", auth.App.PrivateKeyFile, err)
		}
		return "", string(b), nil
	default:
		return "", "", fmt.Errorf("unknown auth mode %q", auth.Mode)
	}
}

func trimNewlines(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}
