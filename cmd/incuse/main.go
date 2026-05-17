// Command incuse is a single-host orchestrator that runs ephemeral GitHub
// Actions runners on Incus VMs. This file is the wiring entrypoint; the
// real work lives under internal/.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
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

	if *validate {
		// Real validation lands with the config package in phase 3.
		logger.Info("validate flag is a placeholder until config lands", "config", *configPath)
		return
	}

	logger.Info("incuse not yet implemented",
		"version", version,
		"commit", commit,
		"config", *configPath,
	)
}
