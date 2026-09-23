package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/cache"
)

var (
	digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
	prefixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`)
	urlPattern    = regexp.MustCompile(`https?://[^\s]+`)
	jwtPattern    = regexp.MustCompile(`[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`)
)

// defaultPackRenewInterval batches retention renewals long enough that a pack
// Bazel downloads shortly after a presence check costs no extra API call.
const defaultPackRenewInterval = 15 * time.Second

// defaultPackCompressionLevel favours flush latency over ratio: blocks are
// compressed once on the upload path, and level 3 already captures most of the
// available saving on cache content.
const defaultPackCompressionLevel = 3

type Config struct {
	Backend              cache.Backend
	Catalog              cache.Catalog
	CacheDir             string
	KeyPrefix            string
	StorageMode          string
	PackSize             int64
	PackFlushInterval    time.Duration
	PackRenewInterval    time.Duration
	PackCompression      bool
	PackCompressionLevel int64
	MaxManifests         int
	WriteEnabled         bool
	FailOpen             bool
	MaxBlobSize          int64
	MaxConcurrent        int
	UploadsPerMinute     int
	BackendTimeout       time.Duration
	ShutdownToken        string
	Shutdown             func()
	Logger               *log.Logger
	// AssetClient fetches remote assets from their origin. A nil client gets a
	// default that refuses to be redirected off HTTPS.
	AssetClient *http.Client
	// AssetHeaderRoutes are the URI patterns whose credentials may be forwarded
	// to an origin. Empty refuses every one of them, so an authenticated asset
	// is left to Bazel rather than cached where the whole repository reads it.
	AssetHeaderRoutes []string
}

type object struct {
	path string
	size int64
}

type publication struct {
	done chan struct{}
	err  error
}

type Server struct {
	cfg            Config
	stats          counters
	objectsMu      sync.RWMutex
	objects        map[string]object
	publicationsMu sync.Mutex
	published      map[string]struct{}
	publishing     map[string]*publication
	sem            chan struct{}
	limiter        *intervalLimiter
	packs          *packStore
	usage          *usageRecorder
	assetRoutes    []assetRoute
}

func New(cfg Config) (*Server, error) {
	if cfg.Backend == nil {
		return nil, errors.New("backend is required")
	}
	if cfg.CacheDir == "" {
		return nil, errors.New("cache directory is required")
	}
	if !prefixPattern.MatchString(cfg.KeyPrefix) {
		return nil, errors.New("key prefix must match [A-Za-z0-9][A-Za-z0-9._-]{0,79}")
	}
	if cfg.MaxBlobSize <= 0 {
		return nil, errors.New("max blob size must be positive")
	}
	if cfg.MaxConcurrent <= 0 {
		return nil, errors.New("max concurrent operations must be positive")
	}
	if cfg.UploadsPerMinute <= 0 || cfg.UploadsPerMinute > 199 {
		return nil, errors.New("uploads per minute must be between 1 and 199")
	}
	if cfg.BackendTimeout <= 0 {
		return nil, errors.New("backend timeout must be positive")
	}
	if cfg.StorageMode == "" {
		cfg.StorageMode = "objects"
	}
	if cfg.StorageMode != "objects" && cfg.StorageMode != "packs" {
		return nil, errors.New("storage mode must be objects or packs")
	}
	if cfg.StorageMode == "packs" {
		if cfg.Catalog == nil {
			return nil, errors.New("packed storage mode requires a manifest catalog")
		}
		if cfg.PackSize <= 0 || cfg.PackSize > 32*1024*1024 {
			return nil, errors.New("pack size must be between 1 byte and 32 MiB")
		}
		if cfg.PackFlushInterval <= 0 {
			return nil, errors.New("pack flush interval must be positive")
		}
		if cfg.PackRenewInterval <= 0 {
			cfg.PackRenewInterval = defaultPackRenewInterval
		}
		if cfg.PackCompressionLevel == 0 {
			cfg.PackCompressionLevel = defaultPackCompressionLevel
		}
		if cfg.PackCompressionLevel < 1 || cfg.PackCompressionLevel > 19 {
			return nil, errors.New("pack compression level must be between 1 and 19")
		}
		if cfg.MaxManifests <= 0 {
			return nil, errors.New("maximum manifests must be positive")
		}
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	if cfg.AssetClient == nil {
		cfg.AssetClient = defaultAssetClient()
	}
	assetRoutes, err := ParseAssetRoutes(cfg.AssetHeaderRoutes)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}
	server := &Server{
		cfg:         cfg,
		objects:     make(map[string]object),
		published:   make(map[string]struct{}),
		publishing:  make(map[string]*publication),
		sem:         make(chan struct{}, cfg.MaxConcurrent),
		limiter:     newIntervalLimiter(cfg.UploadsPerMinute),
		usage:       newUsageRecorder(),
		assetRoutes: assetRoutes,
	}
	server.cfg.Backend = &countingBackend{
		backend: cfg.Backend,
		counter: &server.stats.backendRequests,
	}
	if cfg.StorageMode == "packs" {
		packs, err := newPackStore(server)
		if err != nil {
			return nil, err
		}
		server.packs = packs
	}
	return server, nil
}

