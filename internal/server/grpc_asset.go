package server

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"lukechampine.com/blake3"

	remoteasset "github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/build/bazel/remote/asset/v1"
	remoteexecution "github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/build/bazel/remote/execution/v2"
)

// checksumQualifier carries the expected content hash as Subresource Integrity.
const checksumQualifier = "checksum.sri"

const sha256Algorithm = "sha256"

const blake3DigestSize = 32

// maxAssetRedirects bounds a redirect chain from an asset origin.
const maxAssetRedirects = 10

// checksumAlgorithms are the Subresource Integrity algorithms Bazel can emit. A
// weak one is accepted because it only has to match what the caller declared;
// content is stored under its sha256 either way, and Bazel verifies the same
// checksum itself after the download.
var checksumAlgorithms = map[string]func() hash.Hash{
	"sha1":          sha1.New,
	sha256Algorithm: sha256.New,
	"sha384":        sha512.New384,
	"sha512":        sha512.New,
	"blake3":        func() hash.Hash { return blake3.New(blake3DigestSize, nil) },
}

// assetChecksum is the content hash a caller declared for an asset.
type assetChecksum struct {
	algorithm string
	hash      string
}

func (c assetChecksum) String() string {
	return c.algorithm + "-" + c.hash
}

func defaultAssetClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if request.URL.Scheme != "https" {
				return fmt.Errorf("refusing a redirect to %s", request.URL.Scheme)
			}
			if len(via) >= maxAssetRedirects {
				return errors.New("too many redirects")
			}
			stripSensitiveOnRedirect(request, via)
			return nil
		},
	}
}

type assetService struct {
	remoteasset.UnimplementedFetchServer
	server *Server
}

// FetchBlob backs Bazel's remote downloader, which caches the archives that
// repository rules download. Bazel never pushes those blobs back, so a hit is
// only ever possible if this server fetched the asset itself the first time.
func (a *assetService) FetchBlob(
	ctx context.Context,
	request *remoteasset.FetchBlobRequest,
) (*remoteasset.FetchBlobResponse, error) {
	if err := requireDefaultInstance(request.GetInstanceName()); err != nil {
		return nil, err
	}
	if err := requireSHA256(request.GetDigestFunction()); err != nil {
		return nil, err
	}
	s := a.server
	s.stats.requests.Add(1)
	s.stats.assetRequests.Add(1)
	uris := request.GetUris()

	checksum, err := parseChecksum(request.GetQualifiers())
	if err != nil {
		s.stats.assetRejected.Add(1)
		return fetchFailure(uris, err), nil
	}
	headers, err := parseAssetHeaders(request.GetQualifiers(), len(uris))
	if err != nil {
		s.stats.assetRejected.Add(1)
		return fetchFailure(uris, err), nil
	}
	// Bazel sets this to a future timestamp when a repository rule declares no
	// checksum, precisely to forbid cached content. Nothing here records when an
	// asset was fetched, so the honest answer is always a miss.
	if request.HasOldestContentAccepted() {
		s.stats.assetRejected.Add(1)
		return fetchFailure(uris, errors.New("this cache cannot attest to when content was fetched")), nil
	}

	if reference, found := s.cachedAsset(ctx, checksum); found {
		s.stats.assetHits.Add(1)
		return fetchSuccess("", reference), nil
	}
	uri, reference, err := s.fetchAsset(ctx, uris, checksum, headers)
	if err != nil {
		return fetchFailure(uris, err), nil
	}
	return fetchSuccess(uri, reference), nil
}

func fetchSuccess(uri string, reference digestReference) *remoteasset.FetchBlobResponse {
	return remoteasset.FetchBlobResponse_builder{
		Status:         &statuspb.Status{Code: int32(codes.OK)},
		Uri:            uri,
		BlobDigest:     digestProtoFor(reference),
		DigestFunction: remoteexecution.DigestFunction_SHA256,
	}.Build()
}

