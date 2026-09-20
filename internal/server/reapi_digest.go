package server

import (
	"fmt"

	remoteexecution "github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/build/bazel/remote/execution/v2"
)

const emptySHA256Digest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// digestReferenceFrom validates a decoded REAPI digest and converts it to the
// internal representation. name identifies the enclosing field in error messages.
func digestReferenceFrom(digest *remoteexecution.Digest, name string) (digestReference, error) {
	if digest == nil {
		return digestReference{}, fmt.Errorf("%s is missing", name)
	}
	if !digestPattern.MatchString(digest.GetHash()) {
		return digestReference{}, fmt.Errorf("%s.hash is not a lowercase SHA-256 digest", name)
	}
	if digest.GetSizeBytes() < 0 {
		return digestReference{}, fmt.Errorf("%s.size_bytes is negative", name)
	}
	return digestReference{hash: digest.GetHash(), size: digest.GetSizeBytes()}, nil
}

func isImplicitEmptyDigest(reference digestReference) bool {
	return reference.hash == emptySHA256Digest && reference.size == 0
}
