package server

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	remoteasset "github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/build/bazel/remote/asset/v1"
)

func futureTimestamp() *timestamppb.Timestamp {
	return timestamppb.New(time.Now().Add(time.Hour))
}

func subresourceIntegrity(data []byte) string {
	sum := sha256.Sum256(data)
	return sha256Algorithm + "-" + base64.StdEncoding.EncodeToString(sum[:])
}

func integrityWith(t *testing.T, algorithm string, data []byte) string {
	t.Helper()
	newHash, ok := checksumAlgorithms[algorithm]
	if !ok {
		t.Fatalf("unknown algorithm %q", algorithm)
	}
	hasher := newHash()
	hasher.Write(data)
	return algorithm + "-" + base64.StdEncoding.EncodeToString(hasher.Sum(nil))
}

func checksumQualifiers(value string) []*remoteasset.Qualifier {
	return []*remoteasset.Qualifier{
		remoteasset.Qualifier_builder{Name: checksumQualifier, Value: value}.Build(),
	}
}

// assetOrigin stands in for an upstream that repository rules download from.
func assetOrigin(t *testing.T, body []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write(body)
	}))
	t.Cleanup(origin.Close)
	return origin, &requests
}

func assetServer(t *testing.T, backend *memoryBackend, origin *httptest.Server) *Server {
	t.Helper()
	return testServer(t, backend, func(cfg *Config) {
		if origin != nil {
			cfg.AssetClient = origin.Client()
		}
	})
}

