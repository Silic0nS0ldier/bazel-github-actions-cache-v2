package server

import (
	"bytes"
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	remoteexecution "github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/build/bazel/remote/execution/v2"
	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/build/bazel/semver"
	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/google/bytestream"
)

// maxBatchSize bounds a single BatchUpdateBlobs or BatchReadBlobs payload.
// Anything larger has to use the ByteStream API, which streams in chunks instead
// of buffering a whole gRPC message.
const maxBatchSize = 4 * 1024 * 1024

// grpcMessageHeadroom leaves room for the framing around a maximum-size batch.
const grpcMessageHeadroom = 1024 * 1024

// GRPCHandler returns a gRPC server exposing the cache half of the Bazel remote
// execution API. Remote execution itself is not offered, so Bazel treats the
// endpoint as a cache-only backend.
func (s *Server) GRPCHandler() *grpc.Server {
	handler := grpc.NewServer(
		grpc.MaxRecvMsgSize(maxBatchSize+grpcMessageHeadroom),
		grpc.MaxSendMsgSize(maxBatchSize+grpcMessageHeadroom),
	)
	remoteexecution.RegisterCapabilitiesServer(handler, &capabilitiesService{server: s})
	remoteexecution.RegisterActionCacheServer(handler, &actionCacheService{server: s})
	remoteexecution.RegisterContentAddressableStorageServer(handler, &casService{server: s})
	bytestream.RegisterByteStreamServer(handler, &byteStreamService{server: s})
	return handler
}

// requireDefaultInstance rejects instance names. The cache namespace is fixed by
// the configured key prefix, so honouring an instance name would silently share
// one namespace between callers that asked for different ones.
func requireDefaultInstance(name string) error {
	if name != "" {
		return status.Errorf(codes.InvalidArgument, "instance_name %q is not supported", name)
	}
	return nil
}

func requireSHA256(function remoteexecution.DigestFunction_Value) error {
	switch function {
	case remoteexecution.DigestFunction_UNKNOWN, remoteexecution.DigestFunction_SHA256:
		return nil
	default:
		return status.Errorf(codes.InvalidArgument, "digest function %s is not supported", function)
	}
}

func requireIdentity(compressor remoteexecution.Compressor_Value) error {
	if compressor != remoteexecution.Compressor_IDENTITY {
		return status.Errorf(codes.InvalidArgument, "compressor %s is not supported", compressor)
	}
	return nil
}

func requestDigest(digest *remoteexecution.Digest, field string) (digestReference, error) {
	reference, err := digestReferenceFrom(digest, field)
	if err != nil {
		return digestReference{}, status.Error(codes.InvalidArgument, err.Error())
	}
	return reference, nil
}

// grpcError translates a cache outcome into the status a REAPI client expects.
func grpcError(kind, digest string, err error) error {
	switch {
	case errors.Is(err, errCacheMiss):
		return status.Errorf(codes.NotFound, "%s/%s is not in the cache", kind, digest)
	case errors.Is(err, errPayloadMismatch):
		return status.Errorf(codes.InvalidArgument, "payload does not hash to %s", digest)
	case errors.Is(err, errPayloadTruncated):
		return status.Errorf(codes.InvalidArgument, "payload size does not match %s/%s", kind, digest)
	case errors.Is(err, errLocalFailure):
		return status.Error(codes.Internal, "local cache unavailable")
	default:
		return status.Error(codes.Unavailable, "cache backend unavailable")
	}
}

type capabilitiesService struct {
	remoteexecution.UnimplementedCapabilitiesServer
	server *Server
}

func (c *capabilitiesService) GetCapabilities(
	_ context.Context,
	request *remoteexecution.GetCapabilitiesRequest,
) (*remoteexecution.ServerCapabilities, error) {
	if err := requireDefaultInstance(request.GetInstanceName()); err != nil {
		return nil, err
	}
	c.server.stats.requests.Add(1)
	identity := []remoteexecution.Compressor_Value{remoteexecution.Compressor_IDENTITY}
	return remoteexecution.ServerCapabilities_builder{
		CacheCapabilities: remoteexecution.CacheCapabilities_builder{
			DigestFunctions: []remoteexecution.DigestFunction_Value{
				remoteexecution.DigestFunction_SHA256,
			},
			ActionCacheUpdateCapabilities: remoteexecution.ActionCacheUpdateCapabilities_builder{
				UpdateEnabled: c.server.cfg.WriteEnabled,
			}.Build(),
			MaxBatchTotalSizeBytes: maxBatchSize,
			MaxCasBlobSizeBytes:    c.server.cfg.MaxBlobSize,
			// The cache stores action results verbatim and never resolves a
			// symlink, so it imposes no restriction on their targets.
			SymlinkAbsolutePathStrategy:     remoteexecution.SymlinkAbsolutePathStrategy_ALLOWED,
			SupportedCompressors:            identity,
			SupportedBatchUpdateCompressors: identity,
		}.Build(),
		LowApiVersion:  semVer(2, 0),
		HighApiVersion: semVer(2, 3),
	}.Build(), nil
}

func semVer(major, minor int32) *semver.SemVer {
	return semver.SemVer_builder{Major: major, Minor: minor}.Build()
}

type actionCacheService struct {
	remoteexecution.UnimplementedActionCacheServer
	server *Server
}

func (a *actionCacheService) GetActionResult(
	ctx context.Context,
	request *remoteexecution.GetActionResultRequest,
) (*remoteexecution.ActionResult, error) {
	if err := requireDefaultInstance(request.GetInstanceName()); err != nil {
		return nil, err
	}
	if err := requireSHA256(request.GetDigestFunction()); err != nil {
		return nil, err
	}
	reference, err := requestDigest(request.GetActionDigest(), "action_digest")
	if err != nil {
		return nil, err
	}
	a.server.stats.requests.Add(1)
	data, err := a.server.readObjectBytes(ctx, "ac", reference.hash)
	if err != nil {
		return nil, grpcError("ac", reference.hash, err)
	}
	result := &remoteexecution.ActionResult{}
	if err := proto.Unmarshal(data, result); err != nil {
		// The closure validation already parsed these bytes, so reaching here
		// means the stored entry is unusable rather than merely absent.
		a.server.stats.invalidActionResults.Add(1)
		return nil, status.Errorf(codes.NotFound, "ac/%s is not in the cache", reference.hash)
	}
	return result, nil
}

func (a *actionCacheService) UpdateActionResult(
	ctx context.Context,
	request *remoteexecution.UpdateActionResultRequest,
) (*remoteexecution.ActionResult, error) {
	if err := requireDefaultInstance(request.GetInstanceName()); err != nil {
		return nil, err
	}
	if err := requireSHA256(request.GetDigestFunction()); err != nil {
		return nil, err
	}
	reference, err := requestDigest(request.GetActionDigest(), "action_digest")
	if err != nil {
		return nil, err
	}
	result := request.GetActionResult()
	if result == nil {
		return nil, status.Error(codes.InvalidArgument, "action_result is required")
	}
	data, err := proto.Marshal(result)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "action_result cannot be encoded: %v", err)
	}
	if int64(len(data)) > a.server.cfg.MaxBlobSize {
		return nil, status.Error(codes.InvalidArgument, "action result exceeds the configured maximum size")
	}
	a.server.stats.requests.Add(1)
	err = a.server.writeObject(ctx, "ac", reference.hash, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, grpcError("ac", reference.hash, err)
	}
	return result, nil
}
