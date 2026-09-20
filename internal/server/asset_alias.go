package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"

	"google.golang.org/protobuf/proto"

	remoteexecution "github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/build/bazel/remote/execution/v2"
)

// aliasDomain keeps alias keys from colliding with real action digests, which
// share the action-cache namespace.
const aliasDomain = "bazel-github-actions-cache-v2/asset-alias\x00"

// aliasOutputPath labels the alias record for anyone inspecting the cache.
const aliasOutputPath = "asset"

// aliasKey names the entry mapping a non-sha256 checksum onto the sha256 digest
// the content is actually stored under.
func aliasKey(checksum assetChecksum) string {
	sum := sha256.Sum256([]byte(aliasDomain + checksum.algorithm + "\x00" + checksum.hash))
	return hex.EncodeToString(sum[:])
}

// An alias is recorded as an action result referencing the blob, which is not a
// trick so much as a reuse: that is already the cache's "record pointing at CAS
// content" type. The existing validation then applies unchanged, so an alias
// whose blob has been evicted reads as a miss, and in packed mode serving the
// alias renews the retention of the pack holding the blob.
func aliasRecord(reference digestReference) *remoteexecution.ActionResult {
	return remoteexecution.ActionResult_builder{
		OutputFiles: []*remoteexecution.OutputFile{
			remoteexecution.OutputFile_builder{
				Path:   aliasOutputPath,
				Digest: digestProtoFor(reference),
			}.Build(),
		},
	}.Build()
}

func (s *Server) loadAlias(ctx context.Context, checksum assetChecksum) (digestReference, bool) {
	data, err := s.readObjectBytes(ctx, "ac", aliasKey(checksum))
	if err != nil {
		return digestReference{}, false
	}
	record := &remoteexecution.ActionResult{}
	if err := proto.Unmarshal(data, record); err != nil {
		return digestReference{}, false
	}
	files := record.GetOutputFiles()
	if len(files) != 1 {
		return digestReference{}, false
	}
	reference, err := digestReferenceFrom(files[0].GetDigest(), "asset alias")
	if err != nil {
		return digestReference{}, false
	}
	return reference, true
}

func (s *Server) saveAlias(
	ctx context.Context,
	checksum assetChecksum,
	reference digestReference,
) error {
	data, err := proto.Marshal(aliasRecord(reference))
	if err != nil {
		return err
	}
	return s.writeObject(ctx, "ac", aliasKey(checksum), bytes.NewReader(data), int64(len(data)))
}
