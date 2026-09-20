package server

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	remoteexecution "github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/build/bazel/remote/execution/v2"
)

func TestParseActionResultCollectsOnlyExternalCASReferences(t *testing.T) {
	direct := referenceFor([]byte("direct output"))
	inline := referenceFor([]byte("inline output"))
	tree := referenceFor([]byte("tree"))
	root := referenceFor([]byte("root"))
	stdout := referenceFor([]byte("inline stdout"))
	stderr := referenceFor([]byte("external stderr"))

	actionResult := marshalProto(t, remoteexecution.ActionResult_builder{
		OutputFiles: []*remoteexecution.OutputFile{
			outputFileProto(direct, nil),
			outputFileProto(inline, []byte("inline output")),
		},
		OutputDirectories: []*remoteexecution.OutputDirectory{outputDirectoryProto(tree, root)},
		StdoutRaw:         []byte("inline stdout"),
		StdoutDigest:      digestProto(stdout),
		StderrDigest:      digestProto(stderr),
	}.Build())

	references, err := parseActionResult(actionResult)
	if err != nil {
		t.Fatal(err)
	}
	assertReferences(t, references.blobs.values, direct, stderr)
	assertReferences(t, references.trees.values, tree)
	assertReferences(t, references.directories.values, root)
}

func TestParseActionResultRejectsMalformedDigest(t *testing.T) {
	actionResult := marshalProto(t, remoteexecution.ActionResult_builder{
		OutputFiles: []*remoteexecution.OutputFile{
			outputFileProto(digestReference{hash: strings.Repeat("A", 64), size: 1}, nil),
		},
	}.Build())
	if _, err := parseActionResult(actionResult); err == nil {
		t.Fatal("uppercase digest was accepted")
	}
}

func TestParseTreeCollectsFilesAndRequiresEmbeddedChildren(t *testing.T) {
	rootFile := referenceFor([]byte("root file"))
	childFile := referenceFor([]byte("child file"))
	child := remoteexecution.Directory_builder{
		Files: []*remoteexecution.FileNode{fileNodeProto(childFile)},
	}.Build()
	childReference := referenceFor(marshalProto(t, child))
	root := remoteexecution.Directory_builder{
		Files:       []*remoteexecution.FileNode{fileNodeProto(rootFile)},
		Directories: []*remoteexecution.DirectoryNode{directoryNodeProto(childReference)},
	}.Build()
	tree := marshalProto(t, remoteexecution.Tree_builder{
		Root:     root,
		Children: []*remoteexecution.Directory{child},
	}.Build())

	files, err := parseTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	assertReferences(t, files, rootFile, childFile)

	truncated := marshalProto(t, remoteexecution.Tree_builder{Root: root}.Build())
	if _, err := parseTree(truncated); err == nil {
		t.Fatal("Tree with a missing embedded child was accepted")
	}
}

func TestActionResultReadRequiresCompleteCASClosure(t *testing.T) {
	output := []byte("cached output")
	outputReference := referenceFor(output)
	actionResult := actionResultProto(t, outputReference)
	actionDigest := strings.Repeat("a", 64)

	t.Run("complete", func(t *testing.T) {
		backend := newMemoryBackend()
		backend.objects["test-v1-ac-"+actionDigest] = actionResult
		backend.objects["test-v1-cas-"+outputReference.hash] = output
		server := testServer(t, backend, nil)

		response := readCacheObject(server, "/ac/"+actionDigest)
		if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), actionResult) {
			t.Fatalf("status/body = %d/%q", response.Code, response.Body.Bytes())
		}
		stats := server.Snapshot()
		if stats.ValidatedActionResults != 1 || stats.IncompleteActionResults != 0 ||
			stats.InvalidActionResults != 0 || stats.Hits != 1 || stats.BackendDownloads != 1 ||
			stats.BackendExistenceChecks != 1 {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	})

	t.Run("missing output", func(t *testing.T) {
		backend := newMemoryBackend()
		backend.objects["test-v1-ac-"+actionDigest] = actionResult
		server := testServer(t, backend, nil)

		response := readCacheObject(server, "/ac/"+actionDigest)
		if response.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", response.Code)
		}
		stats := server.Snapshot()
		if stats.IncompleteActionResults != 1 || stats.Misses != 1 || stats.Hits != 0 {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	})
}

