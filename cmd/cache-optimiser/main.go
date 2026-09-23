package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/cache"
	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/optimiser"
	cacheserver "github.com/cre4ture/bazel-github-actions-cache-v2/internal/server"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		cacheDir         = flag.String("cache-dir", "", "local spool directory")
		keyPrefix        = flag.String("key-prefix", "bazel-http-v1", "GitHub cache key prefix")
		usageArtifact    = flag.String("usage-artifact", "bazel-cache-usage", "job artifact holding the usage records")
		maxRecords       = flag.Int("max-records", 25, "how many recent runs to plan from")
		packSize         = flag.Int64("pack-size", 8*1024*1024, "target size of a rebuilt pack, in object bytes")
		minWasteFraction = flag.Float64("min-waste-fraction", 0.25, "how much of a pack must be bytes a restore does not want before rebuilding it")
		minRuns          = flag.Int("min-runs", 3, "refuse to plan from fewer usage records than this")
		maxBlobSize      = flag.Int64("max-blob-size", 512*1024*1024, "maximum object size in bytes")
		timeout          = flag.Duration("backend-timeout", 5*time.Minute, "timeout per backend operation")
		dryRun           = flag.Bool("dry-run", false, "plan and report without publishing or deleting")
		summaryFile      = flag.String("summary-file", "", "write the result as JSON to this file")
		showVersion      = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	logger := log.New(os.Stderr, "cache-optimiser: ", log.LstdFlags|log.LUTC)
	dir, err := cacheserver.SafeCacheDir(*cacheDir)
	if err != nil {
		return fmt.Errorf("prepare spool directory: %w", err)
	}
	defer os.RemoveAll(dir)

	backend, err := cache.NewActionsBackend(*timeout)
	if err != nil {
		return err
	}
	catalog, err := cache.NewActionsCatalog(*timeout, logger)
	if err != nil {
		return err
	}
	records, err := optimiser.NewRecordSource(*usageArtifact, *timeout)
	if err != nil {
		return err
	}
	options := optimiser.RunOptions{
		Backend:     backend,
		Catalog:     catalog,
		Records:     records,
		CacheDir:    dir,
		KeyPrefix:   *keyPrefix,
		MaxRecords:  *maxRecords,
		MaxBlobSize: *maxBlobSize,
		Timeout:     *timeout,
		DryRun:      *dryRun,
		Plan: optimiser.Options{
			TargetPackSize:   *packSize,
			MinWasteFraction: *minWasteFraction,
			MinRuns:          *minRuns,
		},
		Log: logger.Printf,
	}
	// The pruner is the only thing here that needs actions: write, so a dry run
	// never even constructs one.
	if !*dryRun {
		pruner, err := cache.NewActionsPruner(*timeout)
		if err != nil {
			return err
		}
		options.Pruner = pruner
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	result, runErr := optimiser.Run(ctx, options)
	if *summaryFile != "" {
		if err := writeSummary(*summaryFile, result); err != nil {
			logger.Printf("write summary: %v", err)
		}
	}
	if runErr != nil {
		return runErr
	}
	if result.Skipped != "" {
		logger.Printf("read %d usage records; nothing to do: %s", result.Records, result.Skipped)
		return nil
	}
	logger.Printf("read %d usage records; rebuilt %d packs and deleted %d", result.Records, result.Rebuilt, result.Deleted)
	return nil
}

func writeSummary(path string, result optimiser.Result) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
