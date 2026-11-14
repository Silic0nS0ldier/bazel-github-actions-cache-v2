package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/cache"
)

type memoryBackend struct {
	mu          sync.Mutex
	objects     map[string][]byte
	loadErr     error
	existsErr   error
	saveErr     error
	saves       int
	loads       int
	existsCalls int
}

type blockingSaveBackend struct {
	*memoryBackend
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingSaveBackend) Save(ctx context.Context, key string, src *os.File, size int64) error {
	b.once.Do(func() { close(b.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.release:
	}
	return b.memoryBackend.Save(ctx, key, src, size)
}

func newMemoryBackend() *memoryBackend {
	return &memoryBackend{objects: make(map[string][]byte)}
}

func (b *memoryBackend) Load(_ context.Context, key string, dst io.Writer) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.loads++
	if b.loadErr != nil {
		return false, b.loadErr
	}
	data, ok := b.objects[key]
	if !ok {
		return false, nil
	}
	_, err := dst.Write(data)
	return true, err
}

func (b *memoryBackend) loadCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.loads
}

func (b *memoryBackend) Exists(_ context.Context, key string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.existsCalls++
	if b.existsErr != nil {
		return false, b.existsErr
	}
	_, ok := b.objects[key]
	return ok, nil
}

func (b *memoryBackend) existsCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.existsCalls
}

func (b *memoryBackend) remove(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objects, key)
}

func (b *memoryBackend) Save(_ context.Context, key string, src *os.File, size int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.saves++
	if b.saveErr != nil {
		return b.saveErr
	}
	data := make([]byte, size)
	if _, err := src.ReadAt(data, 0); err != nil && err != io.EOF {
		return err
	}
	b.objects[key] = data
	return nil
}

func testServer(t *testing.T, backend cache.Backend, mutate func(*Config)) *Server {
	t.Helper()
	var catalogBackend *memoryBackend
	switch backend := backend.(type) {
	case *memoryBackend:
		catalogBackend = backend
	case *blockingSaveBackend:
		catalogBackend = backend.memoryBackend
	default:
		t.Fatalf("test backend %T does not support cache catalog listing", backend)
	}
	cfg := Config{
		Backend:           backend,
		Catalog:           memoryCatalog{backend: catalogBackend},
		CacheDir:          t.TempDir(),
		KeyPrefix:         "test-v1",
		PackSize:          1024,
		PackFlushInterval: time.Hour,
		MaxManifests:      100,
		WriteEnabled:      true,
		FailOpen:          true,
		MaxBlobSize:       1024,
		MaxConcurrent:     2,
		UploadsPerMinute:  199,
		BackendTimeout:    time.Second,
		Logger:            log.New(io.Discard, "", 0),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	catalogBackend.mu.Lock()
	for key, data := range catalogBackend.objects {
		if strings.HasPrefix(key, cfg.KeyPrefix+"-cas-") || strings.HasPrefix(key, cfg.KeyPrefix+"-ac-") {
			server.objects[key] = object{data: bytes.Clone(data), inMemory: true, size: int64(len(data))}
		}
	}
	catalogBackend.mu.Unlock()
	t.Cleanup(func() { closePackedServer(t, server) })
	return server
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestCASPutHeadGet(t *testing.T) {
	backend := newMemoryBackend()
	server := testServer(t, backend, nil)
	data := []byte("hello bazel")
	path := "/cas/" + digest(data)

	put := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(data))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, put)
	if response.Code != http.StatusNoContent {
		t.Fatalf("PUT status = %d, body = %s", response.Code, response.Body)
	}

	head := httptest.NewRequest(http.MethodHead, path, nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, head)
	if response.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d", response.Code)
	}
	if got := response.Header().Get("Content-Length"); got != "11" {
		t.Fatalf("Content-Length = %q", got)
	}

	get := httptest.NewRequest(http.MethodGet, path, nil)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, get)
	if response.Code != http.StatusOK || response.Body.String() != string(data) {
		t.Fatalf("GET status/body = %d/%q", response.Code, response.Body.String())
	}
	stats := server.Snapshot()
	if stats.Uploads != 0 || stats.Hits != 2 || stats.BytesServed != uint64(len(data)) {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if err := server.packs.flush(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	stats = server.Snapshot()
	if stats.PackUploads != 1 || stats.ManifestUploads != 1 || stats.Uploads != 2 {
		t.Fatalf("cache object was not published as one pack and manifest: %+v", stats)
	}
}

func TestMissBreakdownAccountsForEveryMiss(t *testing.T) {
	server := testServer(t, newMemoryBackend(), nil)
	for _, path := range []string{
		"/cas/" + digest([]byte("absent blob")),
		"/ac/" + digest([]byte("absent action")),
	} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("GET %s status = %d", path, response.Code)
		}
	}

	stats := server.Snapshot()
	if stats.MissesCAS != 1 || stats.MissesAC != 1 {
		t.Fatalf("misses were not attributed by kind: %+v", stats)
	}
	// The breakdown is only useful if it explains the headline number.
	total := stats.MissesAC + stats.MissesCAS + stats.MissesPresence +
		stats.MissesRejected + stats.MissesDegraded
	if total != stats.Misses {
		t.Fatalf("breakdown sums to %d, but misses = %d", total, stats.Misses)
	}
}