func TestActionResultReadAllowsImplicitEmptyCASDigest(t *testing.T) {
	emptyReference := referenceFor(nil)
	actionResult := marshalProto(t, remoteexecution.ActionResult_builder{
		StdoutDigest: digestProto(emptyReference),
	}.Build())
	actionDigest := strings.Repeat("1", 64)
	backend := newMemoryBackend()
	backend.objects["test-v1-ac-"+actionDigest] = actionResult
	server := testServer(t, backend, nil)

	response := readCacheObject(server, "/ac/"+actionDigest)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), actionResult) {
		t.Fatalf("status/body = %d/%q", response.Code, response.Body.Bytes())
	}
	stats := server.Snapshot()
	if stats.ValidatedActionResults != 1 || stats.IncompleteActionResults != 0 ||
		stats.Hits != 1 || stats.BackendDownloads != 1 || stats.BackendExistenceChecks != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestActionResultReadValidatesTreeFileClosure(t *testing.T) {
	fileReference := referenceFor([]byte("missing nested file"))
	tree := marshalProto(t, remoteexecution.Tree_builder{
		Root: remoteexecution.Directory_builder{
			Files: []*remoteexecution.FileNode{fileNodeProto(fileReference)},
		}.Build(),
	}.Build())
	treeReference := referenceFor(tree)
	actionResult := marshalProto(t, remoteexecution.ActionResult_builder{
		OutputDirectories: []*remoteexecution.OutputDirectory{
			outputDirectoryProto(treeReference, digestReference{}),
		},
	}.Build())
	actionDigest := strings.Repeat("b", 64)

	backend := newMemoryBackend()
	backend.objects["test-v1-ac-"+actionDigest] = actionResult
	backend.objects["test-v1-cas-"+treeReference.hash] = tree
	server := testServer(t, backend, nil)

	response := readCacheObject(server, "/ac/"+actionDigest)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	stats := server.Snapshot()
	if stats.IncompleteActionResults != 1 || stats.BackendDownloads != 2 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestActionResultReadValidatesRootDirectoryClosure(t *testing.T) {
	file := []byte("nested output")
	fileReference := referenceFor(file)
	child := marshalProto(t, remoteexecution.Directory_builder{
		Files: []*remoteexecution.FileNode{fileNodeProto(fileReference)},
	}.Build())
	childReference := referenceFor(child)
	root := marshalProto(t, remoteexecution.Directory_builder{
		Directories: []*remoteexecution.DirectoryNode{directoryNodeProto(childReference)},
	}.Build())
	rootReference := referenceFor(root)
	actionResult := marshalProto(t, remoteexecution.ActionResult_builder{
		OutputDirectories: []*remoteexecution.OutputDirectory{
			outputDirectoryProto(digestReference{}, rootReference),
		},
	}.Build())
	actionDigest := strings.Repeat("e", 64)

	t.Run("complete", func(t *testing.T) {
		backend := newMemoryBackend()
		backend.objects["test-v1-ac-"+actionDigest] = actionResult
		backend.objects["test-v1-cas-"+rootReference.hash] = root
		backend.objects["test-v1-cas-"+childReference.hash] = child
		backend.objects["test-v1-cas-"+fileReference.hash] = file
		server := testServer(t, backend, nil)

		response := readCacheObject(server, "/ac/"+actionDigest)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", response.Code)
		}
		stats := server.Snapshot()
		if stats.ValidatedActionResults != 1 || stats.BackendDownloads != 3 ||
			stats.BackendExistenceChecks != 1 {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	})

	t.Run("missing nested file", func(t *testing.T) {
		backend := newMemoryBackend()
		backend.objects["test-v1-ac-"+actionDigest] = actionResult
		backend.objects["test-v1-cas-"+rootReference.hash] = root
		backend.objects["test-v1-cas-"+childReference.hash] = child
		server := testServer(t, backend, nil)

		response := readCacheObject(server, "/ac/"+actionDigest)
		if response.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", response.Code)
		}
		stats := server.Snapshot()
		if stats.IncompleteActionResults != 1 || stats.BackendDownloads != 3 {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	})
}

func TestActionResultReadRejectsInvalidPayload(t *testing.T) {
	actionDigest := strings.Repeat("c", 64)
	backend := newMemoryBackend()
	backend.objects["test-v1-ac-"+actionDigest] = []byte{0xff}
	server := testServer(t, backend, nil)

	response := readCacheObject(server, "/ac/"+actionDigest)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	stats := server.Snapshot()
	if stats.InvalidActionResults != 1 || stats.Misses != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestActionResultReadHandlesExistenceCheckErrors(t *testing.T) {
	outputReference := referenceFor([]byte("output"))
	actionResult := actionResultProto(t, outputReference)
	actionDigest := strings.Repeat("f", 64)

	for _, test := range []struct {
		name     string
		failOpen bool
		status   int
		misses   uint64
	}{
		{name: "fail open", failOpen: true, status: http.StatusNotFound, misses: 1},
		{name: "strict", failOpen: false, status: http.StatusBadGateway, misses: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := newMemoryBackend()
			backend.objects["test-v1-ac-"+actionDigest] = actionResult
			backend.existsErr = errors.New("unavailable")
			server := testServer(t, backend, func(cfg *Config) {
				cfg.FailOpen = test.failOpen
			})

			response := readCacheObject(server, "/ac/"+actionDigest)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
			stats := server.Snapshot()
			if stats.BackendLoadErrors != 1 || stats.Misses != test.misses ||
				stats.BackendExistenceChecks != 1 {
				t.Fatalf("unexpected stats: %+v", stats)
			}
		})
	}
}

func TestActionResultUploadPublishesOnlyCompleteCASClosure(t *testing.T) {
	output := []byte("persisted output")
	outputReference := referenceFor(output)
	actionResult := actionResultProto(t, outputReference)
	actionDigest := strings.Repeat("d", 64)

	t.Run("complete", func(t *testing.T) {
		backend := newMemoryBackend()
		backend.objects["test-v1-cas-"+outputReference.hash] = output
		server := testServer(t, backend, nil)

		response := putCacheObject(server, "/ac/"+actionDigest, actionResult)
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body)
		}
		if !bytes.Equal(backend.objects["test-v1-ac-"+actionDigest], actionResult) {
			t.Fatal("complete action result was not published")
		}
		stats := server.Snapshot()
		if stats.Uploads != 1 || stats.ValidatedActionResults != 1 ||
			stats.SkippedActionResultUploads != 0 {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	})

	t.Run("missing output", func(t *testing.T) {
		backend := newMemoryBackend()
		server := testServer(t, backend, nil)

		response := putCacheObject(server, "/ac/"+actionDigest, actionResult)
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body)
		}
		if _, exists := backend.objects["test-v1-ac-"+actionDigest]; exists {
			t.Fatal("incomplete action result was published")
		}
		stats := server.Snapshot()
		if stats.Uploads != 0 || stats.IncompleteActionResults != 1 ||
			stats.SkippedActionResultUploads != 1 {
			t.Fatalf("unexpected stats: %+v", stats)
		}
	})
}