// countingBackend records every call into the GitHub Actions cache. Counting at
// this boundary rather than at each call site keeps the total honest: closure
// validation, packed reads, and retries all reach the API without a matching
// client request.
type countingBackend struct {
	backend cache.Backend
	counter *atomic.Uint64
}

func (b *countingBackend) Exists(ctx context.Context, key string) (bool, error) {
	b.counter.Add(1)
	return b.backend.Exists(ctx, key)
}

func (b *countingBackend) Load(ctx context.Context, key string, dst io.Writer) (bool, error) {
	b.counter.Add(1)
	return b.backend.Load(ctx, key, dst)
}

func (b *countingBackend) Save(ctx context.Context, key string, src *os.File, size int64) error {
	b.counter.Add(1)
	return b.backend.Save(ctx, key, src, size)
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *Server) Snapshot() Stats {
	stats := s.stats.snapshot()
	// Restored against used is the ratio that says whether packs are placing
	// frequently and rarely fetched content together.
	report := s.usage.report()
	stats.PackBytesRestored = uint64(report.PackBytesRestored)
	stats.PackBytesDeclared = uint64(report.PackBytesDeclared)
	stats.PackBytesUsed = uint64(report.PackBytesUsed)
	return stats
}

// Usage reports which entries this job touched and how.
func (s *Server) Usage() UsageReport {
	return s.usage.report()
}

// Close commits every pending CARv2 batch before the action process exits.
func (s *Server) Close(ctx context.Context) error {
	if s.packs == nil {
		return nil
	}
	return s.packs.close(ctx)
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/ready":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, "ready\n")
		}
		return
	case "/stats":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.serveJSON(w, r, s.Snapshot().JSON())
		return
	case "/shutdown":
		s.handleShutdown(w, r)
		return
	}

	s.stats.requests.Add(1)
	kind, digest, ok := parseObjectPath(r.URL.Path)
	if !ok {
		s.reject(w, "path must be /cas/<lowercase-sha256> or /ac/<lowercase-sha256>", http.StatusBadRequest)
		return
	}
	if kind == "cas" && digest == emptySHA256Digest && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		s.handleImplicitEmptyCASRead(w, r)
		return
	}
	switch r.Method {
	case http.MethodHead:
		s.handleHead(w, r, kind, digest)
	case http.MethodGet:
		s.handleRead(w, r, kind, digest)
	case http.MethodPut:
		s.handlePut(w, r, kind, digest)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT")
		s.reject(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleHead answers a packed CAS presence check from the manifest view. Bazel
// issues these while building without the bytes, so restoring a whole pack to
// answer one would defeat the point.
func (s *Server) handleHead(w http.ResponseWriter, r *http.Request, kind, digest string) {
	if s.packs == nil || kind != "cas" {
		s.handleRead(w, r, kind, digest)
		return
	}
	size, err := s.casPresence(r.Context(), digest)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.writeObjectHeader(w, size)
}

func (s *Server) handleImplicitEmptyCASRead(w http.ResponseWriter, r *http.Request) {
	s.implicitEmptyCASHit()
	s.writeObjectHeader(w, 0)
}

func (s *Server) writeObjectHeader(w http.ResponseWriter, size int64) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) serveJSON(w http.ResponseWriter, r *http.Request, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	provided := r.Header.Get("X-Shutdown-Token")
	expected := s.cfg.ShutdownToken
	if expected == "" || len(provided) != len(expected) ||
		subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	if s.cfg.Shutdown != nil {
		go s.cfg.Shutdown()
	}
}

func parseObjectPath(path string) (kind, digest string, ok bool) {
	if strings.Contains(path, "//") || strings.HasSuffix(path, "/") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) != 2 || (parts[0] != "cas" && parts[0] != "ac") {
		return "", "", false
	}
	if !digestPattern.MatchString(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request, kind, digest string) {
	file, size, err := s.openObject(r.Context(), kind, digest)
	if err != nil {
		s.failRequest(w, r, err)
		return
	}
	defer file.Close()

	s.writeObjectHeader(w, size)
	if r.Method == http.MethodHead {
		return
	}
	n, err := io.Copy(w, file)
	s.stats.bytesServed.Add(uint64(n))
	if err != nil {
		s.cfg.Logger.Printf("serve cache object %s/%s: %v", kind, digest, err)
	}
}

func (s *Server) failRequest(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errCacheMiss):
		http.NotFound(w, r)
	case errors.Is(err, errLocalFailure):
		http.Error(w, "local cache unavailable", http.StatusInternalServerError)
	default:
		http.Error(w, "cache backend unavailable", http.StatusBadGateway)
	}
}