func TestDuplicateCASPutsArePackedOnce(t *testing.T) {
	backend := newMemoryBackend()
	server := testServer(t, backend, nil)
	data := []byte("duplicate output")
	path := "/cas/" + digest(data)

	for range 2 {
		response := putCacheObject(server, path, data)
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body)
		}
	}
	server.packs.mu.Lock()
	pending := len(server.packs.pendingCAS)
	server.packs.mu.Unlock()
	if pending != 1 {
		t.Fatalf("pending CAS objects = %d, want one", pending)
	}
	if err := server.packs.flush(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if stats := server.Snapshot(); stats.PackUploads != 1 || stats.ManifestUploads != 1 {
		t.Fatalf("duplicate PUT created more than one packed entry: %+v", stats)
	}
}

func TestConcurrentDuplicateCASPutsArePackedOnce(t *testing.T) {
	backend := newMemoryBackend()
	server := testServer(t, backend, nil)
	data := []byte("concurrent duplicate output")
	path := "/cas/" + digest(data)

	responses := make(chan *httptest.ResponseRecorder, 2)
	put := func() {
		responses <- putCacheObject(server, path, data)
	}
	go put()
	go put()

	for range 2 {
		response := <-responses
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body)
		}
	}
	server.packs.mu.Lock()
	pending := len(server.packs.pendingCAS)
	server.packs.mu.Unlock()
	if pending != 1 {
		t.Fatalf("pending CAS objects = %d, want one", pending)
	}
}

func TestEmptyCASDigestIsServedImplicitly(t *testing.T) {
	server := testServer(t, newMemoryBackend(), nil)
	path := "/cas/" + digest(nil)

	for _, method := range []string{http.MethodHead, http.MethodGet} {
		request := httptest.NewRequest(method, path, nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK || response.Header().Get("Content-Length") != "0" || response.Body.Len() != 0 {
			t.Fatalf("%s status/length/body = %d/%q/%q", method, response.Code, response.Header().Get("Content-Length"), response.Body.Bytes())
		}
	}
	stats := server.Snapshot()
	if stats.Hits != 2 || stats.BackendDownloads != 0 || stats.BackendExistenceChecks != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestCASRejectsDigestMismatch(t *testing.T) {
	server := testServer(t, newMemoryBackend(), nil)
	request := httptest.NewRequest(http.MethodPut, "/cas/"+strings.Repeat("0", 64), strings.NewReader("not zero"))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
}

func TestStrictRequestValidation(t *testing.T) {
	server := testServer(t, newMemoryBackend(), nil)
	tests := []struct {
		name   string
		path   string
		length int64
		status int
	}{
		{"uppercase digest", "/cas/" + strings.Repeat("A", 64), 0, http.StatusBadRequest},
		{"extra path", "/cas/" + strings.Repeat("a", 64) + "/x", 0, http.StatusBadRequest},
		{"chunked upload", "/ac/" + strings.Repeat("a", 64), -1, http.StatusLengthRequired},
		{"too large", "/ac/" + strings.Repeat("a", 64), 1025, http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, test.path, bytes.NewReader(nil))
			request.ContentLength = test.length
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
		})
	}
}

