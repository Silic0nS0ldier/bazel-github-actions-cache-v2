package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"

	remoteasset "github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/build/bazel/remote/asset/v1"
	remoteexecution "github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/build/bazel/remote/execution/v2"
)

// checksumQualifier carries the expected content hash as Subresource Integrity.
// It is the only qualifier this server acts on: headers are deliberately
// ignored so that no credential is forwarded to an origin on a caller's behalf.
const checksumQualifier = "checksum.sri"

const sha256SRIPrefix = "sha256-"

// maxAssetRedirects bounds a redirect chain from an asset origin.
const maxAssetRedirects = 10

func defaultAssetClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if request.URL.Scheme != "https" {
				return fmt.Errorf("refusing a redirect to %s", request.URL.Scheme)
			}
			if len(via) >= maxAssetRedirects {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

type assetService struct {
	remoteasset.UnimplementedFetchServer
	server *Server
}

// FetchBlob backs Bazel's --remote_downloader, which caches the archives that
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

	hash, err := checksumFromQualifiers(request.GetQualifiers())
	if err != nil {
		return fetchFailure(err), nil
	}
	// Bazel sets this to a future timestamp when a repository rule declares no
	// checksum, precisely to forbid cached content. Nothing here records when an
	// asset was fetched, so the honest answer is always a miss.
	if request.HasOldestContentAccepted() {
		return fetchFailure(errors.New("this server cannot attest to when an asset was fetched")), nil
	}

	if reference, found := s.cachedAsset(ctx, hash); found {
		return fetchSuccess("", reference), nil
	}
	uri, reference, err := s.fetchAsset(ctx, request.GetUris(), hash)
	if err != nil {
		return fetchFailure(err), nil
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
// Asset API puts per-asset outcomes.
func fetchFailure(err error) *remoteasset.FetchBlobResponse {
	return remoteasset.FetchBlobResponse_builder{
		Status: &statuspb.Status{Code: int32(codes.NotFound), Message: safeError(err)},
	}.Build()
}

func digestProtoFor(reference digestReference) *remoteexecution.Digest {
	return remoteexecution.Digest_builder{
		Hash:      reference.hash,
		SizeBytes: reference.size,
	}.Build()
}

// checksumFromQualifiers decodes checksum.sri, which Bazel sends as Subresource
// Integrity rather than hex.
func checksumFromQualifiers(qualifiers []*remoteasset.Qualifier) (string, error) {
	for _, qualifier := range qualifiers {
		if qualifier.GetName() != checksumQualifier {
			continue
		}
		encoded, ok := strings.CutPrefix(qualifier.GetValue(), sha256SRIPrefix)
		if !ok {
			return "", fmt.Errorf("only %s checksums are supported", sha256SRIPrefix)
		}
		sum, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(sum) != sha256.Size {
			return "", fmt.Errorf("%s is not a base64 SHA-256 digest", checksumQualifier)
		}
		return hex.EncodeToString(sum), nil
	}
	return "", fmt.Errorf("a %s qualifier is required to cache an asset", checksumQualifier)
}

// cachedAsset resolves the asset from the cache. Resolving rather than probing
// is deliberate: the client reads the blob over ByteStream immediately
// afterwards, so this both learns the size and warms the local copy.
func (s *Server) cachedAsset(ctx context.Context, hash string) (digestReference, bool) {
	if hash == emptySHA256Digest {
		s.implicitEmptyCASHit()
		return digestReference{hash: hash}, true
	}
	object, err := s.readObject(ctx, "cas", hash)
	if err != nil {
		return digestReference{}, false
	}
	return digestReference{hash: hash, size: object.size}, true
}

func (s *Server) fetchAsset(
	ctx context.Context,
	uris []string,
	hash string,
) (string, digestReference, error) {
	if len(uris) == 0 {
		return "", digestReference{}, errors.New("no uris were supplied")
	}
	var last error
	for _, uri := range uris {
		reference, err := s.fetchAssetFrom(ctx, uri, hash)
		if err == nil {
			return uri, reference, nil
		}
		s.stats.assetFetchErrors.Add(1)
		s.cfg.Logger.Printf("fetching asset failed: %s", safeError(err))
		last = err
	}
	return "", digestReference{}, last
}

func (s *Server) fetchAssetFrom(ctx context.Context, uri, hash string) (digestReference, error) {
	parsed, err := url.Parse(uri)
	if err != nil {
		return digestReference{}, errors.New("uri is not a valid URL")
	}
	if parsed.Scheme != "https" {
		return digestReference{}, fmt.Errorf("refusing to fetch %s over %q", safeURI(parsed), parsed.Scheme)
	}

	path, size, err := s.downloadAsset(ctx, uri, parsed, hash)
	if err != nil {
		return digestReference{}, err
	}
	defer os.Remove(path)

	file, err := os.Open(path)
	if err != nil {
		return digestReference{}, fmt.Errorf("reopen fetched asset: %w", err)
	}
	defer file.Close()
	if err := s.writeObject(ctx, "cas", hash, file, size); err != nil {
		return digestReference{}, fmt.Errorf("publish fetched asset: %w", err)
	}
	return digestReference{hash: hash, size: size}, nil
}

// downloadAsset streams an origin response to a spool file and accepts it only
// if it hashes to the digest the caller asked for.
func (s *Server) downloadAsset(
	ctx context.Context,
	uri string,
	parsed *url.URL,
	hash string,
) (string, int64, error) {
	if err := s.acquire(ctx); err != nil {
		return "", 0, err
	}
	defer s.release()

	fetchCtx, cancel := context.WithTimeout(ctx, s.cfg.BackendTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, uri, nil)
	if err != nil {
		return "", 0, errors.New("uri cannot be requested")
	}
	response, err := s.cfg.AssetClient.Do(request)
	if err != nil {
		return "", 0, fmt.Errorf("fetch %s: %w", safeURI(parsed), err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("fetch %s: HTTP %d", safeURI(parsed), response.StatusCode)
	}
	s.stats.assetDownloads.Add(1)

	file, err := os.CreateTemp(s.cfg.CacheDir, "asset-*")
	if err != nil {
		return "", 0, fmt.Errorf("create asset spool: %w", err)
	}
	path := file.Name()
	defer file.Close()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(path)
		}
	}()

	hasher := sha256.New()
	limit := s.cfg.MaxBlobSize
	size, err := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(response.Body, limit+1))
	if err != nil {
		return "", 0, fmt.Errorf("fetch %s: %w", safeURI(parsed), err)
	}
	if size > limit {
		return "", 0, fmt.Errorf("%s exceeds the configured maximum size", safeURI(parsed))
	}
	if actual := hex.EncodeToString(hasher.Sum(nil)); actual != hash {
		return "", 0, fmt.Errorf("%s hashes to %s, not the requested %s", safeURI(parsed), actual, hash)
	}
	if err := file.Sync(); err != nil {
		return "", 0, fmt.Errorf("sync asset spool: %w", err)
	}
	keep = true
	return path, size, nil
}

// safeURI drops the credentials and query string a URI may carry before it
// reaches a log line or an error returned to the caller.
func safeURI(parsed *url.URL) string {
	return parsed.Scheme + "://" + parsed.Host + parsed.Path
}
