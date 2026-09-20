package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	remoteexecution "github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/build/bazel/remote/execution/v2"
)

var (
	treeRootField     = treeFieldNumber("root")
	treeChildrenField = treeFieldNumber("children")
)

func treeFieldNumber(name protoreflect.Name) protowire.Number {
	field := (&remoteexecution.Tree{}).ProtoReflect().Descriptor().Fields().ByName(name)
	if field == nil {
		panic("build.bazel.remote.execution.v2.Tree has no field " + string(name))
	}
	return protowire.Number(field.Number())
}

type parsedDirectory struct {
	files       []digestReference
	directories []digestReference
}

func parseDirectory(data []byte) (parsedDirectory, error) {
	message := &remoteexecution.Directory{}
	if err := proto.Unmarshal(data, message); err != nil {
		return parsedDirectory{}, fmt.Errorf("directory: %w", err)
	}
	var directory parsedDirectory
	for _, file := range message.GetFiles() {
		reference, err := digestReferenceFrom(file.GetDigest(), "file_node.digest")
		if err != nil {
			return parsedDirectory{}, err
		}
		directory.files = append(directory.files, reference)
	}
	for _, child := range message.GetDirectories() {
		reference, err := digestReferenceFrom(child.GetDigest(), "directory_node.digest")
		if err != nil {
			return parsedDirectory{}, err
		}
		directory.directories = append(directory.directories, reference)
	}
	return directory, nil
}

func parseTree(data []byte) ([]digestReference, error) {
	roots, children, err := splitTree(data)
	if err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		return nil, errors.New("tree has no root directory")
	}
	if len(roots) > 1 {
		return nil, errors.New("tree.root is repeated")
	}
	childDigests := make(map[string]int64, len(children))
	for _, child := range children {
		sum := sha256.Sum256(child)
		childDigests[hex.EncodeToString(sum[:])] = int64(len(child))
	}

	files := newDigestCollection()
	for _, encoded := range slices.Concat(roots, children) {
		directory, err := parseDirectory(encoded)
		if err != nil {
			return nil, fmt.Errorf("tree.directory: %w", err)
		}
		for _, file := range directory.files {
			if err := files.add(file); err != nil {
				return nil, err
			}
		}
		for _, child := range directory.directories {
			if size, exists := childDigests[child.hash]; !exists || size != child.size {
				return nil, fmt.Errorf(
					"tree references missing child directory %s/%d",
					child.hash,
					child.size,
				)
			}
		}
	}
	return files.values, nil
}

// splitTree returns the encoded root and child Directory messages of a Tree. A
// Directory digest covers the exact bytes its producer emitted, so the embedded
// messages have to be hashed as received; re-encoding the decoded messages is not
// guaranteed to reproduce those bytes.
func splitTree(data []byte) (roots [][]byte, children [][]byte, err error) {
	for len(data) > 0 {
		number, wireType, consumed := protowire.ConsumeTag(data)
		if consumed < 0 {
			return nil, nil, protowire.ParseError(consumed)
		}
		data = data[consumed:]

		if wireType == protowire.BytesType && (number == treeRootField || number == treeChildrenField) {
			value, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return nil, nil, protowire.ParseError(n)
			}
			if number == treeRootField {
				roots = append(roots, value)
			} else {
				children = append(children, value)
			}
			consumed = n
		} else {
			consumed = protowire.ConsumeFieldValue(number, wireType, data)
			if consumed < 0 {
				return nil, nil, protowire.ParseError(consumed)
			}
		}
		data = data[consumed:]
	}
	return roots, children, nil
}
