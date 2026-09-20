package server

import (
	"context"
	"io"
	"strconv"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/google/bytestream"
)

// byteStreamChunkSize is the payload size of one Read response. It trades gRPC
// framing overhead against the memory a single in-flight message costs.
const byteStreamChunkSize = 256 * 1024

type byteStreamService struct {
	bytestream.UnimplementedByteStreamServer
	server *Server
}

func (b *byteStreamService) Read(
	request *bytestream.ReadRequest,
	stream grpc.ServerStreamingServer[bytestream.ReadResponse],
) error {
	resource, err := parseReadResource(request.GetResourceName())
	if err != nil {
		return err
	}
	offset, limit := request.GetReadOffset(), request.GetReadLimit()
	if offset < 0 || limit < 0 {
		return status.Error(codes.InvalidArgument, "read_offset and read_limit must not be negative")
	}

	b.server.stats.requests.Add(1)
	if isImplicitEmptyDigest(resource) {
		b.server.implicitEmptyCASHit()
		return nil
	}
	file, size, err := b.server.openObject(stream.Context(), "cas", resource.hash)
	if err != nil {
		return grpcError("cas", resource.hash, err)
	}
	defer file.Close()

	if offset > size {
		return status.Errorf(codes.OutOfRange, "read_offset %d is past the end of the blob", offset)
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		b.server.cfg.Logger.Printf("seek cache object cas/%s: %v", resource.hash, err)
		return status.Error(codes.Internal, "local cache unavailable")
	}
	var reader io.Reader = file
	if limit > 0 {
		reader = io.LimitReader(reader, limit)
	}

	buffer := make([]byte, byteStreamChunkSize)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			response := bytestream.ReadResponse_builder{Data: buffer[:n]}.Build()
			if sendErr := stream.Send(response); sendErr != nil {
				return sendErr
			}
			b.server.stats.bytesServed.Add(uint64(n))
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			b.server.cfg.Logger.Printf("serve cache object cas/%s: %v", resource.hash, err)
			return status.Error(codes.Internal, "local cache unavailable")
		}
	}
}

func (b *byteStreamService) Write(
	stream grpc.ClientStreamingServer[bytestream.WriteRequest, bytestream.WriteResponse],
) error {
	first, err := stream.Recv()
	if err == io.EOF {
		return status.Error(codes.InvalidArgument, "write stream contained no requests")
	}
	if err != nil {
		return err
	}
	resource, err := parseWriteResource(first.GetResourceName())
	if err != nil {
		return err
	}
	if first.GetWriteOffset() != 0 {
		return status.Error(codes.InvalidArgument, "resumed writes are not supported; write from offset 0")
	}

	b.server.stats.requests.Add(1)
	if resource.size > b.server.cfg.MaxBlobSize {
		return status.Error(codes.InvalidArgument, "blob exceeds the configured maximum size")
	}
	body := &writeStreamReader{
		stream:   stream,
		pending:  first.GetData(),
		offset:   int64(len(first.GetData())),
		finished: first.GetFinishWrite(),
	}
	err = b.server.writeObject(stream.Context(), "cas", resource.hash, body, resource.size)
	if err != nil {
		// A stream-level failure is more specific than the truncation the
		// object writer inferred from the short read, so it wins.
		if body.err != nil {
			return body.err
		}
		return grpcError("cas", resource.hash, err)
	}
	return stream.SendAndClose(
		bytestream.WriteResponse_builder{CommittedSize: resource.size}.Build(),
	)
}

// QueryWriteStatus reports that nothing is in flight. Uploads are spooled and
// only become visible once complete, so a client that lost its stream has to
// start again rather than resume.
func (b *byteStreamService) QueryWriteStatus(
	_ context.Context,
	request *bytestream.QueryWriteStatusRequest,
) (*bytestream.QueryWriteStatusResponse, error) {
	b.server.stats.requests.Add(1)
	if _, err := parseWriteResource(request.GetResourceName()); err != nil {
		return nil, err
	}
	return nil, status.Error(codes.NotFound, "no write is in progress for this resource")
}

// writeStreamReader adapts a ByteStream write stream to an io.Reader so that an
// upload is spooled and verified exactly once, by the shared object writer.
type writeStreamReader struct {
	stream   grpc.ClientStreamingServer[bytestream.WriteRequest, bytestream.WriteResponse]
	pending  []byte
	offset   int64
	finished bool
	err      error
}

func (r *writeStreamReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.finished {
			return 0, io.EOF
		}
		request, err := r.stream.Recv()
		if err == io.EOF {
			r.err = status.Error(codes.InvalidArgument, "write stream ended before finish_write")
			return 0, r.err
		}
		if err != nil {
			r.err = err
			return 0, err
		}
		if request.GetWriteOffset() != r.offset {
			r.err = status.Errorf(
				codes.InvalidArgument,
				"write_offset %d does not continue from %d",
				request.GetWriteOffset(),
				r.offset,
			)
			return 0, r.err
		}
		r.pending = request.GetData()
		r.offset += int64(len(r.pending))
		r.finished = request.GetFinishWrite()
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// parseReadResource reads `{instance_name}/blobs/{hash}/{size}`.
func parseReadResource(name string) (digestReference, error) {
	segments := strings.Split(name, "/")
	for i, segment := range segments {
		switch segment {
		case "blobs":
			return blobFromSegments(strings.Join(segments[:i], "/"), segments[i+1:])
		case "compressed-blobs":
			return digestReference{}, errCompressedResource
		}
	}
	return digestReference{}, unparsableResource(name)
}

// parseWriteResource reads
// `{instance_name}/uploads/{uuid}/blobs/{hash}/{size}{/optional_metadata}`.
func parseWriteResource(name string) (digestReference, error) {
	segments := strings.Split(name, "/")
	for i, segment := range segments {
		if segment != "uploads" || i+2 >= len(segments) {
			continue
		}
		switch segments[i+2] {
		case "blobs":
			return blobFromSegments(strings.Join(segments[:i], "/"), segments[i+3:])
		case "compressed-blobs":
			return digestReference{}, errCompressedResource
		}
	}
	return digestReference{}, unparsableResource(name)
}

func blobFromSegments(instance string, segments []string) (digestReference, error) {
	if err := requireDefaultInstance(instance); err != nil {
		return digestReference{}, err
	}
	if len(segments) < 2 {
		return digestReference{}, status.Error(codes.InvalidArgument, "resource name has no blob digest")
	}
	if !digestPattern.MatchString(segments[0]) {
		return digestReference{}, status.Error(
			codes.InvalidArgument,
			"resource name does not carry a lowercase SHA-256 digest",
		)
	}
	size, err := strconv.ParseInt(segments[1], 10, 64)
	if err != nil || size < 0 {
		return digestReference{}, status.Error(codes.InvalidArgument, "resource name has no valid blob size")
	}
	return digestReference{hash: segments[0], size: size}, nil
}

var errCompressedResource = status.Error(codes.InvalidArgument, "compressed blobs are not supported")

func unparsableResource(name string) error {
	return status.Errorf(codes.InvalidArgument, "resource name %q does not name a CAS blob", name)
}
