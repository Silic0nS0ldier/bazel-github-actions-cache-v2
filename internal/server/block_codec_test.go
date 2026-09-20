package server

import (
	"bytes"
	"crypto/rand"
	"net/http"
	"strings"
	"testing"
	"time"
)

func testPackedServerWithCompression(t *testing.T, backend *memoryBackend, maxBlobSize int64, enabled bool) *Server {
	t.Helper()
	return testServer(t, backend, func(cfg *Config) {
		cfg.StorageMode = "packs"
		cfg.Catalog = memoryCatalog{backend: backend}
		cfg.MaxBlobSize = maxBlobSize
		cfg.PackSize = maxBlobSize
		cfg.PackFlushInterval = time.Hour
		cfg.PackCompression = enabled
		cfg.MaxManifests = 100
	})
}

func packBytes(t *testing.T, backend *memoryBackend) int {
	t.Helper()
	backend.mu.Lock()
	defer backend.mu.Unlock()
	total := 0
	for key, value := range backend.objects {
		if strings.Contains(key, "-car-pack-v1-") {
			total += len(value)
		}
	}
	return total
}

func TestPackedBlocksAreCompressedAndRestoredExactly(t *testing.T) {
	const size = 256 << 10
	payload := bytes.Repeat([]byte("bazel remote cache payload; "), size/28)
	backend := newMemoryBackend()
	seed := testPackedServerWithCompression(t, backend, 8<<20, true)
	if response := putCacheObject(seed, "/cas/"+digest(payload), payload); response.Code != http.StatusNoContent {
		t.Fatalf("CAS PUT = %d", response.Code)
	}
	closePackedServer(t, seed)

	stats := seed.Snapshot()
	if stats.CompressedBlocks != 1 || stats.CompressionSavedBytes == 0 {
		t.Fatalf("block was not compressed: %+v", stats)
	}
	if stored := packBytes(t, backend); stored >= len(payload)/2 {
		t.Fatalf("pack holds %d bytes for a %d byte payload", stored, len(payload))
	}

	restore := testPackedServerWithCompression(t, backend, 8<<20, true)
	defer closePackedServer(t, restore)
	response := readCacheObject(restore, "/cas/"+digest(payload))
	if response.Code != http.StatusOK {
		t.Fatalf("CAS GET = %d", response.Code)
	}
	if !bytes.Equal(response.Body.Bytes(), payload) {
		t.Fatal("restored payload differs from the published payload")
	}
}

func TestPackedIncompressibleBlocksAreStoredVerbatim(t *testing.T) {
	payload := make([]byte, 128<<10)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	backend := newMemoryBackend()
	seed := testPackedServerWithCompression(t, backend, 8<<20, true)
	if response := putCacheObject(seed, "/cas/"+digest(payload), payload); response.Code != http.StatusNoContent {
		t.Fatalf("CAS PUT = %d", response.Code)
	}
	closePackedServer(t, seed)
	if stats := seed.Snapshot(); stats.CompressedBlocks != 0 {
		t.Fatalf("random data was stored compressed: %+v", stats)
	}

	restore := testPackedServerWithCompression(t, backend, 8<<20, true)
	defer closePackedServer(t, restore)
	response := readCacheObject(restore, "/cas/"+digest(payload))
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), payload) {
		t.Fatalf("CAS GET = %d", response.Code)
	}
}

func TestPackedCompressedBlocksAreReadableWithoutCompressionEnabled(t *testing.T) {
	payload := bytes.Repeat([]byte("readable by a reader that never compresses; "), 4096)
	backend := newMemoryBackend()
	seed := testPackedServerWithCompression(t, backend, 8<<20, true)
	if response := putCacheObject(seed, "/cas/"+digest(payload), payload); response.Code != http.StatusNoContent {
		t.Fatalf("CAS PUT = %d", response.Code)
	}
	closePackedServer(t, seed)

	// Disabling compression must only stop writing it, never stop reading it.
	restore := testPackedServerWithCompression(t, backend, 8<<20, false)
	defer closePackedServer(t, restore)
	response := readCacheObject(restore, "/cas/"+digest(payload))
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), payload) {
		t.Fatalf("CAS GET = %d", response.Code)
	}
}

