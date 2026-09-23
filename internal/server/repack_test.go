package server

import (
	"context"
	"net/http"
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

	pack, manifestID, err := repacker.Publish(context.Background(), layout.Entries)
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
		pack, _, err := repacker.Publish(context.Background(), []LayoutEntry{entry})
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

	if _, _, err := repacker.Publish(context.Background(), layout.Entries); err == nil {
		t.Fatal("published from a pack that is gone")
	}
}