func TestReadOnlyValidatesAndDiscardsBackendUpload(t *testing.T) {
	backend := newMemoryBackend()
	server := testServer(t, backend, func(cfg *Config) {
		cfg.WriteEnabled = false
	})
	data := []byte("read only")
	request := httptest.NewRequest(http.MethodPut, "/cas/"+digest(data), bytes.NewReader(data))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
	if backend.saves != 0 || server.Snapshot().DiscardedUploads != 1 {
		t.Fatalf("read-only upload reached backend")
	}
}

func TestFailOpenBackendErrors(t *testing.T) {
	backend := newMemoryBackend()
	server := testServer(t, backend, nil)
	request := httptest.NewRequest(http.MethodGet, "/ac/"+strings.Repeat("a", 64), nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}

	backend.saveErr = errors.New("unavailable")
	data := []byte("result")
	request = httptest.NewRequest(http.MethodPut, "/cas/"+digest(data), bytes.NewReader(data))
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want soft-success", response.Code)
	}
	if err := server.packs.flush(t.Context(), true); err == nil {
		t.Fatal("packed backend failure was not surfaced by flush")
	}
	if server.Snapshot().BackendSaveErrors != 1 {
		t.Fatalf("backend save failure was not counted: %+v", server.Snapshot())
	}
	backend.saveErr = nil
}

func TestStrictBackendErrors(t *testing.T) {
	backend := newMemoryBackend()
	writer := testServer(t, backend, nil)
	data := []byte("strict backend failure")
	if response := putCacheObject(writer, "/cas/"+digest(data), data); response.Code != http.StatusNoContent {
		t.Fatalf("seed PUT = %d", response.Code)
	}
	closePackedServer(t, writer)
	server := testServer(t, backend, func(cfg *Config) {
		cfg.FailOpen = false
	})
	backend.loadErr = errors.New("unavailable")
	response := readCacheObject(server, "/cas/"+digest(data))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.Code)
	}
}

func TestUnknownCASDigestIsMiss(t *testing.T) {
	backend := newMemoryBackend()
	server := testServer(t, backend, nil)
	request := httptest.NewRequest(http.MethodGet, "/cas/"+strings.Repeat("a", 64), nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	if server.Snapshot().MissesCAS != 1 {
		t.Fatalf("unknown digest was not counted as a cache miss: %+v", server.Snapshot())
	}
}

func TestPackedDownloadIsMeasured(t *testing.T) {
	backend := newMemoryBackend()
	data := []byte("persisted")
	hash := digest(data)
	seed := testServer(t, backend, nil)
	if response := putCacheObject(seed, "/cas/"+hash, data); response.Code != http.StatusNoContent {
		t.Fatalf("seed PUT = %d", response.Code)
	}
	closePackedServer(t, seed)
	server := testServer(t, backend, nil)
	response := readCacheObject(server, "/cas/"+hash)
	if response.Code != http.StatusOK || response.Body.String() != string(data) {
		t.Fatalf("status/body = %d/%q", response.Code, response.Body.String())
	}
	if server.Snapshot().PackDownloads != 1 {
		t.Fatalf("packed download was not counted: %+v", server.Snapshot())
	}
}

func TestShutdownToken(t *testing.T) {
	called := make(chan struct{}, 1)
	server := testServer(t, newMemoryBackend(), func(cfg *Config) {
		cfg.ShutdownToken = "secret"
		cfg.Shutdown = func() { called <- struct{}{} }
	})
	request := httptest.NewRequest(http.MethodPost, "/shutdown", nil)
	request.Header.Set("X-Shutdown-Token", "wrong")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("wrong token status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/shutdown", nil)
	request.Header.Set("X-Shutdown-Token", "secret")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("correct token status = %d", response.Code)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("shutdown callback was not called")
	}
}