func fetchBlob(
	t *testing.T,
	server *Server,
	request *remoteasset.FetchBlobRequest,
) *remoteasset.FetchBlobResponse {
	t.Helper()
	client := remoteasset.NewFetchClient(testGRPCConn(t, server))
	response, err := client.FetchBlob(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestFetchBlobServesAnAssetAlreadyInTheCache(t *testing.T) {
	asset := []byte("a cached repository archive")
	reference := referenceFor(asset)
	backend := newMemoryBackend()
	backend.objects["test-v1-cas-"+reference.hash] = asset
	origin, requests := assetOrigin(t, asset)
	server := assetServer(t, backend, origin)

	response := fetchBlob(t, server, remoteasset.FetchBlobRequest_builder{
		Uris:       []string{origin.URL + "/archive.tar.gz"},
		Qualifiers: checksumQualifiers(subresourceIntegrity(asset)),
	}.Build())

	if code := codes.Code(response.GetStatus().GetCode()); code != codes.OK {
		t.Fatalf("status = %s (%s)", code, response.GetStatus().GetMessage())
	}
	if got := response.GetBlobDigest(); got.GetHash() != reference.hash || got.GetSizeBytes() != reference.size {
		t.Fatalf("blob digest = %v, want %v", got, reference)
	}
	if requests.Load() != 0 {
		t.Fatal("a cached asset must not be fetched from its origin")
	}
}

func TestFetchBlobFetchesAndPublishesOnMiss(t *testing.T) {
	asset := []byte("an archive that is not cached yet")
	reference := referenceFor(asset)
	backend := newMemoryBackend()
	origin, requests := assetOrigin(t, asset)
	server := assetServer(t, backend, origin)
	request := remoteasset.FetchBlobRequest_builder{
		Uris:       []string{origin.URL + "/archive.tar.gz"},
		Qualifiers: checksumQualifiers(subresourceIntegrity(asset)),
	}.Build()

	response := fetchBlob(t, server, request)
	if code := codes.Code(response.GetStatus().GetCode()); code != codes.OK {
		t.Fatalf("status = %s (%s)", code, response.GetStatus().GetMessage())
	}
	if response.GetBlobDigest().GetHash() != reference.hash {
		t.Fatalf("blob digest = %v", response.GetBlobDigest())
	}
	if response.GetUri() != origin.URL+"/archive.tar.gz" {
		t.Fatalf("uri = %q", response.GetUri())
	}
	// The fetched asset has to reach the shared cache, or no later job benefits.
	if got := backend.objects["test-v1-cas-"+reference.hash]; string(got) != string(asset) {
		t.Fatalf("asset was not published: %q", got)
	}
	if stats := server.Snapshot(); stats.AssetDownloads != 1 || stats.Uploads != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	// A second request is served from the cache the first one populated.
	if response := fetchBlob(t, server, request); codes.Code(response.GetStatus().GetCode()) != codes.OK {
		t.Fatalf("second fetch = %s", response.GetStatus().GetMessage())
	}
	if requests.Load() != 1 {
		t.Fatalf("origin was contacted %d times, want 1", requests.Load())
	}
	// A hit costs no download, so without its own counter it leaves no trace at
	// all and a busy cache is indistinguishable from an unused one.
	stats := server.Snapshot()
	if stats.AssetRequests != 2 || stats.AssetHits != 1 || stats.AssetDownloads != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestFetchBlobRejectsContentThatFailsItsChecksum(t *testing.T) {
	expected := []byte("what the caller asked for")
	origin, _ := assetOrigin(t, []byte("something else entirely"))
	backend := newMemoryBackend()
	server := assetServer(t, backend, origin)

	response := fetchBlob(t, server, remoteasset.FetchBlobRequest_builder{
		Uris:       []string{origin.URL + "/archive.tar.gz"},
		Qualifiers: checksumQualifiers(subresourceIntegrity(expected)),
	}.Build())

	if code := codes.Code(response.GetStatus().GetCode()); code != codes.NotFound {
		t.Fatalf("status = %s, want NotFound", code)
	}
	if len(backend.objects) != 0 {
		t.Fatalf("unverified content was published: %v", backend.objects)
	}
	if stats := server.Snapshot(); stats.AssetFetchErrors != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

// These messages surface to users as "WARNING: Remote Cache: NOT_FOUND: ...",
// often once per download, so they have to explain themselves on one line.
func TestChecksumRejectionsExplainThemselves(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
		want  string
	}{
		{
			name:  "unknown algorithm",
			value: "md5-" + base64.StdEncoding.EncodeToString(make([]byte, 16)),
			want:  `checksum algorithm "md5" is not supported`,
		},
		{
			name:  "not subresource integrity",
			value: "deadbeef",
			want:  `checksum.sri "deadbeef" is not subresource integrity`,
		},
		{
			name:  "empty",
			value: "",
			want:  `checksum.sri "" is not subresource integrity`,
		},
		{
			name:  "not base64",
			value: "sha256-not base64!",
			want:  `checksum.sri "sha256-not base64!" is not base64`,
		},
		{
			name:  "wrong digest length",
			value: "sha256-" + base64.StdEncoding.EncodeToString([]byte("short")),
			want:  `checksum.sri "sha256-c2hvcnQ=" decodes to 5 bytes, not the 32 a sha256 digest needs`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseChecksum(checksumQualifiers(test.value))
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}

	_, err := parseChecksum(nil)
	want := "no checksum was declared, so the download cannot be cached"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

// Bazel reports the message without saying which download produced it.
func TestFetchFailuresNameTheResource(t *testing.T) {
	server := assetServer(t, newMemoryBackend(), nil)

	response := fetchBlob(t, server, remoteasset.FetchBlobRequest_builder{
		Uris:       []string{"https://example.invalid/pkg.tgz?token=secret"},
		Qualifiers: checksumQualifiers("deadbeef"),
	}.Build())
	message := response.GetStatus().GetMessage()
	want := `https://example.invalid/pkg.tgz: checksum.sri "deadbeef" is not subresource integrity`
	if message != want {
		t.Fatalf("message = %q, want %q", message, want)
	}

	// Several mirrors still name one resource, and never a query string.
	response = fetchBlob(t, server, remoteasset.FetchBlobRequest_builder{
		Uris: []string{
			"https://example.invalid/a.tgz?sig=secret",
			"https://mirror.invalid/a.tgz",
		},
		Qualifiers: checksumQualifiers("deadbeef"),
	}.Build())
	message = response.GetStatus().GetMessage()
	if !strings.HasPrefix(message, "https://example.invalid/a.tgz (and 1 more): ") {
		t.Fatalf("message = %q", message)
	}
	if strings.Contains(message, "secret") {
		t.Fatalf("message leaked a query string: %q", message)
	}
}

// npm integrity is sha512, so without alias support none of it could be cached.
func TestFetchBlobCachesAssetsDeclaredWithAnyChecksum(t *testing.T) {
	asset := []byte("a package tarball from a registry")
	reference := referenceFor(asset)

	for _, algorithm := range []string{"sha1", "sha256", "sha384", "sha512", "blake3"} {
		t.Run(algorithm, func(t *testing.T) {
			backend := newMemoryBackend()
			origin, requests := assetOrigin(t, asset)
			server := assetServer(t, backend, origin)
			request := remoteasset.FetchBlobRequest_builder{
				Uris:       []string{origin.URL + "/pkg.tgz"},
				Qualifiers: checksumQualifiers(integrityWith(t, algorithm, asset)),
			}.Build()

			response := fetchBlob(t, server, request)
			if code := codes.Code(response.GetStatus().GetCode()); code != codes.OK {
				t.Fatalf("status = %s (%s)", code, response.GetStatus().GetMessage())
			}
			// Whatever the caller declared, the blob is addressed by sha256.
			if got := response.GetBlobDigest().GetHash(); got != reference.hash {
				t.Fatalf("blob digest = %s, want %s", got, reference.hash)
			}
			if got := backend.objects["test-v1-cas-"+reference.hash]; string(got) != string(asset) {
				t.Fatalf("asset was not published: %q", got)
			}

			second := fetchBlob(t, server, request)
			if code := codes.Code(second.GetStatus().GetCode()); code != codes.OK {
				t.Fatalf("second fetch = %s", second.GetStatus().GetMessage())
			}
			if requests.Load() != 1 {
				t.Fatalf("origin was contacted %d times, want 1", requests.Load())
			}
		})
	}
}

// Packed mode is the interesting case for aliases: they must travel in a CARv2
// pack rather than costing one Actions-cache creation each, and must survive a
// fresh runner that only has the published manifests.
func TestAssetAliasSurvivesAPackedRoundTrip(t *testing.T) {
	asset := []byte("a registry tarball addressed by sha512")
	reference := referenceFor(asset)
	backend := newMemoryBackend()
	origin, requests := assetOrigin(t, asset)
	request := remoteasset.FetchBlobRequest_builder{
		Uris:       []string{origin.URL + "/pkg.tgz"},
		Qualifiers: checksumQualifiers(integrityWith(t, "sha512", asset)),
	}.Build()

	seed := testServer(t, backend, func(cfg *Config) {
		cfg.StorageMode = "packs"
		cfg.Catalog = memoryCatalog{backend: backend}
		cfg.PackSize = 1024
		cfg.PackFlushInterval = time.Hour
		cfg.MaxManifests = 16
		cfg.AssetClient = origin.Client()
	})
	if response := fetchBlob(t, seed, request); codes.Code(response.GetStatus().GetCode()) != codes.OK {
		t.Fatalf("seed fetch = %s", response.GetStatus().GetMessage())
	}
	closePackedServer(t, seed)
	if stats := seed.Snapshot(); stats.PackUploads == 0 || stats.ManifestUploads == 0 {
		t.Fatalf("nothing was published: %+v", stats)
	}

	restored := testServer(t, backend, func(cfg *Config) {
		cfg.StorageMode = "packs"
		cfg.Catalog = memoryCatalog{backend: backend}
		cfg.PackSize = 1024
		cfg.PackFlushInterval = time.Hour
		cfg.MaxManifests = 16
		cfg.AssetClient = origin.Client()
	})
	response := fetchBlob(t, restored, request)
	if code := codes.Code(response.GetStatus().GetCode()); code != codes.OK {
		t.Fatalf("restored fetch = %s (%s)", code, response.GetStatus().GetMessage())
	}
	if got := response.GetBlobDigest().GetHash(); got != reference.hash {
		t.Fatalf("blob digest = %s, want %s", got, reference.hash)
	}
	if requests.Load() != 1 {
		t.Fatalf("origin was contacted %d times, want 1", requests.Load())
	}
}

func TestFetchBlobRefusesUnverifiableOrInsecureRequests(t *testing.T) {
	asset := []byte("an archive")
	origin, requests := assetOrigin(t, asset)

	for _, test := range []struct {
		name   string
		mutate func(*remoteasset.FetchBlobRequest_builder)
	}{
		{
			name:   "no checksum",
			mutate: func(b *remoteasset.FetchBlobRequest_builder) { b.Qualifiers = nil },
		},
		{
			name: "checksum in an unknown algorithm",
			mutate: func(b *remoteasset.FetchBlobRequest_builder) {
				b.Qualifiers = checksumQualifiers("md5-" + strings.Repeat("A", 24))
			},
		},
		{
			name: "plain http origin",
			mutate: func(b *remoteasset.FetchBlobRequest_builder) {
				b.Uris = []string{"http://127.0.0.1:1/archive.tar.gz"}
			},
		},
		{
			name:   "no uris",
			mutate: func(b *remoteasset.FetchBlobRequest_builder) { b.Uris = nil },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := assetServer(t, newMemoryBackend(), origin)
			builder := remoteasset.FetchBlobRequest_builder{
				Uris:       []string{origin.URL + "/archive.tar.gz"},
				Qualifiers: checksumQualifiers(subresourceIntegrity(asset)),
			}
			test.mutate(&builder)

			response := fetchBlob(t, server, builder.Build())
			if code := codes.Code(response.GetStatus().GetCode()); code != codes.NotFound {
				t.Fatalf("status = %s, want NotFound", code)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("origin was contacted %d times, want 0", requests.Load())
	}
}

// Bazel sets oldest_content_accepted to a future timestamp when a repository
// rule declares no checksum, which forbids answering from cache.
func TestFetchBlobWillNotAnswerWhenFreshContentIsDemanded(t *testing.T) {
	asset := []byte("a cached archive")
	reference := referenceFor(asset)
	backend := newMemoryBackend()
	backend.objects["test-v1-cas-"+reference.hash] = asset
	server := assetServer(t, backend, nil)

	response := fetchBlob(t, server, remoteasset.FetchBlobRequest_builder{
		Uris:                  []string{"https://example.invalid/archive.tar.gz"},
		Qualifiers:            checksumQualifiers(subresourceIntegrity(asset)),
		OldestContentAccepted: futureTimestamp(),
	}.Build())

	if code := codes.Code(response.GetStatus().GetCode()); code != codes.NotFound {
		t.Fatalf("status = %s, want NotFound", code)
	}
}

func TestFetchBlobRejectsInstanceNames(t *testing.T) {
	server := assetServer(t, newMemoryBackend(), nil)
	client := remoteasset.NewFetchClient(testGRPCConn(t, server))

	_, err := client.FetchBlob(t.Context(), remoteasset.FetchBlobRequest_builder{
		InstanceName: "other",
		Uris:         []string{"https://example.invalid/archive.tar.gz"},
	}.Build())
	assertCode(t, err, codes.InvalidArgument)
}
