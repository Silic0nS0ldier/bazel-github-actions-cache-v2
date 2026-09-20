package server

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	remoteexecution "github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/build/bazel/remote/execution/v2"
	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/google/bytestream"
)

func testGRPCConn(t *testing.T, server *Server) *grpc.ClientConn {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	handler := server.GRPCHandler()
	go func() { _ = handler.Serve(listener) }()
	t.Cleanup(handler.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func assertCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if status.Code(err) != want {
		t.Fatalf("code = %s (%v), want %s", status.Code(err), err, want)
	}
}

func readResource(reference digestReference) string {
	return "blobs/" + reference.hash + "/" + strconv.FormatInt(reference.size, 10)
}

func writeResource(reference digestReference) string {
	return "uploads/8c6ded1a-0f5e-4f2b-9c10-3a5f1d8b7e42/blobs/" +
		reference.hash + "/" + strconv.FormatInt(reference.size, 10)
}

func TestGRPCCapabilitiesAdvertiseSHA256AndWriteState(t *testing.T) {
	for _, writeEnabled := range []bool{true, false} {
		server := testServer(t, newMemoryBackend(), func(cfg *Config) {
			cfg.WriteEnabled = writeEnabled
		})
		client := remoteexecution.NewCapabilitiesClient(testGRPCConn(t, server))

		capabilities, err := client.GetCapabilities(
			t.Context(),
			remoteexecution.GetCapabilitiesRequest_builder{}.Build(),
		)
		if err != nil {
			t.Fatal(err)
		}
		cache := capabilities.GetCacheCapabilities()
		functions := cache.GetDigestFunctions()
		if len(functions) != 1 || functions[0] != remoteexecution.DigestFunction_SHA256 {
			t.Fatalf("digest functions = %v", functions)
		}
		if got := cache.GetActionCacheUpdateCapabilities().GetUpdateEnabled(); got != writeEnabled {
			t.Fatalf("update_enabled = %t, want %t", got, writeEnabled)
		}
		if cache.GetMaxBatchTotalSizeBytes() != maxBatchSize {
			t.Fatalf("max_batch_total_size_bytes = %d", cache.GetMaxBatchTotalSizeBytes())
		}
		if capabilities.GetExecutionCapabilities() != nil {
			t.Fatal("cache-only server advertised execution capabilities")
		}
	}
}

func TestGRPCRejectsInstanceNames(t *testing.T) {
	server := testServer(t, newMemoryBackend(), nil)
	conn := testGRPCConn(t, server)

	_, err := remoteexecution.NewCapabilitiesClient(conn).GetCapabilities(
		t.Context(),
		remoteexecution.GetCapabilitiesRequest_builder{InstanceName: "other"}.Build(),
	)
	assertCode(t, err, codes.InvalidArgument)

	_, err = remoteexecution.NewActionCacheClient(conn).GetActionResult(
		t.Context(),
		remoteexecution.GetActionResultRequest_builder{
			InstanceName: "other",
			ActionDigest: digestProto(referenceFor([]byte("action"))),
		}.Build(),
	)
	assertCode(t, err, codes.InvalidArgument)
}

func TestGRPCActionCacheRoundTripRequiresCompleteClosure(t *testing.T) {
	output := []byte("grpc cached output")
	outputReference := referenceFor(output)
	actionDigest := digestProto(digestReference{hash: strings.Repeat("a", 64), size: 7})
	result := remoteexecution.ActionResult_builder{
		OutputFiles: []*remoteexecution.OutputFile{outputFileProto(outputReference, nil)},
	}.Build()

	t.Run("complete", func(t *testing.T) {
		backend := newMemoryBackend()
		backend.objects["test-v1-cas-"+outputReference.hash] = output
		server := testServer(t, backend, nil)
		client := remoteexecution.NewActionCacheClient(testGRPCConn(t, server))

		_, err := client.UpdateActionResult(t.Context(), remoteexecution.UpdateActionResultRequest_builder{
			ActionDigest: actionDigest,
			ActionResult: result,
		}.Build())
		if err != nil {
			t.Fatal(err)
		}
		got, err := client.GetActionResult(t.Context(), remoteexecution.GetActionResultRequest_builder{
			ActionDigest: actionDigest,
		}.Build())
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(got, result) {
			t.Fatalf("action result = %v, want %v", got, result)
		}
	})

	t.Run("missing output", func(t *testing.T) {
		server := testServer(t, newMemoryBackend(), nil)
		client := remoteexecution.NewActionCacheClient(testGRPCConn(t, server))

		// The upload is accepted but never published, so the read misses.
		if _, err := client.UpdateActionResult(t.Context(), remoteexecution.UpdateActionResultRequest_builder{
			ActionDigest: actionDigest,
			ActionResult: result,
		}.Build()); err != nil {
			t.Fatal(err)
		}
		_, err := client.GetActionResult(t.Context(), remoteexecution.GetActionResultRequest_builder{
			ActionDigest: actionDigest,
		}.Build())
		assertCode(t, err, codes.NotFound)
		if stats := server.Snapshot(); stats.SkippedActionResultUploads != 1 {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	})
}

func TestGRPCFindMissingBlobsBatchesPresenceChecks(t *testing.T) {
	present := []byte("present blob")
	presentReference := referenceFor(present)
	absentReference := referenceFor([]byte("absent blob"))
	emptyReference := referenceFor(nil)

	backend := newMemoryBackend()
	backend.objects["test-v1-cas-"+presentReference.hash] = present
	server := testServer(t, backend, nil)
	client := remoteexecution.NewContentAddressableStorageClient(testGRPCConn(t, server))

	response, err := client.FindMissingBlobs(t.Context(), remoteexecution.FindMissingBlobsRequest_builder{
		BlobDigests: []*remoteexecution.Digest{
			digestProto(presentReference),
			digestProto(absentReference),
			digestProto(emptyReference),
		},
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	missing := response.GetMissingBlobDigests()
	if len(missing) != 1 || missing[0].GetHash() != absentReference.hash {
		t.Fatalf("missing = %v, want only %s", missing, absentReference.hash)
	}
	// The empty digest is answered locally; only the other two reach the backend.
	if calls := backend.existsCount(); calls != 2 {
		t.Fatalf("backend existence checks = %d, want 2", calls)
	}
}

func TestGRPCBatchUpdateAndReadBlobs(t *testing.T) {
	blob := []byte("batched blob")
	reference := referenceFor(blob)
	mismatched := referenceFor([]byte("different blob"))
	server := testServer(t, newMemoryBackend(), nil)
	client := remoteexecution.NewContentAddressableStorageClient(testGRPCConn(t, server))

	update, err := client.BatchUpdateBlobs(t.Context(), remoteexecution.BatchUpdateBlobsRequest_builder{
		Requests: []*remoteexecution.BatchUpdateBlobsRequest_Request{
			remoteexecution.BatchUpdateBlobsRequest_Request_builder{
				Digest: digestProto(reference),
				Data:   blob,
			}.Build(),
			remoteexecution.BatchUpdateBlobsRequest_Request_builder{
				Digest: digestProto(digestReference{hash: mismatched.hash, size: reference.size}),
				Data:   blob,
			}.Build(),
		},
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	responses := update.GetResponses()
	if len(responses) != 2 {
		t.Fatalf("responses = %d, want 2", len(responses))
	}
	if code := codes.Code(responses[0].GetStatus().GetCode()); code != codes.OK {
		t.Fatalf("first upload status = %s", code)
	}
	if code := codes.Code(responses[1].GetStatus().GetCode()); code != codes.InvalidArgument {
		t.Fatalf("mismatched upload status = %s, want InvalidArgument", code)
	}

	read, err := client.BatchReadBlobs(t.Context(), remoteexecution.BatchReadBlobsRequest_builder{
		Digests: []*remoteexecution.Digest{
			digestProto(reference),
			digestProto(referenceFor([]byte("never uploaded"))),
		},
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	items := read.GetResponses()
	if len(items) != 2 {
		t.Fatalf("responses = %d, want 2", len(items))
	}
	if string(items[0].GetData()) != string(blob) {
		t.Fatalf("blob = %q", items[0].GetData())
	}
	if code := codes.Code(items[1].GetStatus().GetCode()); code != codes.NotFound {
		t.Fatalf("absent blob status = %s, want NotFound", code)
	}
}

func TestGRPCBatchRejectsOversizedRequests(t *testing.T) {
	server := testServer(t, newMemoryBackend(), nil)
	client := remoteexecution.NewContentAddressableStorageClient(testGRPCConn(t, server))

	_, err := client.BatchReadBlobs(t.Context(), remoteexecution.BatchReadBlobsRequest_builder{
		Digests: []*remoteexecution.Digest{
			digestProto(digestReference{hash: strings.Repeat("b", 64), size: maxBatchSize + 1}),
		},
	}.Build())
	assertCode(t, err, codes.InvalidArgument)
}

// Probing the same blobs over either transport must touch the same number of
// objects and the same number of backend entries; only the number of client
// calls differs, which is what makes batching visible in the statistics.
func TestAccountingSeparatesCallsFromObjectsAndBackendEntries(t *testing.T) {
	present := []byte("present blob")
	presentReference := referenceFor(present)
	absent := referenceFor([]byte("absent blob"))
	empty := referenceFor(nil)

	newServer := func(t *testing.T) *Server {
		backend := newMemoryBackend()
		backend.objects["test-v1-cas-"+presentReference.hash] = present
		return testServer(t, backend, nil)
	}

	t.Run("http", func(t *testing.T) {
		server := newServer(t)
		for _, reference := range []digestReference{presentReference, absent, empty} {
			headCacheObject(server, "/cas/"+reference.hash)
		}
		stats := server.Snapshot()
		if stats.Requests != 3 || stats.Operations != 3 || stats.BackendRequests != 2 {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	})

	t.Run("grpc", func(t *testing.T) {
		server := newServer(t)
		client := remoteexecution.NewContentAddressableStorageClient(testGRPCConn(t, server))
		_, err := client.FindMissingBlobs(t.Context(), remoteexecution.FindMissingBlobsRequest_builder{
			BlobDigests: []*remoteexecution.Digest{
				digestProto(presentReference),
				digestProto(absent),
				digestProto(empty),
			},
		}.Build())
		if err != nil {
			t.Fatal(err)
		}
		stats := server.Snapshot()
		if stats.Requests != 1 || stats.Operations != 3 || stats.BackendRequests != 2 {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	})
}

func TestGRPCByteStreamRoundTrip(t *testing.T) {
	blob := []byte("a reasonably long byte stream payload")
	reference := referenceFor(blob)
	server := testServer(t, newMemoryBackend(), nil)
	client := bytestream.NewByteStreamClient(testGRPCConn(t, server))

	stream, err := client.Write(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	split := 10
	send := func(offset int64, data []byte, finish bool) {
		t.Helper()
		err := stream.Send(bytestream.WriteRequest_builder{
			ResourceName: writeResource(reference),
			WriteOffset:  offset,
			Data:         data,
			FinishWrite:  finish,
		}.Build())
		if err != nil {
			t.Fatal(err)
		}
	}
	send(0, blob[:split], false)
	send(int64(split), blob[split:], true)
	response, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatal(err)
	}
	if response.GetCommittedSize() != reference.size {
		t.Fatalf("committed_size = %d, want %d", response.GetCommittedSize(), reference.size)
	}

	for _, test := range []struct {
		name   string
		offset int64
		limit  int64
		want   string
	}{
		{name: "whole blob", want: string(blob)},
		{name: "from offset", offset: 2, want: string(blob[2:])},
		{name: "limited", limit: 6, want: string(blob[:6])},
	} {
		t.Run(test.name, func(t *testing.T) {
			reads, err := client.Read(t.Context(), bytestream.ReadRequest_builder{
				ResourceName: readResource(reference),
				ReadOffset:   test.offset,
				ReadLimit:    test.limit,
			}.Build())
			if err != nil {
				t.Fatal(err)
			}
			var got []byte
			for {
				chunk, err := reads.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				got = append(got, chunk.GetData()...)
			}
			if string(got) != test.want {
				t.Fatalf("read = %q, want %q", got, test.want)
			}
		})
	}
}

func TestGRPCByteStreamRejectsMalformedWrites(t *testing.T) {
	blob := []byte("stream payload")
	reference := referenceFor(blob)
	server := testServer(t, newMemoryBackend(), nil)
	client := bytestream.NewByteStreamClient(testGRPCConn(t, server))

	write := func(t *testing.T, requests ...*bytestream.WriteRequest) error {
		t.Helper()
		stream, err := client.Write(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, request := range requests {
			if err := stream.Send(request); err != nil {
				break
			}
		}
		_, err = stream.CloseAndRecv()
		return err
	}

	t.Run("digest mismatch", func(t *testing.T) {
		other := referenceFor([]byte("something else"))
		err := write(t, bytestream.WriteRequest_builder{
			ResourceName: writeResource(digestReference{hash: other.hash, size: reference.size}),
			Data:         blob,
			FinishWrite:  true,
		}.Build())
		assertCode(t, err, codes.InvalidArgument)
	})

	t.Run("non-contiguous offset", func(t *testing.T) {
		err := write(t,
			bytestream.WriteRequest_builder{
				ResourceName: writeResource(reference),
				Data:         blob[:4],
			}.Build(),
			bytestream.WriteRequest_builder{
				ResourceName: writeResource(reference),
				WriteOffset:  99,
				Data:         blob[4:],
				FinishWrite:  true,
			}.Build(),
		)
		assertCode(t, err, codes.InvalidArgument)
	})

	t.Run("ended before finish_write", func(t *testing.T) {
		err := write(t, bytestream.WriteRequest_builder{
			ResourceName: writeResource(reference),
			Data:         blob[:4],
		}.Build())
		assertCode(t, err, codes.InvalidArgument)
	})

	t.Run("compressed resource", func(t *testing.T) {
		err := write(t, bytestream.WriteRequest_builder{
			ResourceName: "uploads/id/compressed-blobs/zstd/" + reference.hash + "/14",
			Data:         blob,
			FinishWrite:  true,
		}.Build())
		assertCode(t, err, codes.InvalidArgument)
	})
}

func TestGRPCByteStreamReadRejectsOutOfRangeOffset(t *testing.T) {
	blob := []byte("short")
	reference := referenceFor(blob)
	backend := newMemoryBackend()
	backend.objects["test-v1-cas-"+reference.hash] = blob
	server := testServer(t, backend, nil)
	client := bytestream.NewByteStreamClient(testGRPCConn(t, server))

	stream, err := client.Read(t.Context(), bytestream.ReadRequest_builder{
		ResourceName: readResource(reference),
		ReadOffset:   reference.size + 1,
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	assertCode(t, err, codes.OutOfRange)
}

func TestGRPCQueryWriteStatusReportsNoProgress(t *testing.T) {
	server := testServer(t, newMemoryBackend(), nil)
	client := bytestream.NewByteStreamClient(testGRPCConn(t, server))

	_, err := client.QueryWriteStatus(t.Context(), bytestream.QueryWriteStatusRequest_builder{
		ResourceName: writeResource(referenceFor([]byte("anything"))),
	}.Build())
	assertCode(t, err, codes.NotFound)
}

func TestGRPCGetTreeStreamsNestedDirectories(t *testing.T) {
	file := referenceFor([]byte("nested file"))
	child := marshalProto(t, remoteexecution.Directory_builder{
		Files: []*remoteexecution.FileNode{fileNodeProto(file)},
	}.Build())
	childReference := referenceFor(child)
	root := marshalProto(t, remoteexecution.Directory_builder{
		Directories: []*remoteexecution.DirectoryNode{directoryNodeProto(childReference)},
	}.Build())
	rootReference := referenceFor(root)

	backend := newMemoryBackend()
	backend.objects["test-v1-cas-"+rootReference.hash] = root
	backend.objects["test-v1-cas-"+childReference.hash] = child
	server := testServer(t, backend, nil)
	client := remoteexecution.NewContentAddressableStorageClient(testGRPCConn(t, server))

	stream, err := client.GetTree(t.Context(), remoteexecution.GetTreeRequest_builder{
		RootDigest: digestProto(rootReference),
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	var directories []*remoteexecution.Directory
	for {
		page, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		directories = append(directories, page.GetDirectories()...)
	}
	if len(directories) != 2 {
		t.Fatalf("directories = %d, want 2", len(directories))
	}
	if got := directories[1].GetFiles(); len(got) != 1 || got[0].GetDigest().GetHash() != file.hash {
		t.Fatalf("child directory = %v", directories[1])
	}
}

func TestGRPCGetTreeRejectsMissingRoot(t *testing.T) {
	server := testServer(t, newMemoryBackend(), nil)
	client := remoteexecution.NewContentAddressableStorageClient(testGRPCConn(t, server))

	stream, err := client.GetTree(t.Context(), remoteexecution.GetTreeRequest_builder{
		RootDigest: digestProto(referenceFor([]byte("absent root"))),
	}.Build())
	if err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	assertCode(t, err, codes.NotFound)
}