func TestPackedTamperedCompressedBlockIsACacheMiss(t *testing.T) {
	payload := bytes.Repeat([]byte("tamper detection through the block CID; "), 4096)
	backend := newMemoryBackend()
	seed := testPackedServerWithCompression(t, backend, 8<<20, true)
	if response := putCacheObject(seed, "/cas/"+digest(payload), payload); response.Code != http.StatusNoContent {
		t.Fatalf("CAS PUT = %d", response.Code)
	}
	closePackedServer(t, seed)

	backend.mu.Lock()
	for key, value := range backend.objects {
		if strings.Contains(key, "-car-pack-v1-") {
			value[len(value)-1] ^= 0xff
		}
	}
	backend.mu.Unlock()

	restore := testPackedServerWithCompression(t, backend, 8<<20, true)
	defer closePackedServer(t, restore)
	if response := readCacheObject(restore, "/cas/"+digest(payload)); response.Code != http.StatusNotFound {
		t.Fatalf("tampered pack = %d", response.Code)
	}
}

func TestBlockCodecSkipsSmallAndIncompressibleInput(t *testing.T) {
	codec, err := newBlockCodec(true, defaultPackCompressionLevel, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer codec.close()

	small := bytes.Repeat([]byte("a"), minCompressibleBlockSize-1)
	if _, encoding := codec.encode(small); encoding != blockEncodingRaw {
		t.Fatalf("small input encoding = %q", encoding)
	}
	random := make([]byte, compressionProbeThreshold+1)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	if _, encoding := codec.encode(random); encoding != blockEncodingRaw {
		t.Fatalf("incompressible input encoding = %q", encoding)
	}

	compressible := bytes.Repeat([]byte("compress me please "), 1024)
	stored, encoding := codec.encode(compressible)
	if encoding != blockEncodingZstd || len(stored) >= len(compressible) {
		t.Fatalf("compressible input encoding = %q, %d bytes", encoding, len(stored))
	}
	plain, err := codec.decode(stored, encoding, int64(len(compressible)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain, compressible) {
		t.Fatal("round trip changed the block")
	}
	if _, err := codec.decode(stored, encoding, int64(len(compressible))-1); err == nil {
		t.Fatal("a size disagreeing with the manifest was accepted")
	}
	if _, err := codec.decode(stored, "brotli", int64(len(compressible))); err == nil {
		t.Fatal("an unknown encoding was accepted")
	}
}

func TestManifestFormatOneIsStillReadable(t *testing.T) {
	casDigest := digest([]byte("legacy"))
	casCID, err := rawCIDForDigest(casDigest)
	if err != nil {
		t.Fatal(err)
	}
	packID := digest([]byte("legacy pack"))
	legacy := manifest{
		Version: minManifestFormatVersion,
		Parents: []string{},
		Packs:   []packDescriptor{{ID: packID, Key: packKeyFor("test", packID), Size: 10}},
		CAS: []manifestObject{{
			Digest: casDigest,
			CID:    casCID.String(),
			Block:  casCID.String(),
			PackID: packID,
			Size:   6,
		}},
	}
	encoded, manifestCID, err := encodeManifest(legacy)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeManifest(encoded, manifestCID)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.CAS) != 1 {
		t.Fatalf("decoded %d CAS entries", len(decoded.CAS))
	}
	if decoded.CAS[0].Encoding != blockEncodingRaw || decoded.CAS[0].Block != casCID.String() {
		t.Fatalf("format 1 entry did not default to a verbatim block: %+v", decoded.CAS[0])
	}
}
