package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
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

func TestUploadsWaitForSpoolIOSlot(t *testing.T) {
	server := testServer(t, newMemoryBackend(), nil)
	server.spoolIO = newIOGate(1)
	client := bytestream.NewByteStreamClient(testGRPCConn(t, server))

	// Occupy the only slot, as a burst of other uploads would.
	if err := server.spoolIO.enter(t.Context()); err != nil {
		t.Fatal(err)
	}
	held := true
	defer func() {
		if held {
			server.spoolIO.exit()
		}
	}()

	data := []byte("queued behind spool io")
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
		t.Fatalf("upload finished while spool io was saturated: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if files := spoolFiles(t, server.cfg.CacheDir); len(files) != 0 {
		t.Fatalf("spool files created while spool io was saturated: %v", files)
	}

	held = false
	server.spoolIO.exit()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not resume after a spool io slot was released")
	}
	if got := server.Snapshot().BytesReceived; got != uint64(len(data)) {
		t.Fatalf("bytes received = %d, want %d", got, len(data))
	}
}

func TestQueuedUploadLeavesNoSpoolFileWhenCancelled(t *testing.T) {
	server := testServer(t, newMemoryBackend(), nil)
	server.spoolIO = newIOGate(1)
	if err := server.spoolIO.enter(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer server.spoolIO.exit()

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
func TestConcurrentUploadBurstStaysWithinSpoolIOLimit(t *testing.T) {
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
	server.spoolIO = newIOGate(limit)
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

	if peak := server.spoolIO.peak.Load(); peak > limit || peak == 0 {
		t.Fatalf("peak concurrent spool io = %d, want 1..%d", peak, limit)
	}
	if active := server.spoolIO.active.Load(); active != 0 {
		t.Fatalf("%d spool io slots leaked", active)
	}
	if got, want := server.Snapshot().BytesReceived, sent.Load(); got != want {
		t.Fatalf("bytes received = %d, want %d", got, want)
	}
}
