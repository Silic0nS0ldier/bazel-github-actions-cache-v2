package server

import (
	"bytes"
	"context"
	"net/http"
	"sort"
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
		cfg.StorageMode = "packs"
		cfg.Catalog = memoryCatalog{backend: backend}
		cfg.MaxBlobSize = maxBlobSize
		cfg.PackSize = 1024
		cfg.PackFlushInterval = time.Hour
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

func TestPackedStoreRoundTripCommitsPackBeforeManifest(t *testing.T) {
	backend := newMemoryBackend()
	seed := testPackedServer(t, backend)
	output := []byte("packed output")
	outputReference := referenceFor(output)
	actionResult := bytesField(2, outputFileProto(outputReference, nil))
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
		cfg.StorageMode = "packs"
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
		cfg.StorageMode = "packs"
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
		cfg.StorageMode = "packs"
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

func TestPackedStoreRejectsConflictingActionResults(t *testing.T) {
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

	restore := testPackedServer(t, backend)
	defer closePackedServer(t, restore)
	if response := readCacheObject(restore, "/ac/"+actionDigest); response.Code != http.StatusNotFound {
		t.Fatalf("conflicting action result = %d", response.Code)
	}
	if restore.Snapshot().ActionDigestConflicts != 1 {
		t.Fatalf("conflict was not reported: %+v", restore.Snapshot())
	}
}