func TestActionResultUploadAllowsImplicitEmptyCASDigest(t *testing.T) {
	emptyReference := referenceFor(nil)
	actionResult := marshalProto(t, remoteexecution.ActionResult_builder{
		StderrDigest: digestProto(emptyReference),
	}.Build())
	actionDigest := strings.Repeat("2", 64)
	backend := newMemoryBackend()
	server := testServer(t, backend, nil)

	response := putCacheObject(server, "/ac/"+actionDigest, actionResult)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if !bytes.Equal(backend.objects["test-v1-ac-"+actionDigest], actionResult) {
		t.Fatal("action result with an implicit empty digest was not published")
	}
	stats := server.Snapshot()
	if stats.Uploads != 1 || stats.ValidatedActionResults != 1 ||
		stats.IncompleteActionResults != 0 || stats.SkippedActionResultUploads != 0 ||
		stats.BackendExistenceChecks != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func readCacheObject(server *Server, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func headCacheObject(server *Server, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodHead, path, nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func putCacheObject(server *Server, path string, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(body))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func referenceFor(data []byte) digestReference {
	return digestReference{hash: digest(data), size: int64(len(data))}
}

func digestProto(reference digestReference) *remoteexecution.Digest {
	return remoteexecution.Digest_builder{
		Hash:      reference.hash,
		SizeBytes: reference.size,
	}.Build()
}

func outputFileProto(reference digestReference, inline []byte) *remoteexecution.OutputFile {
	return remoteexecution.OutputFile_builder{
		Digest:   digestProto(reference),
		Contents: inline,
	}.Build()
}

func outputDirectoryProto(tree, root digestReference) *remoteexecution.OutputDirectory {
	message := remoteexecution.OutputDirectory_builder{}
	if tree.hash != "" {
		message.TreeDigest = digestProto(tree)
	}
	if root.hash != "" {
		message.RootDirectoryDigest = digestProto(root)
	}
	return message.Build()
}

func fileNodeProto(reference digestReference) *remoteexecution.FileNode {
	return remoteexecution.FileNode_builder{Digest: digestProto(reference)}.Build()
}

func directoryNodeProto(reference digestReference) *remoteexecution.DirectoryNode {
	return remoteexecution.DirectoryNode_builder{Digest: digestProto(reference)}.Build()
}

// actionResultProto builds the minimal action result that references a single CAS
// blob, which is the fixture most cache-level tests need.
func actionResultProto(t *testing.T, output digestReference) []byte {
	t.Helper()
	return marshalProto(t, remoteexecution.ActionResult_builder{
		OutputFiles: []*remoteexecution.OutputFile{outputFileProto(output, nil)},
	}.Build())
}

func marshalProto(t *testing.T, message proto.Message) []byte {
	t.Helper()
	encoded, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func assertReferences(t *testing.T, got []digestReference, want ...digestReference) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("references = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("references[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
