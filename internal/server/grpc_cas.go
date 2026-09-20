package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	remoteexecution "github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/build/bazel/remote/execution/v2"
)

// treePageSize bounds how many directories are packed into one GetTree response.
const treePageSize = 1024

type casService struct {
	remoteexecution.UnimplementedContentAddressableStorageServer
	server *Server
}

// FindMissingBlobs is the reason this server speaks gRPC: Bazel probes the whole
// input closure of a build in a handful of calls instead of one HTTP HEAD each.
// The probes still run against the backend, so they are spread over the same
// concurrency budget as every other backend operation.
func (c *casService) FindMissingBlobs(
	ctx context.Context,
	request *remoteexecution.FindMissingBlobsRequest,
) (*remoteexecution.FindMissingBlobsResponse, error) {
	if err := requireDefaultInstance(request.GetInstanceName()); err != nil {
		return nil, err
	}
	if err := requireSHA256(request.GetDigestFunction()); err != nil {
		return nil, err
	}
	digests := request.GetBlobDigests()
	references := make([]digestReference, len(digests))
	for i, digest := range digests {
		reference, err := requestDigest(digest, "blob_digests")
		if err != nil {
			return nil, err
		}
		references[i] = reference
	}

	c.server.stats.requests.Add(1)
	absent := make([]bool, len(digests))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(c.server.cfg.MaxConcurrent)
	for i, reference := range references {
		if isImplicitEmptyDigest(reference) {
			c.server.implicitEmptyCASHit()
			continue
		}
		group.Go(func() error {
			_, err := c.server.casPresence(groupCtx, reference.hash)
			switch {
			case err == nil:
				return nil
			case errors.Is(err, errCacheMiss):
				absent[i] = true
				return nil
			default:
				return grpcError("cas", reference.hash, err)
			}
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}

	var missing []*remoteexecution.Digest
	for i, isAbsent := range absent {
		if isAbsent {
			missing = append(missing, digests[i])
		}
	}
	return remoteexecution.FindMissingBlobsResponse_builder{MissingBlobDigests: missing}.Build(), nil
}

func (c *casService) BatchUpdateBlobs(
	ctx context.Context,
	request *remoteexecution.BatchUpdateBlobsRequest,
) (*remoteexecution.BatchUpdateBlobsResponse, error) {
	if err := requireDefaultInstance(request.GetInstanceName()); err != nil {
		return nil, err
	}
	if err := requireSHA256(request.GetDigestFunction()); err != nil {
		return nil, err
	}
	blobs := request.GetRequests()
	var total int64
	for _, blob := range blobs {
		total += int64(len(blob.GetData()))
	}
	if total > maxBatchSize {
		return nil, status.Errorf(
			codes.InvalidArgument,
			"batch of %d bytes exceeds the %d byte limit",
			total,
			maxBatchSize,
		)
	}

	c.server.stats.requests.Add(1)
	responses := make([]*remoteexecution.BatchUpdateBlobsResponse_Response, len(blobs))
	for i, blob := range blobs {
		digest := blob.GetDigest()
		responses[i] = remoteexecution.BatchUpdateBlobsResponse_Response_builder{
			Digest: digest,
			Status: status.Convert(c.updateBlob(ctx, blob)).Proto(),
		}.Build()
	}
	return remoteexecution.BatchUpdateBlobsResponse_builder{Responses: responses}.Build(), nil
}

func (c *casService) updateBlob(
	ctx context.Context,
	blob *remoteexecution.BatchUpdateBlobsRequest_Request,
) error {
	if err := requireIdentity(blob.GetCompressor()); err != nil {
		return err
	}
	reference, err := requestDigest(blob.GetDigest(), "digest")
	if err != nil {
		return err
	}
	data := blob.GetData()
	if int64(len(data)) != reference.size {
		return status.Errorf(
			codes.InvalidArgument,
			"blob is %d bytes; digest declares %d",
			len(data),
			reference.size,
		)
	}
	if reference.size > c.server.cfg.MaxBlobSize {
		return status.Error(codes.InvalidArgument, "blob exceeds the configured maximum size")
	}
	err = c.server.writeObject(ctx, "cas", reference.hash, bytes.NewReader(data), reference.size)
	if err != nil {
		return grpcError("cas", reference.hash, err)
	}
	return nil
}

func (c *casService) BatchReadBlobs(
	ctx context.Context,
	request *remoteexecution.BatchReadBlobsRequest,
) (*remoteexecution.BatchReadBlobsResponse, error) {
	if err := requireDefaultInstance(request.GetInstanceName()); err != nil {
		return nil, err
	}
	if err := requireSHA256(request.GetDigestFunction()); err != nil {
		return nil, err
	}
	for _, compressor := range request.GetAcceptableCompressors() {
		if err := requireIdentity(compressor); err != nil {
			return nil, err
		}
	}
	digests := request.GetDigests()
	var total int64
	for _, digest := range digests {
		total += digest.GetSizeBytes()
	}
	if total > maxBatchSize {
		return nil, status.Errorf(
			codes.InvalidArgument,
			"batch of %d bytes exceeds the %d byte limit",
			total,
			maxBatchSize,
		)
	}

	c.server.stats.requests.Add(1)
	responses := make([]*remoteexecution.BatchReadBlobsResponse_Response, len(digests))
	for i, digest := range digests {
		data, err := c.readBlob(ctx, digest)
		responses[i] = remoteexecution.BatchReadBlobsResponse_Response_builder{
			Digest: digest,
			Data:   data,
			Status: status.Convert(err).Proto(),
		}.Build()
	}
	return remoteexecution.BatchReadBlobsResponse_builder{Responses: responses}.Build(), nil
}

func (c *casService) readBlob(ctx context.Context, digest *remoteexecution.Digest) ([]byte, error) {
	reference, err := requestDigest(digest, "digests")
	if err != nil {
		return nil, err
	}
	if isImplicitEmptyDigest(reference) {
		c.server.implicitEmptyCASHit()
		return nil, nil
	}
	data, err := c.server.readObjectBytes(ctx, "cas", reference.hash)
	if err != nil {
		return nil, grpcError("cas", reference.hash, err)
	}
	return data, nil
}

// GetTree walks an output directory that was stored as individual Directory
// objects rather than as a single Tree.
func (c *casService) GetTree(
	request *remoteexecution.GetTreeRequest,
	stream grpc.ServerStreamingServer[remoteexecution.GetTreeResponse],
) error {
	if err := requireDefaultInstance(request.GetInstanceName()); err != nil {
		return err
	}
	if err := requireSHA256(request.GetDigestFunction()); err != nil {
		return err
	}
	if request.GetPageToken() != "" {
		return status.Error(codes.InvalidArgument, "page tokens are not supported")
	}
	root, err := requestDigest(request.GetRootDigest(), "root_digest")
	if err != nil {
		return err
	}

	ctx := stream.Context()
	c.server.stats.requests.Add(1)
	queue := []digestReference{root}
	visited := map[string]struct{}{root.hash: {}}
	page := make([]*remoteexecution.Directory, 0, treePageSize)
	for len(queue) > 0 {
		reference := queue[0]
		queue = queue[1:]
		if len(visited) > maxActionResultObjects {
			return status.Errorf(
				codes.ResourceExhausted,
				"tree references more than %d directories",
				maxActionResultObjects,
			)
		}
		directory, err := c.loadDirectory(ctx, reference)
		if err != nil {
			return err
		}
		page = append(page, directory)
		for _, child := range directory.GetDirectories() {
			childReference, err := requestDigest(child.GetDigest(), "directory_node.digest")
			if err != nil {
				return err
			}
			if _, seen := visited[childReference.hash]; seen {
				continue
			}
			visited[childReference.hash] = struct{}{}
			queue = append(queue, childReference)
		}
		if len(page) == treePageSize {
			if err := sendTreePage(stream, page); err != nil {
				return err
			}
			page = page[:0]
		}
	}
	if len(page) == 0 {
		return nil
	}
	return sendTreePage(stream, page)
}

func (c *casService) loadDirectory(
	ctx context.Context,
	reference digestReference,
) (*remoteexecution.Directory, error) {
	data, err := c.server.readObjectBytes(ctx, "cas", reference.hash)
	if err != nil {
		return nil, grpcError("cas", reference.hash, err)
	}
	directory := &remoteexecution.Directory{}
	if err := proto.Unmarshal(data, directory); err != nil {
		return nil, status.Error(
			codes.NotFound,
			fmt.Sprintf("cas/%s is not a directory", reference.hash),
		)
	}
	return directory, nil
}

func sendTreePage(
	stream grpc.ServerStreamingServer[remoteexecution.GetTreeResponse],
	page []*remoteexecution.Directory,
) error {
	return stream.Send(remoteexecution.GetTreeResponse_builder{Directories: page}.Build())
}
