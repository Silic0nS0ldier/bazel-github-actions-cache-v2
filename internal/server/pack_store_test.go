package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/cache"
)

type memoryCatalog struct {
	backend *memoryBackend
}

func (c memoryCatalog) List(_ context.Context, prefix string) ([]string, error) {
	c.backend.mu.Lock()
	defer c.backend.mu.Unlock()
	keys := make([]string, 0)
	for key := range c.backend.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

var _ cache.Catalog = memoryCatalog{}

// phantomCatalog reports keys the backend cannot serve, as GitHub's REST
// listing does for packs outside the running job's cache scope.
type phantomCatalog struct {
	memoryCatalog
	phantom []string
}

func (c phantomCatalog) List(ctx context.Context, prefix string) ([]string, error) {
	keys, err := c.memoryCatalog.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	for _, key := range c.phantom {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func testPackedServer(t *testing.T, backend *memoryBackend) *Server {
	return testPackedServerWithMaxBlob(t, backend, 1024)
}

func testPackedServerWithMaxBlob(t *testing.T, backend *memoryBackend, maxBlobSize int64) *Server {
	t.Helper()
	return testServer(t, backend, func(cfg *Config) {
		cfg.Catalog = memoryCatalog{backend: backend}
		cfg.MaxBlobSize = maxBlobSize
		cfg.PackSize = 1024
		cfg.PackFlushInterval = time.Hour
		cfg.PackCompression = true
		cfg.MaxManifests = 100
	})
}

func closePackedServer(t *testing.T, server *Server) {
	t.Helper()
	context, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Close(context); err != nil {
		t.Fatal(err)
	}
}

func cacheFilesWithPrefix(t *testing.T, dir, prefix string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			paths = append(paths, entry.Name())
		}
	}
	return paths
}

func TestPackedStoreRoundTripCommitsPackBeforeManifest(t *testing.T) {
	backend := newMemoryBackend()
	seed := testPackedServer(t, backend)
	output := []byte("packed output")
	outputReference := referenceFor(output)
	actionResult := actionResultProto(t, outputReference)
	actionDigest := digest([]byte("packed action"))
	if response := putCacheObject(seed, "/cas/"+outputReference.hash, output); response.Code != http.StatusNoContent {
		t.Fatalf("CAS PUT = %d", response.Code)
	}
	if response := putCacheObject(seed, "/ac/"+actionDigest, actionResult); response.Code != http.StatusNoContent {
		t.Fatalf("AC PUT = %d", response.Code)
	}
	closePackedServer(t, seed)
	stats := seed.Snapshot()
	if stats.PackUploads != 1 || stats.ManifestUploads != 1 || stats.Uploads != 2 {
		t.Fatalf("unexpected publication stats: %+v", stats)
	}

	restore := testPackedServer(t, backend)
	defer closePackedServer(t, restore)
	if response := readCacheObject(restore, "/ac/"+actionDigest); response.Code != http.StatusOK {
		t.Fatalf("AC GET = %d, body = %s", response.Code, response.Body.String())
	}
	if response := readCacheObject(restore, "/cas/"+outputReference.hash); response.Code != http.StatusOK || response.Body.String() != string(output) {
		t.Fatalf("CAS GET = %d, body = %q", response.Code, response.Body.String())
	}
	stats = restore.Snapshot()
	if stats.PackDownloads != 1 || stats.Hits != 2 || stats.ValidatedActionResults != 1 {
		t.Fatalf("unexpected restore stats: %+v", stats)
	}
}

func TestPackedStoreIndexesRawCASWithoutRedundantBlockID(t *testing.T) {
	server := testPackedServer(t, newMemoryBackend())
	payload := []byte("raw object")
	objectDigest := digest(payload)
	if response := putCacheObject(server, "/cas/"+objectDigest, payload); response.Code != http.StatusNoContent {
		t.Fatalf("CAS PUT = %d, body = %s", response.Code, response.Body.String())
	}
	if err := server.packs.flush(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	server.packs.mu.Lock()
	entry, found := server.packs.cas[objectDigest]
	server.packs.mu.Unlock()
	if !found {
		t.Fatal("flushed CAS object is absent from the packed index")
	}
	if entry.block != "" || entry.encoding != blockEncodingRaw || entry.size != int64(len(payload)) {
		t.Fatalf("raw CAS index entry retains redundant metadata: %+v", entry)
	}
	if response := readCacheObject(server, "/cas/"+objectDigest); response.Code != http.StatusOK || response.Body.String() != string(payload) {
		t.Fatalf("CAS GET = %d, body = %q", response.Code, response.Body.String())
	}
	closePackedServer(t, server)
}

func TestPackedStoreFlushReplacesUploadSpoolsWithLocalPack(t *testing.T) {
	server := testPackedServer(t, newMemoryBackend())
	payload := []byte("keep only the committed pack")
	digest := digest(payload)
	if response := putCacheObject(server, "/cas/"+digest, payload); response.Code != http.StatusNoContent {
		t.Fatalf("CAS PUT = %d, body = %s", response.Code, response.Body.String())
	}
	if got := len(cacheFilesWithPrefix(t, server.cfg.CacheDir, "upload-")); got != 1 {
		t.Fatalf("upload spools before flush = %d, want 1", got)
	}

	if err := server.packs.flush(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := cacheFilesWithPrefix(t, server.cfg.CacheDir, "upload-"); len(got) != 0 {
		t.Fatalf("flush left upload spools: %v", got)
	}
	if got := cacheFilesWithPrefix(t, server.cfg.CacheDir, "manifest-"); len(got) != 0 {
		t.Fatalf("flush left manifest spools: %v", got)
	}
	if got := len(cacheFilesWithPrefix(t, server.cfg.CacheDir, "pack-")); got != 1 {
		t.Fatalf("local packs after flush = %d, want 1", got)
	}
	if response := readCacheObject(server, "/cas/"+digest); response.Code != http.StatusOK || response.Body.String() != string(payload) {
		t.Fatalf("CAS GET after flush = %d, body = %q", response.Code, response.Body.String())
	}
	if stats := server.Snapshot(); stats.PackDownloads != 0 {
		t.Fatalf("newly flushed pack was downloaded again: %+v", stats)
	}
	if response := putCacheObject(server, "/cas/"+digest, payload); response.Code != http.StatusNoContent {
		t.Fatalf("duplicate CAS PUT = %d, body = %s", response.Code, response.Body.String())
	}
	if got := cacheFilesWithPrefix(t, server.cfg.CacheDir, "upload-"); len(got) != 0 {
		t.Fatalf("duplicate of a committed CAS left upload spools: %v", got)
	}

	closePackedServer(t, server)
	if got := cacheFilesWithPrefix(t, server.cfg.CacheDir, "pack-"); len(got) != 0 {
		t.Fatalf("close left local pack files: %v", got)
	}
}

func TestPackedStoreFailedDownloadRemovesSpool(t *testing.T) {
	server := testPackedServer(t, newMemoryBackend())
	defer closePackedServer(t, server)

	_, err := server.packs.downloadPack(context.Background(), packDescriptor{
		ID:  strings.Repeat("0", 64),
		Key: "missing-pack",
	})
	if err == nil {
		t.Fatal("download of a missing pack succeeded")
	}
	if got := cacheFilesWithPrefix(t, server.cfg.CacheDir, "pack-download-"); len(got) != 0 {
		t.Fatalf("failed download left spool files: %v", got)
	}
}

func TestPackedStoreFailedFlushRemovesTemporaryPack(t *testing.T) {
	backend := newMemoryBackend()
	server := testPackedServer(t, backend)
	payload := []byte("retry after a backend failure")
	if response := putCacheObject(server, "/cas/"+digest(payload), payload); response.Code != http.StatusNoContent {
		t.Fatalf("CAS PUT = %d, body = %s", response.Code, response.Body.String())
	}
	backend.mu.Lock()
	backend.saveErr = errors.New("backend unavailable")
	backend.mu.Unlock()

	if err := server.packs.flush(context.Background(), true); err == nil {
		t.Fatal("flush succeeded while the backend was unavailable")
	}
	if got := cacheFilesWithPrefix(t, server.cfg.CacheDir, "pack-"); len(got) != 0 {
		t.Fatalf("failed flush left pack files: %v", got)
	}
	if got := cacheFilesWithPrefix(t, server.cfg.CacheDir, "manifest-"); len(got) != 0 {
		t.Fatalf("failed flush left manifest files: %v", got)
	}
	if got := len(cacheFilesWithPrefix(t, server.cfg.CacheDir, "upload-")); got != 1 {
		t.Fatalf("failed flush discarded pending upload spools: got %d, want 1", got)
	}

	backend.mu.Lock()
	backend.saveErr = nil
	backend.mu.Unlock()
	closePackedServer(t, server)
}

func TestPackedStoreRestoresCASLargerThanCARDefaultSectionLimit(t *testing.T) {
	const payloadSize = (8 << 20) + 1
	const maxBlobSize = payloadSize + 1024
	backend := newMemoryBackend()
	seed := testPackedServerWithMaxBlob(t, backend, maxBlobSize)
	payload := bytes.Repeat([]byte{0xa5}, payloadSize)
	digest := digest(payload)
	if response := putCacheObject(seed, "/cas/"+digest, payload); response.Code != http.StatusNoContent {
		t.Fatalf("CAS PUT = %d, body = %s", response.Code, response.Body.String())
	}
	closePackedServer(t, seed)

	restore := testPackedServerWithMaxBlob(t, backend, maxBlobSize)
	defer closePackedServer(t, restore)
	response := readCacheObject(restore, "/cas/"+digest)
	if response.Code != http.StatusOK {
		t.Fatalf("CAS GET = %d, body = %s", response.Code, response.Body.String())
	}
	if !bytes.Equal(response.Body.Bytes(), payload) {
		t.Fatal("restored CAS payload differs from the published payload")
	}
}

func TestPackedStoreMergesConcurrentManifestHeads(t *testing.T) {
	backend := newMemoryBackend()
	writerA := testPackedServer(t, backend)
	writerB := testPackedServer(t, backend)

	for _, test := range []struct {
		server *Server
		body   []byte
	}{
		{writerA, []byte("writer A")},
		{writerB, []byte("writer B")},
	} {
		reference := referenceFor(test.body)
		if response := putCacheObject(test.server, "/cas/"+reference.hash, test.body); response.Code != http.StatusNoContent {
			t.Fatalf("CAS PUT = %d", response.Code)
		}
	}
	closePackedServer(t, writerA)
	closePackedServer(t, writerB)

	restore := testPackedServer(t, backend)
	defer closePackedServer(t, restore)
	for _, body := range [][]byte{[]byte("writer A"), []byte("writer B")} {
		response := readCacheObject(restore, "/cas/"+digest(body))
		if response.Code != http.StatusOK || response.Body.String() != string(body) {
			t.Fatalf("concurrent CAS restore = %d/%q", response.Code, response.Body.String())
		}
	}
	if restore.Snapshot().ManifestsDiscovered != 2 {
		t.Fatalf("manifest heads were not both discovered: %+v", restore.Snapshot())
	}
}

func TestPackedStoreLeavesOrphanedManifestsUnread(t *testing.T) {
	backend := newMemoryBackend()
	seed := testPackedServer(t, backend)
	actionDigest := digest([]byte("action"))
	if response := putCacheObject(seed, "/ac/"+actionDigest, []byte{0x08, 0x01}); response.Code != http.StatusNoContent {
		t.Fatalf("AC PUT = %d", response.Code)
	}
	closePackedServer(t, seed)

	// Emulate GitHub evicting the large pack while its small manifest survives.
	backend.mu.Lock()
	for key := range backend.objects {
		if strings.Contains(key, "-car-pack-v1-") {
			delete(backend.objects, key)
		}
	}
	backend.mu.Unlock()

	loadsBeforeDiscovery := backend.loadCount()
	restore := testPackedServer(t, backend)
	defer closePackedServer(t, restore)
	if backend.loadCount() != loadsBeforeDiscovery {
		t.Fatal("an orphaned manifest was downloaded, which would renew its lifetime")
	}
	if response := readCacheObject(restore, "/ac/"+actionDigest); response.Code != http.StatusNotFound {
		t.Fatalf("orphaned action result = %d", response.Code)
	}
	stats := restore.Snapshot()
	if stats.ManifestsDiscovered != 1 || stats.ManifestsOrphaned != 1 || stats.BackendLoadErrors != 0 {
		t.Fatalf("unexpected orphaned-manifest stats: %+v", stats)
	}
}

func TestPackedStoreRetriesAnUnrestorablePackOnlyOnce(t *testing.T) {
	backend := newMemoryBackend()
	seed := testPackedServer(t, backend)
	body := []byte("out of scope payload")
	if response := putCacheObject(seed, "/cas/"+digest(body), body); response.Code != http.StatusNoContent {
		t.Fatalf("CAS PUT = %d", response.Code)
	}
	closePackedServer(t, seed)

	// A pack listed for the repository can still be outside this job's cache
	// scope, which only a failed download reveals.
	var packKey string
	backend.mu.Lock()
	for key := range backend.objects {
		if strings.Contains(key, "-car-pack-v1-") {
			packKey = key
		}
	}
	delete(backend.objects, packKey)
	backend.mu.Unlock()
	if packKey == "" {
		t.Fatal("no pack was published")
	}

	restore := testServer(t, backend, func(cfg *Config) {
		cfg.Catalog = phantomCatalog{memoryCatalog{backend: backend}, []string{packKey}}
		cfg.PackSize = 1024
		cfg.PackFlushInterval = time.Hour
		cfg.MaxManifests = 100
	})
	defer closePackedServer(t, restore)
	for attempt := 0; attempt < 2; attempt++ {
		if response := readCacheObject(restore, "/cas/"+digest(body)); response.Code != http.StatusNotFound {
			t.Fatalf("attempt %d = %d", attempt, response.Code)
		}
	}
	stats := restore.Snapshot()
	if stats.ManifestsOrphaned != 0 || stats.PackLoadsSkipped != 1 {
		t.Fatalf("an unrestorable pack was not memoized: %+v", stats)
	}
}

func TestPackedStoreDiscoversPacksAndManifests(t *testing.T) {
	backend := newMemoryBackend()
	seed := testPackedServer(t, backend)
	body := []byte("listed payload")
	if response := putCacheObject(seed, "/cas/"+digest(body), body); response.Code != http.StatusNoContent {
		t.Fatalf("CAS PUT = %d", response.Code)
	}
	closePackedServer(t, seed)

	restore := testPackedServer(t, backend)
	defer closePackedServer(t, restore)
	stats := restore.Snapshot()
	if stats.PacksDiscovered != 1 || stats.ManifestsDiscovered != 1 {
		t.Fatalf("unexpected discovery stats: %+v", stats)
	}
	if response := readCacheObject(restore, "/cas/"+digest(body)); response.Code != http.StatusOK {
		t.Fatalf("CAS GET = %d", response.Code)
	}
}

func TestPackedStoreLimitsDownloadedManifestsWithoutCountingPacks(t *testing.T) {
	backend := newMemoryBackend()
	payloads := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	for _, body := range payloads {
		writer := testPackedServer(t, backend)
		if response := putCacheObject(writer, "/cas/"+digest(body), body); response.Code != http.StatusNoContent {
			t.Fatalf("CAS PUT = %d", response.Code)
		}
		closePackedServer(t, writer)
	}

	restore := testServer(t, backend, func(cfg *Config) {
		cfg.Catalog = memoryCatalog{backend: backend}
		cfg.PackSize = 1024
		cfg.PackFlushInterval = time.Hour
		cfg.MaxManifests = 2
	})
	defer closePackedServer(t, restore)
	stats := restore.Snapshot()
	if stats.PacksDiscovered != 3 || stats.ManifestsDiscovered != 3 || stats.ManifestsSkipped != 1 {
		t.Fatalf("unexpected bounded-discovery stats: %+v", stats)
	}
	for _, body := range payloads {
		readCacheObject(restore, "/cas/"+digest(body))
	}
	stats = restore.Snapshot()
	if stats.Hits != 2 || stats.Misses != 1 {
		t.Fatalf("max-manifests should bound manifest reads, not the pack listing: %+v", stats)
	}
}

func TestPackedStoreDoesNotSpendTheManifestLimitOnOrphans(t *testing.T) {
	backend := newMemoryBackend()
	payloads := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	packKeys := make(map[string]string, len(payloads))
	known := make(map[string]bool)
	for _, body := range payloads {
		writer := testPackedServer(t, backend)
		if response := putCacheObject(writer, "/cas/"+digest(body), body); response.Code != http.StatusNoContent {
			t.Fatalf("CAS PUT = %d", response.Code)
		}
		closePackedServer(t, writer)
		backend.mu.Lock()
		for key := range backend.objects {
			if strings.Contains(key, "-car-pack-v1-") && !known[key] {
				known[key] = true
				packKeys[string(body)] = key
			}
		}
		backend.mu.Unlock()
	}

	// Two of the three manifests are orphaned. A budget of one must be spent on
	// the manifest that can still produce a hit.
	backend.mu.Lock()
	delete(backend.objects, packKeys["one"])
	delete(backend.objects, packKeys["two"])
	backend.mu.Unlock()

	restore := testServer(t, backend, func(cfg *Config) {
		cfg.Catalog = memoryCatalog{backend: backend}
		cfg.PackSize = 1024
		cfg.PackFlushInterval = time.Hour
		cfg.MaxManifests = 1
	})
	defer closePackedServer(t, restore)
	stats := restore.Snapshot()
	if stats.ManifestsDiscovered != 3 || stats.ManifestsOrphaned != 2 || stats.ManifestsSkipped != 0 {
		t.Fatalf("orphans consumed the manifest budget: %+v", stats)
	}
	if response := readCacheObject(restore, "/cas/"+digest([]byte("three"))); response.Code != http.StatusOK {
		t.Fatalf("live manifest was not read: %d", response.Code)
	}
	if response := readCacheObject(restore, "/cas/"+digest([]byte("one"))); response.Code != http.StatusNotFound {
		t.Fatalf("orphaned mapping = %d", response.Code)
	}
}

func TestPackedStorePresenceCheckRenewsPackWithoutDownloadingIt(t *testing.T) {
	backend := newMemoryBackend()
	seed := testPackedServer(t, backend)
	payloads := [][]byte{[]byte("presence one"), []byte("presence two")}
	for _, body := range payloads {
		if response := putCacheObject(seed, "/cas/"+digest(body), body); response.Code != http.StatusNoContent {
			t.Fatalf("CAS PUT = %d", response.Code)
		}
	}
	closePackedServer(t, seed)

	restore := testPackedServer(t, backend)
	loadsAfterDiscovery := backend.loadCount()
	for _, body := range payloads {
		response := headCacheObject(restore, "/cas/"+digest(body))
		if response.Code != http.StatusOK {
			t.Fatalf("HEAD = %d", response.Code)
		}
		if got := response.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
			t.Fatalf("Content-Length = %q", got)
		}
		if response.Body.Len() != 0 {
			t.Fatal("HEAD returned a body")
		}
	}
	if backend.loadCount() != loadsAfterDiscovery {
		t.Fatal("a presence check restored the pack")
	}
	if backend.existsCount() != 0 {
		t.Fatal("renewal was issued immediately instead of being batched")
	}

	closePackedServer(t, restore)
	if backend.existsCount() != 1 {
		t.Fatalf("expected one renewal for the shared pack, got %d", backend.existsCount())
	}
	if stats := restore.Snapshot(); stats.PackRenewals != 1 || stats.PackDownloads != 0 {
		t.Fatalf("unexpected renewal stats: %+v", stats)
	}
}

func TestPackedStoreSkipsRenewalWhenThePackIsDownloaded(t *testing.T) {
	backend := newMemoryBackend()
	seed := testPackedServer(t, backend)
	body := []byte("downloaded soon after")
	if response := putCacheObject(seed, "/cas/"+digest(body), body); response.Code != http.StatusNoContent {
		t.Fatalf("CAS PUT = %d", response.Code)
	}
	closePackedServer(t, seed)

	restore := testPackedServer(t, backend)
	if response := headCacheObject(restore, "/cas/"+digest(body)); response.Code != http.StatusOK {
		t.Fatalf("HEAD = %d", response.Code)
	}
	if response := readCacheObject(restore, "/cas/"+digest(body)); response.Code != http.StatusOK {
		t.Fatalf("GET = %d", response.Code)
	}
	closePackedServer(t, restore)
	if backend.existsCount() != 0 {
		t.Fatalf("the download should have covered the renewal, got %d calls", backend.existsCount())
	}
	if stats := restore.Snapshot(); stats.PackRenewals != 0 || stats.PackDownloads != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

// seedPackedClosure publishes an output blob and then, in a separate job, an
// action result referencing it, so the two land in different packs.
func seedPackedClosure(t *testing.T, backend *memoryBackend, output []byte, actionDigest string) string {
	t.Helper()
	reference := referenceFor(output)
	blobWriter := testPackedServer(t, backend)
	if response := putCacheObject(blobWriter, "/cas/"+reference.hash, output); response.Code != http.StatusNoContent {
		t.Fatalf("CAS PUT = %d", response.Code)
	}
	closePackedServer(t, blobWriter)
	var blobPackKey string
	backend.mu.Lock()
	for key := range backend.objects {
		if strings.Contains(key, "-car-pack-v1-") {
			blobPackKey = key
		}
	}
	backend.mu.Unlock()
	if blobPackKey == "" {
		t.Fatal("no pack was published for the output")
	}

	actionWriter := testPackedServer(t, backend)
	if response := putCacheObject(actionWriter, "/ac/"+actionDigest, actionResultProto(t, reference)); response.Code != http.StatusNoContent {
		t.Fatalf("AC PUT = %d", response.Code)
	}
	closePackedServer(t, actionWriter)
	return blobPackKey
}

func TestPackedStoreRefusesActionResultWithAnEvictedOutputPack(t *testing.T) {
	backend := newMemoryBackend()
	actionDigest := digest([]byte("action with evicted output"))
	blobPackKey := seedPackedClosure(t, backend, []byte("evicted output"), actionDigest)

	backend.mu.Lock()
	delete(backend.objects, blobPackKey)
	backend.mu.Unlock()

	restore := testPackedServer(t, backend)
	defer closePackedServer(t, restore)
	if response := readCacheObject(restore, "/ac/"+actionDigest); response.Code != http.StatusNotFound {
		t.Fatalf("action result with an evicted output = %d", response.Code)
	}
	if stats := restore.Snapshot(); stats.IncompleteActionResults != 1 {
		t.Fatalf("closure was not checked before serving: %+v", stats)
	}
}

func TestPackedStoreActionResultHitRenewsItsClosurePacks(t *testing.T) {
	backend := newMemoryBackend()
	actionDigest := digest([]byte("action with a remote output"))
	seedPackedClosure(t, backend, []byte("output kept in another pack"), actionDigest)
	// The publishing job presence-checks the output too, so count from here.
	renewalsAfterSeeding := backend.existsCount()

	restore := testPackedServer(t, backend)
	if response := readCacheObject(restore, "/ac/"+actionDigest); response.Code != http.StatusOK {
		t.Fatalf("AC GET = %d", response.Code)
	}
	stats := restore.Snapshot()
	if stats.PackDownloads != 1 || stats.ValidatedActionResults != 1 {
		t.Fatalf("serving an action result should restore only its own pack: %+v", stats)
	}
	if backend.existsCount() != renewalsAfterSeeding {
		t.Fatal("renewal was issued immediately instead of being batched")
	}

	closePackedServer(t, restore)
	if backend.existsCount() != renewalsAfterSeeding+1 {
		t.Fatalf("the closure pack was not renewed, got %d calls", backend.existsCount()-renewalsAfterSeeding)
	}
	if stats := restore.Snapshot(); stats.PackRenewals != 1 {
		t.Fatalf("unexpected renewal stats: %+v", stats)
	}
}

func TestPackedStoreServesOneOfConflictingActionResults(t *testing.T) {
	backend := newMemoryBackend()
	writerA := testPackedServer(t, backend)
	writerB := testPackedServer(t, backend)
	actionDigest := digest([]byte("same action"))
	if response := putCacheObject(writerA, "/ac/"+actionDigest, []byte{0x08, 0x01}); response.Code != http.StatusNoContent {
		t.Fatalf("first AC PUT = %d", response.Code)
	}
	if response := putCacheObject(writerB, "/ac/"+actionDigest, []byte{0x08, 0x02}); response.Code != http.StatusNoContent {
		t.Fatalf("second AC PUT = %d", response.Code)
	}
	closePackedServer(t, writerA)
	closePackedServer(t, writerB)

	var served []byte
	for range 2 {
		restore := testPackedServer(t, backend)
		response := readCacheObject(restore, "/ac/"+actionDigest)
		if response.Code != http.StatusOK {
			t.Fatalf("conflicting action result = %d", response.Code)
		}
		if restore.Snapshot().ActionDigestConflicts != 1 {
			t.Fatalf("conflict was not reported: %+v", restore.Snapshot())
		}
		if served != nil && !bytes.Equal(served, response.Body.Bytes()) {
			t.Fatalf("restores disagree on the served result: %x and %x", served, response.Body.Bytes())
		}
		served = response.Body.Bytes()
		closePackedServer(t, restore)
	}

	servedCID, err := rawCIDForData(served)
	if err != nil {
		t.Fatal(err)
	}
	var inLayout bool
	for _, entry := range readLayout(t, backend).Entries {
		if entry.Kind == "ac" && entry.Digest == actionDigest && entry.CID == servedCID.String() {
			inLayout = true
		}
	}
	if !inLayout {
		t.Fatal("the layout does not resolve the action to the served result")
	}
}

func TestPackedStoreServesReadsWhileAFlushIsUploading(t *testing.T) {
	memory := newMemoryBackend()
	seed := testPackedServer(t, memory)
	restored := []byte("restored while uploading")
	if response := putCacheObject(seed, "/cas/"+digest(restored), restored); response.Code != http.StatusNoContent {
		t.Fatalf("CAS PUT = %d", response.Code)
	}
	closePackedServer(t, seed)

	backend := &blockingSaveBackend{
		memoryBackend: memory,
		started:       make(chan struct{}),
		release:       make(chan struct{}),
	}
	server := testServer(t, backend, func(cfg *Config) {
		cfg.Catalog = memoryCatalog{backend: memory}
		cfg.PackSize = 1024
		cfg.PackFlushInterval = time.Hour
		cfg.MaxManifests = 100
		cfg.BackendTimeout = time.Minute
	})
	staged := []byte("staged for the next pack")
	if response := putCacheObject(server, "/cas/"+digest(staged), staged); response.Code != http.StatusNoContent {
		t.Fatalf("CAS PUT = %d", response.Code)
	}
	flushed := make(chan error, 1)
	go func() { flushed <- server.packs.flush(context.Background(), true) }()
	<-backend.started

	read := make(chan *httptest.ResponseRecorder, 1)
	go func() { read <- readCacheObject(server, "/cas/"+digest(restored)) }()
	select {
	case response := <-read:
		if response.Code != http.StatusOK || response.Body.String() != string(restored) {
			t.Fatalf("CAS GET = %d, body = %q", response.Code, response.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a pack read waited for an unrelated upload")
	}

	close(backend.release)
	if err := <-flushed; err != nil {
		t.Fatal(err)
	}
	closePackedServer(t, server)
}

func TestManifestReadConcurrencyRaisesButNeverLowers(t *testing.T) {
	for _, test := range []struct {
		maxConcurrent int
		want          int
	}{
		{maxConcurrent: 1, want: 16},
		{maxConcurrent: 4, want: 16},
		{maxConcurrent: 64, want: 64},
	} {
		if got := manifestReadConcurrency(test.maxConcurrent); got != test.want {
			t.Errorf("manifestReadConcurrency(%d) = %d, want %d", test.maxConcurrent, got, test.want)
		}
	}
}