// fetchFailure reports a miss inside the response, which is where the Remote
// Asset API puts per-asset outcomes. Bazel logs the message on its own, without
// saying which download produced it, so the resource is always named here.
func fetchFailure(uris []string, err error) *remoteasset.FetchBlobResponse {
	return remoteasset.FetchBlobResponse_builder{
		Status: &statuspb.Status{
			Code:    int32(codes.NotFound),
			Message: fmt.Sprintf("%s: %s", describeAssets(uris), safeError(err)),
		},
	}.Build()
}

func describeAssets(uris []string) string {
	if len(uris) == 0 {
		return "asset with no uri"
	}
	described := safeURIString(uris[0])
	if len(uris) > 1 {
		described = fmt.Sprintf("%s (and %d more)", described, len(uris)-1)
	}
	return described
}

func digestProtoFor(reference digestReference) *remoteexecution.Digest {
	return remoteexecution.Digest_builder{
		Hash:      reference.hash,
		SizeBytes: reference.size,
	}.Build()
}

// parseChecksum decodes checksum.sri, which Bazel sends as Subresource Integrity
// rather than hex.
func parseChecksum(qualifiers []*remoteasset.Qualifier) (assetChecksum, error) {
	for _, qualifier := range qualifiers {
		if qualifier.GetName() != checksumQualifier {
			continue
		}
		value := qualifier.GetValue()
		algorithm, encoded, found := strings.Cut(value, "-")
		if !found || algorithm == "" || encoded == "" {
			return assetChecksum{}, fmt.Errorf("%s %q is not subresource integrity", checksumQualifier, value)
		}
		newHash, supported := checksumAlgorithms[algorithm]
		if !supported {
			return assetChecksum{}, fmt.Errorf("checksum algorithm %q is not supported", algorithm)
		}
		sum, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return assetChecksum{}, fmt.Errorf("%s %q is not base64", checksumQualifier, value)
		}
		if size := newHash().Size(); len(sum) != size {
			return assetChecksum{}, fmt.Errorf(
				"%s %q decodes to %d bytes, not the %d a %s digest needs",
				checksumQualifier, value, len(sum), size, algorithm,
			)
		}
		return assetChecksum{algorithm: algorithm, hash: hex.EncodeToString(sum)}, nil
	}
	return assetChecksum{}, errors.New("no checksum was declared, so the download cannot be cached")
}

// cachedAsset resolves the asset from the cache. A sha256 checksum addresses the
// content store directly; anything else has to go through an alias recorded when
// the asset was first fetched.
func (s *Server) cachedAsset(ctx context.Context, checksum assetChecksum) (digestReference, bool) {
	if checksum.algorithm != sha256Algorithm {
		return s.loadAlias(ctx, checksum)
	}
	if checksum.hash == emptySHA256Digest {
		s.implicitEmptyCASHit()
		return digestReference{hash: checksum.hash}, true
	}
	// Resolving rather than probing is deliberate: the client reads the blob over
	// ByteStream immediately afterwards, so this both learns the size and warms
	// the local copy.
	object, err := s.readObject(ctx, "cas", checksum.hash)
	if err != nil {
		return digestReference{}, false
	}
	return digestReference{hash: checksum.hash, size: object.size}, true
}

func (s *Server) fetchAsset(
	ctx context.Context,
	uris []string,
	checksum assetChecksum,
	headers assetHeaders,
) (string, digestReference, error) {
	if len(uris) == 0 {
		return "", digestReference{}, errors.New("no uris were supplied")
	}
	var last error
	for index, uri := range uris {
		reference, err := s.fetchAssetFrom(ctx, uri, checksum, headers.forURI(index))
		if err == nil {
			return uri, reference, nil
		}
		s.stats.assetFetchErrors.Add(1)
		s.cfg.Logger.Printf("fetching %s failed: %s", safeURIString(uri), safeError(err))
		last = err
	}
	if len(uris) > 1 {
		return "", digestReference{}, fmt.Errorf("all %d uris failed, last: %w", len(uris), last)
	}
	return "", digestReference{}, last
}

