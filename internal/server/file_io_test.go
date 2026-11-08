package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/google/bytestream"
)

func TestIOGateBoundsConcurrentCalls(t *testing.T) {
	const limit = 3
	gate := newIOGate(limit)
	var active, peak atomic.Int64
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			err := gate.do(t.Context(), func() error {
				now := active.Add(1)
				defer active.Add(-1)
				for {
					old := peak.Load()
					if now <= old || peak.CompareAndSwap(old, now) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := peak.Load(); got > limit {
		t.Fatalf("observed %d concurrent calls, limit is %d", got, limit)
	}
	if got := gate.peak.Load(); got != peak.Load() {
		t.Fatalf("gate recorded peak %d, observed %d", got, peak.Load())
	}
}

func TestIOGateAbandonsQueuedCallOnCancellation(t *testing.T) {
	gate := newIOGate(1)
	if err := gate.enter(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer gate.exit()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	err := gate.do(ctx, func() error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("err = %v, called = %t; want context.Canceled without running the call", err, called)
	}
}

func spoolFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "upload-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func TestUploadsWaitForFileIOSlot(t *testing.T) {
	server := testServer(t, newMemoryBackend(), nil)
	server.fileIO = newIOGate(1)
	client := bytestream.NewByteStreamClient(testGRPCConn(t, server))

	// Occupy the only slot, as a burst of other uploads would.
	if err := server.fileIO.enter(t.Context()); err != nil {
		t.Fatal(err)
	}
	held := true
	defer func() {
		if held {
			server.fileIO.exit()
		}
	}()

	data := []byte("queued behind file io")
	reference := digestReference{hash: digest(data), size: int64(len(data))}
	done := make(chan error, 1)
	go func() {
		stream, err := client.Write(t.Context())
		if err == nil {
			err = stream.Send(bytestream.WriteRequest_builder{
				ResourceName: writeResource(reference),
				Data:         data,
				FinishWrite:  true,
			}.Build())
		}
		if err == nil {
			_, err = stream.CloseAndRecv()
		}
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("upload finished while file io was saturated: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if files := spoolFiles(t, server.cfg.CacheDir); len(files) != 0 {
		t.Fatalf("spool files created while file io was saturated: %v", files)
	}

	held = false
	server.fileIO.exit()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not resume after a file io slot was released")
	}
	if got := server.Snapshot().BytesReceived; got != uint64(len(data)) {
		t.Fatalf("bytes received = %d, want %d", got, len(data))
	}
}

func TestQueuedUploadLeavesNoSpoolFileWhenCancelled(t *testing.T) {
	server := testServer(t, newMemoryBackend(), nil)
	server.fileIO = newIOGate(1)
	if err := server.fileIO.enter(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer server.fileIO.exit()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	data := []byte("abandoned")
	err := server.writeObject(ctx, "cas", digest(data), bytes.NewReader(data), int64(len(data)))
	if !errors.Is(err, errLocalFailure) {
		t.Fatalf("err = %v, want errLocalFailure", err)
	}
	if files := spoolFiles(t, server.cfg.CacheDir); len(files) != 0 {
		t.Fatalf("cancelled upload left spool files: %v", files)
	}
}

// Bazel fans uploads out to thousands of concurrent ByteStream writes. Spool
// filesystem calls must stay within the gate however wide the fan-out, because
// each one in flight pins an OS thread.
func TestConcurrentUploadBurstStaysWithinFileIOLimit(t *testing.T) {
	const limit, uploads, chunk = 4, 500, 4 * 1024
	backend := newMemoryBackend()
	server := testServer(t, backend, func(cfg *Config) {
		cfg.StorageMode = "packs"
		cfg.Catalog = memoryCatalog{backend: backend}
		cfg.MaxBlobSize = 64 * 1024
		cfg.PackSize = 16 * 1024 * 1024
		cfg.PackFlushInterval = time.Hour
		cfg.MaxManifests = 100
	})
	defer closePackedServer(t, server)
	server.fileIO = newIOGate(limit)
	client := bytestream.NewByteStreamClient(testGRPCConn(t, server))

	var wg sync.WaitGroup
	var sent atomic.Uint64
	for i := range uploads {
		wg.Go(func() {
			data := bytes.Repeat([]byte(fmt.Sprintf("blob-%04d|", i)), 3*chunk/10)
			sum := sha256.Sum256(data)
			reference := digestReference{hash: hex.EncodeToString(sum[:]), size: int64(len(data))}
			stream, err := client.Write(t.Context())
			if err != nil {
				t.Error(err)
				return
			}
			for offset := 0; offset < len(data); offset += chunk {
				end := min(offset+chunk, len(data))
				request := bytestream.WriteRequest_builder{
					Data:        data[offset:end],
					WriteOffset: int64(offset),
					FinishWrite: end == len(data),
				}
				if offset == 0 {
					request.ResourceName = writeResource(reference)
				}
				if err := stream.Send(request.Build()); err != nil {
					t.Error(err)
					return
				}
			}
			if _, err := stream.CloseAndRecv(); err != nil {
				t.Error(err)
				return
			}
			sent.Add(uint64(len(data)))
		})
	}
	wg.Wait()

	if peak := server.fileIO.peak.Load(); peak > limit || peak == 0 {
		t.Fatalf("peak concurrent file io = %d, want 1..%d", peak, limit)
	}
	if active := server.fileIO.active.Load(); active != 0 {
		t.Fatalf("%d file io slots leaked", active)
	}
	if got, want := server.Snapshot().BytesReceived, sent.Load(); got != want {
		t.Fatalf("bytes received = %d, want %d", got, want)
	}
}

func burstPackedConfig(backend *memoryBackend) func(*Config) {
	return func(cfg *Config) {
		cfg.StorageMode = "packs"
		cfg.Catalog = memoryCatalog{backend: backend}
		// A restored pack is bounded by the blob size limit too.
		cfg.MaxBlobSize = 16 * 1024 * 1024
		cfg.PackSize = 16 * 1024 * 1024
		cfg.PackFlushInterval = time.Hour
		cfg.MaxManifests = 100
	}
}

func TestPackedSpoolReaderClosesBeforeDeletion(t *testing.T) {
	backend := newMemoryBackend()
	server := testServer(t, backend, burstPackedConfig(backend))
	t.Cleanup(func() { closePackedServer(t, server) })
	data := []byte("packed spool reader")
	if err := server.writeObject(t.Context(), "cas", digest(data), bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	reader, _, err := server.openObject(t.Context(), "cas", digest(data))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if runtime.GOOS == "windows" {
		if server.objectFilesMu.TryLock() {
			server.objectFilesMu.Unlock()
			t.Fatal("open Windows spool reader did not hold the deletion lock")
		}
	}
	got, err := io.ReadAll(server.gateReader(t.Context(), reader))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read spool = %q, %v", got, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if !server.objectFilesMu.TryLock() {
		t.Fatal("closed spool reader still holds the deletion lock")
	}
	server.objectFilesMu.Unlock()
	if err := server.packs.flush(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if files := spoolFiles(t, server.cfg.CacheDir); len(files) != 0 {
		t.Fatalf("packed publication left object spool files: %v", files)
	}
}

// restoredPackedServer seeds one pack with count blobs and returns a fresh
// server that can only serve them by restoring that pack.
func restoredPackedServer(t *testing.T, count int) (*Server, [][]byte) {
	t.Helper()
	backend := newMemoryBackend()
	seed := testServer(t, backend, burstPackedConfig(backend))
	blobs := make([][]byte, count)
	for i := range blobs {
		blobs[i] = bytes.Repeat([]byte(fmt.Sprintf("packed-%04d|", i)), 100)
		err := seed.writeObject(t.Context(), "cas", digest(blobs[i]), bytes.NewReader(blobs[i]), int64(len(blobs[i])))
		if err != nil {
			t.Fatal(err)
		}
	}
	closePackedServer(t, seed)

	restore := testServer(t, backend, burstPackedConfig(backend))
	t.Cleanup(func() { closePackedServer(t, restore) })
	return restore, blobs
}

func readBlob(t *testing.T, client bytestream.ByteStreamClient, data []byte) ([]byte, error) {
	t.Helper()
	reference := digestReference{hash: digest(data), size: int64(len(data))}
	stream, err := client.Read(t.Context(), bytestream.ReadRequest_builder{
		ResourceName: readResource(reference),
	}.Build())
	if err != nil {
		return nil, err
	}
	var received []byte
	for {
		response, err := stream.Recv()
		if err == io.EOF {
			return received, nil
		}
		if err != nil {
			return nil, err
		}
		received = append(received, response.GetData()...)
	}
}

func packedObjectFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "packed-object-*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

// Reopening a pack per read re-indexed the whole archive and spent several
// blocking syscalls on every lookup, which made read bursts pin thousands of
// threads. Once restored, a pack must serve every later read from one reader.
func TestPackedReadsShareOneReaderPerPack(t *testing.T) {
	server, blobs := restoredPackedServer(t, 3)
	if _, err := server.readObjectBytes(t.Context(), "cas", digest(blobs[0])); err != nil {
		t.Fatal(err)
	}
	server.packs.mu.Lock()
	if len(server.packs.loadedPacks) != 1 {
		t.Fatalf("loaded %d packs, want 1", len(server.packs.loadedPacks))
	}
	for _, pack := range server.packs.loadedPacks {
		// Unix permits unlinking a mapped file; Windows keeps its path until
		// the reader closes. Both must reuse the same restored reader.
		if runtime.GOOS != "windows" {
			if err := os.Remove(pack.path); err != nil {
				t.Fatal(err)
			}
		}
	}
	server.packs.mu.Unlock()

	for _, blob := range blobs[1:] {
		data, err := server.readObjectBytes(t.Context(), "cas", digest(blob))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, blob) {
			t.Fatal("packed read returned the wrong bytes")
		}
	}
	if downloads := server.Snapshot().PackDownloads; downloads != 1 {
		t.Fatalf("pack downloads = %d, want 1", downloads)
	}
}

func TestPackedReadsAvoidFileIOForRestoredObjects(t *testing.T) {
	server, blobs := restoredPackedServer(t, 2)
	client := bytestream.NewByteStreamClient(testGRPCConn(t, server))
	// Restore the pack first so the gate only blocks file-backed operations.
	if _, err := readBlob(t, client, blobs[0]); err != nil {
		t.Fatal(err)
	}
	before := len(packedObjectFiles(t, server.cfg.CacheDir))
	if before != 0 {
		t.Fatalf("pack restore created %d packed-object files before the second read", before)
	}

	server.fileIO = newIOGate(1)
	if err := server.fileIO.enter(t.Context()); err != nil {
		t.Fatal(err)
	}
	held := true
	defer func() {
		if held {
			server.fileIO.exit()
		}
	}()

	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := readBlob(t, client, blobs[1])
		done <- result{data, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !bytes.Equal(got.data, blobs[1]) {
			t.Fatal("read returned the wrong bytes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("packed object read waited for a file io slot")
	}
	if after := len(packedObjectFiles(t, server.cfg.CacheDir)); after != before {
		t.Fatalf("read created %d packed-object files", after-before)
	}

	held = false
	server.fileIO.exit()
}

// Restored objects are served from decoded pack data without request-driven
// filesystem operations, even when Bazel fans reads out widely.
func TestConcurrentPackedReadBurstDoesNotUseFileIO(t *testing.T) {
	const limit, reads = 4, 500
	server, blobs := restoredPackedServer(t, reads)
	server.fileIO = newIOGate(limit)
	client := bytestream.NewByteStreamClient(testGRPCConn(t, server))

	var wg sync.WaitGroup
	for _, blob := range blobs {
		wg.Go(func() {
			data, err := readBlob(t, client, blob)
			if err != nil {
				t.Error(err)
				return
			}
			if !bytes.Equal(data, blob) {
				t.Error("read returned the wrong bytes")
			}
		})
	}
	wg.Wait()

	if peak := server.fileIO.peak.Load(); peak != 0 {
		t.Fatalf("packed reads used %d concurrent file io slots, want none (limit %d)", peak, limit)
	}
	if active := server.fileIO.active.Load(); active != 0 {
		t.Fatalf("%d file io slots leaked", active)
	}
	if downloads := server.Snapshot().PackDownloads; downloads != 1 {
		t.Fatalf("pack downloads = %d, want 1", downloads)
	}
}
