package server

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	remoteexecution "github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/build/bazel/remote/execution/v2"
)

const maxActionResultObjects = 100_000

type digestReference struct {
	hash string
	size int64
}

type digestCollection struct {
	values []digestReference
	sizes  map[string]int64
}

func newDigestCollection() digestCollection {
	return digestCollection{sizes: make(map[string]int64)}
}

func (c *digestCollection) add(reference digestReference) error {
	if existingSize, exists := c.sizes[reference.hash]; exists {
		if existingSize != reference.size {
			return fmt.Errorf(
				"digest %s has inconsistent sizes %d and %d",
				reference.hash,
				existingSize,
				reference.size,
			)
		}
		return nil
	}
	c.sizes[reference.hash] = reference.size
	c.values = append(c.values, reference)
	return nil
}

type actionResultReferences struct {
	blobs       digestCollection
	trees       digestCollection
	directories digestCollection
}

func newActionResultReferences() actionResultReferences {
	return actionResultReferences{
		blobs:       newDigestCollection(),
		trees:       newDigestCollection(),
		directories: newDigestCollection(),
	}
}

func parseActionResult(data []byte) (actionResultReferences, error) {
	message := &remoteexecution.ActionResult{}
	if err := proto.Unmarshal(data, message); err != nil {
		return actionResultReferences{}, fmt.Errorf("action_result: %w", err)
	}

	references := newActionResultReferences()
	for _, file := range message.GetOutputFiles() {
		if !file.HasDigest() {
			if len(file.GetContents()) == 0 {
				return actionResultReferences{}, errors.New(
					"output_file has neither digest nor inline contents",
				)
			}
			continue
		}
		reference, err := digestReferenceFrom(file.GetDigest(), "output_file.digest")
		if err != nil {
			return actionResultReferences{}, err
		}
		// Inlined contents travel with the action result, so they are not part of
		// the CAS closure even though the digest is still declared.
		if len(file.GetContents()) > 0 {
			continue
		}
		if err := references.blobs.add(reference); err != nil {
			return actionResultReferences{}, err
		}
	}

	for _, directory := range message.GetOutputDirectories() {
		if !directory.HasTreeDigest() && !directory.HasRootDirectoryDigest() {
			return actionResultReferences{}, errors.New(
				"output_directory has no tree or root directory digest",
			)
		}
		if err := addOutputDigest(
			&references.trees,
			directory.GetTreeDigest(),
			"output_directory.tree_digest",
		); err != nil {
			return actionResultReferences{}, err
		}
		if err := addOutputDigest(
			&references.directories,
			directory.GetRootDirectoryDigest(),
			"output_directory.root_directory_digest",
		); err != nil {
			return actionResultReferences{}, err
		}
	}

	addStream := func(digest *remoteexecution.Digest, inline []byte, name string) error {
		if digest == nil {
			return nil
		}
		reference, err := digestReferenceFrom(digest, name)
		if err != nil {
			return err
		}
		if len(inline) > 0 {
			return nil
		}
		return references.blobs.add(reference)
	}
	if err := addStream(
		message.GetStdoutDigest(),
		message.GetStdoutRaw(),
		"action_result.stdout_digest",
	); err != nil {
		return actionResultReferences{}, err
	}
	if err := addStream(
		message.GetStderrDigest(),
		message.GetStderrRaw(),
		"action_result.stderr_digest",
	); err != nil {
		return actionResultReferences{}, err
	}
	return references, nil
}

func addOutputDigest(
	collection *digestCollection,
	digest *remoteexecution.Digest,
	name string,
) error {
	if digest == nil {
		return nil
	}
	reference, err := digestReferenceFrom(digest, name)
	if err != nil {
		return err
	}
	return collection.add(reference)
}