func TestNewRejectsUnsafeConfiguration(t *testing.T) {
	_, err := New(Config{
		Backend:          newMemoryBackend(),
		CacheDir:         t.TempDir(),
		KeyPrefix:        "../unsafe",
		MaxBlobSize:      1,
		MaxConcurrent:    1,
		UploadsPerMinute: 199,
		BackendTimeout:   time.Second,
	})
	if err == nil {
		t.Fatal("unsafe key prefix accepted")
	}
}

func TestNewRequiresManifestCatalog(t *testing.T) {
	_, err := New(Config{
		Backend:           newMemoryBackend(),
		CacheDir:          t.TempDir(),
		KeyPrefix:         "test-v1",
		PackSize:          1024,
		PackFlushInterval: time.Second,
		MaxManifests:      1,
		MaxBlobSize:       1024,
		MaxConcurrent:     1,
		UploadsPerMinute:  1,
		BackendTimeout:    time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "manifest catalog is required") {
		t.Fatalf("New error = %v, want manifest catalog required", err)
	}
}

func TestIntervalLimiterHonorsCancellation(t *testing.T) {
	limiter := newIntervalLimiter(1)
	if waited, err := limiter.wait(context.Background()); err != nil || waited {
		t.Fatalf("first wait = %t, %v", waited, err)
	}
	limiter.mu.Lock()
	firstStart := limiter.last
	limiter.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if waited, err := limiter.wait(ctx); !waited || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second wait = %t, %v", waited, err)
	}
	limiter.mu.Lock()
	last := limiter.last
	limiter.mu.Unlock()
	if !last.Equal(firstStart) {
		t.Fatalf("cancelled wait changed the last start time: %s, want %s", last, firstStart)
	}
}

func TestIntervalLimiterSpacesActualStarts(t *testing.T) {
	limiter := &intervalLimiter{interval: 20 * time.Millisecond}
	if delay := limiter.claimStart(); delay != 0 {
		t.Fatalf("first start delay = %s, want none", delay)
	}
	delay := limiter.claimStart()
	if delay <= 0 {
		t.Fatalf("second start delay = %s, want a positive delay", delay)
	}
	time.Sleep(delay)
	if delay := limiter.claimStart(); delay != 0 {
		t.Fatalf("start delay after waiting = %s, want none", delay)
	}
}

func TestThrottledPublishDoesNotHoldBackendSlot(t *testing.T) {
	server := testServer(t, newMemoryBackend(), func(cfg *Config) {
		cfg.MaxConcurrent = 1
	})
	server.limiter.mu.Lock()
	server.limiter.last = time.Now()
	server.limiter.mu.Unlock()

	file, err := os.CreateTemp(server.cfg.CacheDir, "test-publish-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}()

	ctx, cancel := context.WithCancel(context.Background())
	published := make(chan error, 1)
	go func() {
		published <- server.publish(ctx, "test-key", file, 0)
	}()
	defer cancel()

	time.Sleep(10 * time.Millisecond)

	readCtx, stopWaiting := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stopWaiting()
	if err := server.acquire(readCtx); err != nil {
		t.Fatalf("rate-limited publish occupied the backend slot: %v", err)
	}
	server.release()

	cancel()
	if err := <-published; !errors.Is(err, context.Canceled) {
		t.Fatalf("publish error = %v, want context.Canceled", err)
	}
}

func TestSafeErrorRedactsURLsAndTokens(t *testing.T) {
	raw := "request https://blob.example.invalid/object?sig=secret failed with " +
		"aaaaaaaaaaaaaaaa.bbbbbbbbbbbbbbbb.cccccccccccccccc"
	got := safeError(errors.New(raw))
	if strings.Contains(got, "secret") || strings.Contains(got, "aaaaaaaa") {
		t.Fatalf("secret was not redacted: %q", got)
	}
}