func (s *Server) fetchAssetFrom(
	ctx context.Context,
	uri string,
	checksum assetChecksum,
	headers http.Header,
) (digestReference, error) {
	parsed, err := url.Parse(uri)
	if err != nil {
		return digestReference{}, errors.New("uri is not a valid URL")
	}
	if parsed.Scheme != "https" {
		return digestReference{}, fmt.Errorf("refusing to fetch over %q", parsed.Scheme)
	}
	headers, err = permittedHeaders(headers, parsed, s.assetRoutes)
	if err != nil {
		return digestReference{}, err
	}

	path, reference, err := s.downloadAsset(ctx, uri, checksum, headers)
	if err != nil {
		return digestReference{}, err
	}
	defer os.Remove(path)

	file, err := os.Open(path)
	if err != nil {
		return digestReference{}, fmt.Errorf("reopen fetched asset: %w", err)
	}
	defer file.Close()
	if err := s.writeObject(ctx, "cas", reference.hash, file, reference.size); err != nil {
		return digestReference{}, fmt.Errorf("publish fetched asset: %w", err)
	}
	if checksum.algorithm != sha256Algorithm {
		// The blob is cached either way; without the alias the next build simply
		// fetches it again, so this must not fail the request.
		if err := s.saveAlias(ctx, checksum, reference); err != nil {
			s.cfg.Logger.Printf("recording %s alias failed: %s", checksum.algorithm, safeError(err))
		}
	}
	return reference, nil
}

// downloadAsset streams an origin response to a spool file, accepts it only if
// it matches the checksum the caller declared, and reports the sha256 digest the
// content is stored under.
func (s *Server) downloadAsset(
	ctx context.Context,
	uri string,
	checksum assetChecksum,
	headers http.Header,
) (string, digestReference, error) {
	if err := s.acquire(ctx); err != nil {
		return "", digestReference{}, err
	}
	defer s.release()

	fetchCtx, cancel := context.WithTimeout(ctx, s.cfg.BackendTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, uri, nil)
	if err != nil {
		return "", digestReference{}, errors.New("uri cannot be requested")
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := s.cfg.AssetClient.Do(request)
	if err != nil {
		return "", digestReference{}, fmt.Errorf("fetch failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", digestReference{}, fmt.Errorf("origin returned HTTP %d", response.StatusCode)
	}
	s.stats.assetDownloads.Add(1)

	file, err := os.CreateTemp(s.cfg.CacheDir, "asset-*")
	if err != nil {
		return "", digestReference{}, fmt.Errorf("create asset spool: %w", err)
	}
	path := file.Name()
	defer file.Close()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(path)
		}
	}()

	// Storage is addressed by sha256 whatever the caller declared, so the content
	// is hashed twice unless those happen to be the same function.
	canonical := sha256.New()
	declared := canonical
	writers := []io.Writer{file, canonical}
	if checksum.algorithm != sha256Algorithm {
		declared = checksumAlgorithms[checksum.algorithm]()
		writers = append(writers, declared)
	}

	limit := s.cfg.MaxBlobSize
	size, err := io.Copy(io.MultiWriter(writers...), io.LimitReader(response.Body, limit+1))
	if err != nil {
		return "", digestReference{}, fmt.Errorf("fetch failed: %w", err)
	}
	if size > limit {
		return "", digestReference{}, errors.New("content exceeds the configured maximum size")
	}
	actual := assetChecksum{algorithm: checksum.algorithm, hash: hex.EncodeToString(declared.Sum(nil))}
	if actual != checksum {
		return "", digestReference{}, fmt.Errorf("content is %s, not the requested %s", actual, checksum)
	}
	if err := file.Sync(); err != nil {
		return "", digestReference{}, fmt.Errorf("sync asset spool: %w", err)
	}
	keep = true
	return path, digestReference{hash: hex.EncodeToString(canonical.Sum(nil)), size: size}, nil
}

// safeURI drops the credentials and query string a URI may carry before it
// reaches a log line or an error returned to the caller.
func safeURI(parsed *url.URL) string {
	return parsed.Scheme + "://" + parsed.Host + parsed.Path
}

func safeURIString(uri string) string {
	parsed, err := url.Parse(uri)
	if err != nil {
		return "malformed uri"
	}
	return safeURI(parsed)
}
