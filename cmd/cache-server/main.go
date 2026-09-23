package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/artifact"
	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/cache"
	cacheserver "github.com/cre4ture/bazel-github-actions-cache-v2/internal/server"
)

var version = "dev"

type readyInfo struct {
	URL      string `json:"url"`
	GRPCURL  string `json:"grpc_url"`
	StatsURL string `json:"stats_url"`
	PID      int    `json:"pid"`
	Version  string `json:"version"`
}

func main() {
	if err := run(); err != nil {
		log.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		port             = flag.Int("port", 0, "loopback TCP port; zero selects a dynamic port")
		grpcPort         = flag.Int("grpc-port", 0, "loopback TCP port for the gRPC remote-cache API; zero selects a dynamic port")
		cacheDir         = flag.String("cache-dir", "", "local spool directory")
		keyPrefix        = flag.String("key-prefix", "bazel-http-v1", "GitHub cache key prefix")
		storageMode      = flag.String("storage-mode", "objects", "storage mode: objects or packs")
		packSize         = flag.Int64("pack-size", 8*1024*1024, "target CARv2 pack size in bytes")
		packFlush        = flag.Duration("pack-flush-interval", 30*time.Second, "maximum delay before flushing pending CARv2 data")
		packRenew        = flag.Duration("pack-renew-interval", 15*time.Second, "delay before renewing retention for packs that only presence checks touched")
		packCompression  = flag.Bool("pack-compression", true, "compress individual pack blocks that shrink meaningfully")
		packLevel        = flag.Int64("pack-compression-level", 3, "zstd level used for pack blocks (1-19)")
		maxManifests     = flag.Int("max-manifests", 2048, "maximum manifests to read during discovery; any beyond this are ignored")
		writeEnabled     = flag.Bool("write-enabled", false, "publish validated uploads")
		failOpen         = flag.Bool("fail-open", true, "degrade backend errors to misses/success")
		maxBlobSize      = flag.Int64("max-blob-size", 512*1024*1024, "maximum object size in bytes")
		maxConcurrent    = flag.Int("max-concurrent", 4, "maximum concurrent backend operations")
		uploadsPerMinute = flag.Int("uploads-per-minute", 180, "maximum GitHub cache uploads per minute")
		backendTimeout   = flag.Duration("backend-timeout", 5*time.Minute, "timeout per backend operation")
		readyFile        = flag.String("ready-file", "", "write startup metadata to this file")
		statsFile        = flag.String("stats-file", "", "write final statistics to this file")
		usageArtifact    = flag.String("usage-artifact", "", "publish the usage record as a job artifact with this name; empty disables it")
		assetRoutes      = flag.String("asset-header-routes", "", "newline-separated https URI patterns whose credentials may be forwarded to an origin")
		showVersion      = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *readyFile == "" {
		return fmt.Errorf("--ready-file is required")
	}

	dir, err := cacheserver.SafeCacheDir(*cacheDir)
	if err != nil {
		return fmt.Errorf("prepare spool directory: %w", err)
	}
	defer os.RemoveAll(dir)

	backend, err := cache.NewActionsBackend(*backendTimeout)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		return fmt.Errorf("listen on loopback: %w", err)
	}
	defer listener.Close()
	address := listener.Addr().(*net.TCPAddr)
	baseURL := "http://127.0.0.1:" + strconv.Itoa(address.Port)

	grpcListener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(*grpcPort)))
	if err != nil {
		return fmt.Errorf("listen on loopback for gRPC: %w", err)
	}
	defer grpcListener.Close()
	grpcAddress := grpcListener.Addr().(*net.TCPAddr)
	grpcURL := "grpc://127.0.0.1:" + strconv.Itoa(grpcAddress.Port)

	shutdownRequested := make(chan struct{}, 1)
	requestShutdown := func() {
		select {
		case shutdownRequested <- struct{}{}:
		default:
		}
	}
	logger := log.New(os.Stderr, "bazel-gha-cache: ", log.LstdFlags|log.LUTC)
	var catalog cache.Catalog
	if *storageMode == "packs" {
		catalog, err = cache.NewActionsCatalog(*backendTimeout, logger)
		if err != nil {
			return err
		}
	}
	srv, err := cacheserver.New(cacheserver.Config{
		Backend:              backend,
		Catalog:              catalog,
		CacheDir:             dir,
		KeyPrefix:            *keyPrefix,
		StorageMode:          *storageMode,
		PackSize:             *packSize,
		PackFlushInterval:    *packFlush,
		PackRenewInterval:    *packRenew,
		PackCompression:      *packCompression,
		PackCompressionLevel: *packLevel,
		MaxManifests:         *maxManifests,
		WriteEnabled:         *writeEnabled,
		FailOpen:             *failOpen,
		MaxBlobSize:          *maxBlobSize,
		MaxConcurrent:        *maxConcurrent,
		UploadsPerMinute:     *uploadsPerMinute,
		BackendTimeout:       *backendTimeout,
		ShutdownToken:        os.Getenv("BAZEL_GHA_CACHE_SHUTDOWN_TOKEN"),
		AssetHeaderRoutes:    []string{*assetRoutes},
		Shutdown:             requestShutdown,
		Logger:               logger,
	})
	if err != nil {
		return err
	}
	_ = os.Unsetenv("BAZEL_GHA_CACHE_SHUTDOWN_TOKEN")

	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Minute,
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	serveErr := make(chan error, 2)
	go func() {
		if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			serveErr <- fmt.Errorf("serve http: %w", err)
		}
	}()
	grpcServer := srv.GRPCHandler()
	go func() {
		if err := grpcServer.Serve(grpcListener); err != nil && err != grpc.ErrServerStopped {
			serveErr <- fmt.Errorf("serve grpc: %w", err)
		}
	}()

	ready := readyInfo{
		URL:      baseURL,
		GRPCURL:  grpcURL,
		StatsURL: baseURL + "/stats",
		PID:      os.Getpid(),
		Version:  version,
	}
	data, err := json.Marshal(ready)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*readyFile, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write ready file: %w", err)
	}
	logger.Printf("ready on %s and %s (write=%t, fail_open=%t)", baseURL, grpcURL, *writeEnabled, *failOpen)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case sig := <-signals:
		logger.Printf("received signal %s", sig)
	case <-shutdownRequested:
		logger.Printf("shutdown requested")
	case err := <-serveErr:
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Printf("graceful shutdown failed: %v", err)
		_ = httpServer.Close()
	}
	stopGRPC(grpcServer)
	flushContext, flushCancel := context.WithTimeout(context.Background(), *backendTimeout)
	if err := srv.Close(flushContext); err != nil {
		logger.Printf("flush packed cache: %v", err)
	}
	flushCancel()
	stats := srv.Snapshot()
	if err := cacheserver.WriteStatsFile(*statsFile, stats); err != nil {
		logger.Printf("write stats: %v", err)
	}
	// Published here rather than by a workflow step because post steps run last;
	// nothing else gets a chance to collect it.
	publishUsage(*usageArtifact, srv, *backendTimeout, logger)
	logger.Printf("stopped: %s", strings.TrimSpace(string(stats.JSON())))
	return nil
}

func publishUsage(name string, srv *cacheserver.Server, timeout time.Duration, logger *log.Logger) {
	if name == "" {
		return
	}
	uploader, err := artifact.New(timeout)
	if err != nil {
		logger.Printf("not publishing the usage record: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := uploader.Upload(ctx, name, "cache-usage.json", srv.Usage().JSON()); err != nil {
		logger.Printf("publishing the usage record failed: %v", err)
		return
	}
	logger.Printf("published the usage record as artifact %q", name)
}

// stopGRPC drains in-flight RPCs, but does not let a stuck stream hold up the
// post step that has to flush the cache before the job ends.
func stopGRPC(server *grpc.Server) {
	stopped := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		server.Stop()
	}
}
