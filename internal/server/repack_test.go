package server

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func readLayout(t *testing.T, backend *memoryBackend) Layout {
	t.Helper()
	layout, err := ReadLayout(context.Background(), LayoutOptions{
		Backend:     backend,
		Catalog:     memoryCatalog{backend: backend},
		KeyPrefix:   "test-v1",
		MaxBlobSize: 1 << 20,
		Timeout:     5 * time.Second,
		Warn:        func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(layout.Entries) == 0 {
		t.Fatal("the layout is empty, so anything built on it proves nothing")
	}
	return layout
}

func seedPacked(t *testing.T, backend *memoryBackend, blobs ...[]byte) {
	t.Helper()
	writer := testPackedServer(t, backend)
	for _, blob := range blobs {
		if response := putCacheObject(writer, "/cas/"+digest(blob), blob); response.Code != http.StatusNoContent {
			t.Fatalf("seed PUT = %d", response.Code)
		}
	}
	closePackedServer(t, writer)
}

// dropOriginals removes everything the layout was read from. Packs are content
// addressed, so a rebuild that reproduces one exactly shares its key; the
// caller must not have produced such a pack.
func dropOriginals(t *testing.T, backend *memoryBackend, layout Layout, keep map[string]struct{}) {
	t.Helper()
	for _, original := range layout.Packs {
		if _, protected := keep[original.Key]; protected {
			t.Fatalf("the rebuild reproduced pack %s exactly, so the test proves nothing", original.ID)
		}
		backend.remove(original.Key)
	}
	for _, reference := range layout.Manifests {
		backend.remove(reference.Key)
	}
}

// The layout has to resolve a digest to the same copy a reader is served, or a
// plan built on it would delete the wrong pack.
func TestReadLayoutMatchesWhatAServerResolves(t *testing.T) {
	backend := newMemoryBackend()
	blobs := [][]byte{[]byte("first output"), []byte("second output")}
	seedPacked(t, backend, blobs...)

	layout := readLayout(t, backend)
	if len(layout.Packs) != 1 {
		t.Fatalf("packs = %+v", layout.Packs)
	}
	if len(layout.Entries) != len(blobs) {
		t.Fatalf("entries = %+v", layout.Entries)
	}
	if len(layout.Heads) != 1 {
		t.Fatalf("heads = %v", layout.Heads)
	}

	reader := testPackedServer(t, backend)
	defer closePackedServer(t, reader)
	for _, entry := range layout.Entries {
		if got := reader.packFor(entry.Kind, entry.Digest); got != entry.Pack {
			t.Fatalf("layout puts %s in %s, the server reads it from %s", entry.Digest, entry.Pack, got)
		}
	}
}

// The REST listing reports entries this job's cache scope cannot restore, so a
// layout has to be buildable from the part that is visible.
func TestReadLayoutSkipsManifestsItCannotRestore(t *testing.T) {
	backend := newMemoryBackend()
	blob := []byte("an output this job can see")
	seedPacked(t, backend, blob)

	phantomPack := strings.Repeat("f", 64)
	// A real manifest ID, so the key parses and the load is what fails.
	realManifest := readLayout(t, backend).Manifests[0].ID
	phantomManifest := manifestKeyFor("test-v1", phantomPack, realManifest)
	layout, err := ReadLayout(context.Background(), LayoutOptions{
		Backend: backend,
		Catalog: phantomCatalog{
			memoryCatalog{backend: backend},
			[]string{packKeyFor("test-v1", phantomPack), phantomManifest},
		},
		KeyPrefix:   "test-v1",
		MaxBlobSize: 1 << 20,
		Timeout:     5 * time.Second,
		Warn:        func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if layout.UnreadableManifests != 1 {
		t.Fatalf("unreadable = %d, want 1", layout.UnreadableManifests)
	}
	if len(layout.Entries) != 1 || layout.Entries[0].Digest != digest(blob) {
		t.Fatalf("entries = %+v", layout.Entries)
	}
	// The pack behind an unreadable manifest must not become a deletion
	// candidate, so it must not appear in the layout at all.
	for _, pack := range layout.Packs {
		if pack.ID == phantomPack {
			t.Fatal("a pack whose manifest could not be read entered the layout")
		}
	}
}

// A repacked entry has to be byte-identical and servable from its new pack
// once the original is gone.
func TestRepackerRepublishesEntriesReadably(t *testing.T) {
	backend := newMemoryBackend()
	blobs := [][]byte{[]byte("output that moves"), []byte("output that also moves")}
	// Seeded one per pack, so merging them produces a pack that did not exist.
	for _, blob := range blobs {
		seedPacked(t, backend, blob)
	}
	layout := readLayout(t, backend)
	if len(layout.Packs) != 2 {
		t.Fatalf("packs = %+v, want one per blob", layout.Packs)
	}

	repacker, err := NewRepacker(RepackOptions{
		Backend:     backend,
		CacheDir:    t.TempDir(),
		KeyPrefix:   "test-v1",
		MaxBlobSize: 1 << 20,
		Timeout:     5 * time.Second,
		Parents:     layout.Heads,
		Packs:       layout.Packs,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer repacker.Close()

	pack, manifestID, err := repacker.Publish(context.Background(), layout.Entries, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if manifestID == "" {
		t.Fatal("no manifest was committed")
	}

	// Removing the originals is what forces reads through the rebuilt pack; the
	// merge prefers the lowest pack ID, so leaving them would prove nothing.
	dropOriginals(t, backend, layout, map[string]struct{}{pack.Key: {}})

	reader := testPackedServer(t, backend)
	defer closePackedServer(t, reader)
	for _, blob := range blobs {
		response := readCacheObject(reader, "/cas/"+digest(blob))
		if response.Code != http.StatusOK {
			t.Fatalf("reading %s from the rebuilt pack = %d", digest(blob), response.Code)
		}
		if response.Body.String() != string(blob) {
			t.Fatalf("rebuilt pack served %q, want %q", response.Body.String(), blob)
		}
		if got := reader.packFor("cas", digest(blob)); got != pack.ID {
			t.Fatalf("entry is served from %s, want the rebuilt %s", got, pack.ID)
		}
	}
}

// Splitting into several packs must leave every entry readable from exactly the
// pack the plan put it in.
func TestRepackerSplitsEntriesAcrossPacks(t *testing.T) {
	backend := newMemoryBackend()
	blobs := [][]byte{[]byte("alpha output"), []byte("beta output"), []byte("gamma output")}
	seedPacked(t, backend, blobs...)
	layout := readLayout(t, backend)

	repacker, err := NewRepacker(RepackOptions{
		Backend:     backend,
		CacheDir:    t.TempDir(),
		KeyPrefix:   "test-v1",
		MaxBlobSize: 1 << 20,
		Timeout:     5 * time.Second,
		Parents:     layout.Heads,
		Packs:       layout.Packs,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer repacker.Close()

	placement := make(map[string]string)
	keep := make(map[string]struct{})
	for _, entry := range layout.Entries {
		pack, _, err := repacker.Publish(context.Background(), []LayoutEntry{entry}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		placement[entry.Digest] = pack.ID
		keep[pack.Key] = struct{}{}
	}
	dropOriginals(t, backend, layout, keep)

	reader := testPackedServer(t, backend)
	defer closePackedServer(t, reader)
	for _, blob := range blobs {
		if response := readCacheObject(reader, "/cas/"+digest(blob)); response.Code != http.StatusOK {
			t.Fatalf("reading %s = %d", digest(blob), response.Code)
		}
		if got := reader.packFor("cas", digest(blob)); got != placement[digest(blob)] {
			t.Fatalf("%s is served from %s, want %s", digest(blob), got, placement[digest(blob)])
		}
	}
}

// The optimiser passes a spool path that does not exist yet, because it never
// goes through New, which is what creates one for a cache server.
func TestRepackerCreatesItsSpoolDirectory(t *testing.T) {
	backend := newMemoryBackend()
	blob := []byte("an output to move")
	seedPacked(t, backend, blob)
	layout := readLayout(t, backend)

	spool := filepath.Join(t.TempDir(), "missing", "spool")
	repacker, err := NewRepacker(RepackOptions{
		Backend:     backend,
		CacheDir:    spool,
		KeyPrefix:   "test-v1",
		MaxBlobSize: 1 << 20,
		Timeout:     5 * time.Second,
		Parents:     layout.Heads,
		Packs:       layout.Packs,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer repacker.Close()

	if _, _, err := repacker.Publish(context.Background(), layout.Entries, time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestRepackerRejectsAMissingSourcePack(t *testing.T) {
	backend := newMemoryBackend()
	seedPacked(t, backend, []byte("an output"))
	layout := readLayout(t, backend)
	for _, original := range layout.Packs {
		backend.remove(original.Key)
	}

	repacker, err := NewRepacker(RepackOptions{
		Backend:     backend,
		CacheDir:    t.TempDir(),
		KeyPrefix:   "test-v1",
		MaxBlobSize: 1 << 20,
		Timeout:     5 * time.Second,
		Packs:       layout.Packs,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer repacker.Close()

	if _, _, err := repacker.Publish(context.Background(), layout.Entries, time.Time{}); err == nil {
		t.Fatal("published from a pack that is gone")
	}
}

// Concurrent publications that copy out of the same source pack must share one
// download of it, and all stay readable.
func TestRepackerPublishesConcurrentlyFromSharedSources(t *testing.T) {
	backend := newMemoryBackend()
	blobs := make([][]byte, 0, 8)
	for index := range 8 {
		blobs = append(blobs, []byte("shared source output "+strconv.Itoa(index)))
	}
	seedPacked(t, backend, blobs...)
	layout := readLayout(t, backend)
	if len(layout.Packs) != 1 {
		t.Fatalf("seeded %d packs, want one shared source", len(layout.Packs))
	}

	repacker, err := NewRepacker(RepackOptions{
		Backend:     backend,
		CacheDir:    t.TempDir(),
		KeyPrefix:   "test-v1",
		MaxBlobSize: 1 << 20,
		Timeout:     5 * time.Second,
		Parents:     layout.Heads,
		Packs:       layout.Packs,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer repacker.Close()

	backend.mu.Lock()
	loadsBefore := backend.loads
	backend.mu.Unlock()
	keep := make(map[string]struct{})
	var mu sync.Mutex
	var wait sync.WaitGroup
	errs := make(chan error, len(layout.Entries))
	for _, entry := range layout.Entries {
		wait.Add(1)
		go func() {
			defer wait.Done()
			pack, _, err := repacker.Publish(context.Background(), []LayoutEntry{entry}, time.Time{})
			if err != nil {
				errs <- err
				return
			}
			mu.Lock()
			keep[pack.Key] = struct{}{}
			mu.Unlock()
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	backend.mu.Lock()
	loads := backend.loads - loadsBefore
	backend.mu.Unlock()
	if loads != 1 {
		t.Fatalf("the shared source was downloaded %d times", loads)
	}
	repacker.Release(layout.Packs[0].ID)
	dropOriginals(t, backend, layout, keep)

	reader := testPackedServer(t, backend)
	defer closePackedServer(t, reader)
	for _, blob := range blobs {
		if response := readCacheObject(reader, "/cas/"+digest(blob)); response.Code != http.StatusOK {
			t.Fatalf("reading %s = %d", digest(blob), response.Code)
		}
	}
}

// Reaping needs the manifests whose pack is gone by key, not just a count.
func TestReadLayoutListsManifestsWhosePackIsGone(t *testing.T) {
	backend := newMemoryBackend()
	seedPacked(t, backend, []byte("evicted output"))
	seedPacked(t, backend, []byte("surviving output"))
	before := readLayout(t, backend)
	if len(before.Packs) != 2 {
		t.Fatalf("seeded %d packs, want 2", len(before.Packs))
	}
	evicted := before.Packs[0]
	backend.remove(evicted.Key)

	after := readLayout(t, backend)
	if after.OrphanManifests != 1 || len(after.PacklessManifests) != 1 {
		t.Fatalf("orphaned %d, listed %+v", after.OrphanManifests, after.PacklessManifests)
	}
	if got := after.PacklessManifests[0]; got.Pack != evicted.ID ||
		!strings.HasPrefix(got.Key, ManifestKeyPrefix("test-v1")) {
		t.Fatalf("listed %+v, want the manifest of %s", got, evicted.ID)
	}
}

// Dropping stale entries must not make a pack look newly shaped, or the
// age-gated reshaping would never reach it. The age travels in the manifest.
func TestRepackerCarriesAPackAgeThroughItsManifest(t *testing.T) {
	backend := newMemoryBackend()
	seedPacked(t, backend, []byte("aged output"))
	layout := readLayout(t, backend)
	if !layout.Packs[0].Since.IsZero() {
		t.Fatalf("a server-written pack recorded since %v", layout.Packs[0].Since)
	}

	repacker, err := NewRepacker(RepackOptions{
		Backend:     backend,
		CacheDir:    t.TempDir(),
		KeyPrefix:   "test-v1",
		MaxBlobSize: 1 << 20,
		Timeout:     5 * time.Second,
		Parents:     layout.Heads,
		Packs:       layout.Packs,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer repacker.Close()
	since := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if _, _, err := repacker.Publish(context.Background(), layout.Entries, since); err != nil {
		t.Fatal(err)
	}
	// The rebuild of a one-entry pack reproduces its bytes, so only the old
	// manifest goes.
	for _, reference := range layout.Manifests {
		backend.remove(reference.Key)
	}

	after := readLayout(t, backend)
	if len(after.Packs) != 1 || !after.Packs[0].Since.Equal(since) {
		t.Fatalf("packs = %+v, want one shaped at %v", after.Packs, since)
	}
}