func (s *Server) resolve(ctx context.Context, key, kind, digest string) (object, bool, error) {
	value, found, err := s.resolveObject(ctx, key, kind, digest)
	if found && err == nil {
		s.usage.record(kind, digest, s.packFor(kind, digest), accessDownload, value.size)
	}
	return value, found, err
}

func (s *Server) resolveObject(ctx context.Context, key, kind, digest string) (object, bool, error) {
	s.objectsMu.RLock()
	obj, ok := s.objects[key]
	s.objectsMu.RUnlock()
	if ok {
		return obj, true, nil
	}
	if s.packs != nil {
		return s.packs.resolve(ctx, key, kind, digest)
	}

	if err := s.acquire(ctx); err != nil {
		return object{}, false, err
	}
	defer s.release()

	// Re-check after waiting for another backend operation.
	s.objectsMu.RLock()
	obj, ok = s.objects[key]
	s.objectsMu.RUnlock()
	if ok {
		return obj, true, nil
	}

	file, err := os.CreateTemp(s.cfg.CacheDir, "download-*")
	if err != nil {
		return object{}, false, fmt.Errorf("create download spool: %w", err)
	}
	path := file.Name()
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()

	limited := &maxWriter{writer: file, remaining: s.cfg.MaxBlobSize}
	backendCtx, cancel := context.WithTimeout(ctx, s.cfg.BackendTimeout)
	defer cancel()
	found, err := s.cfg.Backend.Load(backendCtx, key, limited)
	if err != nil {
		return object{}, false, err
	}
	if !found {
		return object{}, false, nil
	}
	s.stats.backendDownloads.Add(1)
	if err := file.Sync(); err != nil {
		return object{}, false, fmt.Errorf("sync download spool: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		return object{}, false, fmt.Errorf("stat download spool: %w", err)
	}
	if kind == "cas" {
		actual, err := hashFile(file)
		if err != nil {
			return object{}, false, err
		}
		if actual != digest {
			return object{}, false, fmt.Errorf("CAS integrity check failed: expected %s, got %s", digest, actual)
		}
	}
	obj = object{path: path, size: info.Size()}
	s.objectsMu.Lock()
	if existing, exists := s.objects[key]; exists {
		obj = existing
	} else {
		s.objects[key] = obj
		keep = true
	}
	s.objectsMu.Unlock()
	return obj, true, nil
}

// presence reports whether a CAS object can be served, and its recorded size
// when the storage mode knows that without a download. Packed mode answers
// from the manifest view and renews the holding pack's retention.
func (s *Server) presence(ctx context.Context, kind, digest string) (int64, bool, error) {
	key := s.objectKey(kind, digest)
	s.objectsMu.RLock()
	object, ok := s.objects[key]
	s.objectsMu.RUnlock()
	if ok {
		s.usage.record(kind, digest, s.packFor(kind, digest), accessPresence, object.size)
		return object.size, true, nil
	}
	if s.packs != nil {
		if kind != "cas" {
			return 0, false, errors.New("packed storage can only resolve CAS keys")
		}
		size, found := s.packs.presence(digest)
		if found {
			s.usage.record(kind, digest, s.packFor(kind, digest), accessPresence, size)
		}
		return size, found, nil
	}
	found, err := s.backendExists(ctx, key)
	if found {
		s.usage.record(kind, digest, "", accessPresence, -1)
	}
	return -1, found, err
}

// packFor names the pack serving an entry, which is what lets a later
// optimisation pass map usage onto cache layout.
func (s *Server) packFor(kind, digest string) string {
	if s.packs == nil {
		return ""
	}
	return s.packs.packFor(kind, digest)
}

func (s *Server) backendExists(ctx context.Context, key string) (bool, error) {
	if err := s.acquire(ctx); err != nil {
		return false, err
	}
	defer s.release()

	backendCtx, cancel := context.WithTimeout(ctx, s.cfg.BackendTimeout)
	defer cancel()
	s.stats.backendExistenceChecks.Add(1)
	return s.cfg.Backend.Exists(backendCtx, key)
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request, kind, digest string) {
	if encoding := r.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		s.reject(w, "content encoding is unsupported", http.StatusUnsupportedMediaType)
		return
	}
	if r.ContentLength < 0 {
		s.reject(w, "Content-Length is required", http.StatusLengthRequired)
		return
	}
	if r.ContentLength > s.cfg.MaxBlobSize {
		s.reject(w, "object exceeds configured maximum size", http.StatusRequestEntityTooLarge)
		return
	}

	switch err := s.writeObject(r.Context(), kind, digest, r.Body, r.ContentLength); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, errPayloadTruncated):
		s.reject(w, "body size does not match Content-Length", http.StatusBadRequest)
	case errors.Is(err, errPayloadMismatch):
		s.reject(w, "CAS digest does not match request body", http.StatusUnprocessableEntity)
	case errors.Is(err, errLocalFailure):
		http.Error(w, "cannot spool upload", http.StatusInternalServerError)
	default:
		http.Error(w, "cache backend unavailable", http.StatusBadGateway)
	}
}

