package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/artifact"
)

const uploadArtifactCommandName = "upload-artifact"

// defaultMaxArtifactBytes bounds what one upload holds in memory. A runtime
// crash dump lists every goroutine, so a burst of tens of thousands of
// requests can produce a log of hundreds of MiB.
const defaultMaxArtifactBytes = 256 * 1024 * 1024

var artifactNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,180}$`)

type artifactUploader interface {
	Upload(ctx context.Context, name, fileName string, content []byte) error
}

func newArtifactUploader(timeout time.Duration) (artifactUploader, error) {
	return artifact.New(timeout)
}

// uploadArtifactCommand publishes one file as a job artifact. The post step
// runs it to keep the server log, which the server cannot publish itself when
// it has crashed.
func uploadArtifactCommand(
	args []string,
	newUploader func(time.Duration) (artifactUploader, error),
	stdout io.Writer,
) error {
	flags := flag.NewFlagSet(uploadArtifactCommandName, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var (
		name      = flags.String("name", "", "artifact name")
		file      = flags.String("file", "", "file to publish")
		entryName = flags.String("entry-name", "", "file name inside the artifact; defaults to the file's base name")
		timeout   = flags.Duration("timeout", 2*time.Minute, "timeout for the whole upload")
		maxBytes  = flags.Int64("max-bytes", defaultMaxArtifactBytes, "larger files keep their head and tail")
	)
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("%s: %w", uploadArtifactCommandName, err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%s: unexpected arguments %q", uploadArtifactCommandName, flags.Args())
	}
	if !artifactNamePattern.MatchString(*name) {
		return fmt.Errorf("%s: --name must match %s", uploadArtifactCommandName, artifactNamePattern)
	}
	if *file == "" {
		return fmt.Errorf("%s: --file is required", uploadArtifactCommandName)
	}
	if *timeout <= 0 || *maxBytes <= 0 {
		return fmt.Errorf("%s: --timeout and --max-bytes must be positive", uploadArtifactCommandName)
	}
	if *entryName == "" {
		*entryName = filepath.Base(*file)
	}

	content, omitted, err := readBounded(*file, *maxBytes)
	if err != nil {
		return fmt.Errorf("%s: %w", uploadArtifactCommandName, err)
	}
	uploader, err := newUploader(*timeout)
	if err != nil {
		return fmt.Errorf("%s: %w", uploadArtifactCommandName, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := uploader.Upload(ctx, *name, *entryName, content); err != nil {
		return fmt.Errorf("%s: %w", uploadArtifactCommandName, err)
	}
	if omitted > 0 {
		fmt.Fprintf(stdout, "published %s as artifact %q with %d bytes omitted from the middle\n", *entryName, *name, omitted)
	} else {
		fmt.Fprintf(stdout, "published %s as artifact %q\n", *entryName, *name)
	}
	return nil
}

// readBounded returns the whole file, or its head and tail around a marker when
// it exceeds limit. Both ends matter for a crash: the head holds the fatal
// error and the tail holds the last goroutines.
func readBounded(path string, limit int64) ([]byte, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, errors.New("--file must name a regular file")
	}
	size := info.Size()
	if size <= limit {
		content, err := io.ReadAll(io.LimitReader(file, limit))
		return content, 0, err
	}

	headSize := limit / 2
	tailSize := limit - headSize
	omitted := size - headSize - tailSize
	marker := fmt.Sprintf("\n\n[... %d bytes omitted ...]\n\n", omitted)
	content := make([]byte, 0, limit+int64(len(marker)))
	head := make([]byte, headSize)
	if _, err := io.ReadFull(file, head); err != nil {
		return nil, 0, err
	}
	tail := make([]byte, tailSize)
	if _, err := file.ReadAt(tail, size-tailSize); err != nil {
		return nil, 0, err
	}
	content = append(content, head...)
	content = append(content, marker...)
	content = append(content, tail...)
	return content, omitted, nil
}