// publishOnce coalesces identical immutable keys for the lifetime of the
// server. Only the request which creates the publication reaches the backend
// and consumes an upload-rate-limit slot. A failed publication is not cached,
// so a later request can retry it.
func (s *Server) publishOnce(ctx context.Context, key string, file *os.File, size int64) (bool, error) {
	for {
		s.publicationsMu.Lock()
		if _, ok := s.published[key]; ok {
			s.publicationsMu.Unlock()
			return true, nil
		}
		if ongoing, ok := s.publishing[key]; ok {
			done := ongoing.done
			s.publicationsMu.Unlock()
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-done:
				if ongoing.err == nil {
					return true, nil
				}
				// The publishing request failed. Try this request as a new
				// leader so transient backend failures remain retryable.
				continue
			}
		}

		ongoing := &publication{done: make(chan struct{})}
		s.publishing[key] = ongoing
		s.publicationsMu.Unlock()

		err := s.publish(ctx, key, file, size)

		s.publicationsMu.Lock()
		if err == nil {
			s.published[key] = struct{}{}
		}
		ongoing.err = err
		delete(s.publishing, key)
		close(ongoing.done)
		s.publicationsMu.Unlock()
		return false, err
	}
}

func (s *Server) publish(ctx context.Context, key string, file *os.File, size int64) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if waited, err := s.limiter.wait(ctx); err != nil {
		return err
	} else if waited {
		s.stats.throttleWaits.Add(1)
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	backendCtx, cancel := context.WithTimeout(ctx, s.cfg.BackendTimeout)
	defer cancel()
	return s.cfg.Backend.Save(backendCtx, key, file, size)
}

func (s *Server) reject(w http.ResponseWriter, message string, status int) {
	s.stats.rejectedRequests.Add(1)
	http.Error(w, message, status)
}

func (s *Server) acquire(ctx context.Context) error {
	select {
	case s.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) release() {
	<-s.sem
}

func hashFile(file *os.File) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("seek cache object: %w", err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash cache object: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind cache object: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type maxWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *maxWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("download exceeds configured maximum size")
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func WriteStatsFile(path string, stats Stats) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func safeError(err error) string {
	if err == nil {
		return ""
	}
	message := urlPattern.ReplaceAllString(err.Error(), "[redacted-url]")
	message = jwtPattern.ReplaceAllString(message, "[redacted-token]")
	const maxLength = 1000
	if len(message) > maxLength {
		message = message[:maxLength] + "…"
	}
	return message
}

func SafeCacheDir(base string) (string, error) {
	if base != "" {
		absolute, err := filepath.Abs(base)
		if err != nil {
			return "", err
		}
		return absolute, nil
	}
	return os.MkdirTemp("", "bazel-gha-cache-v2-*")
}
