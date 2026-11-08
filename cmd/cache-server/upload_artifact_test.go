package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recordedUpload struct {
	name, fileName string
	content        []byte
	timeout        time.Duration
}

func fakeUploader(record *recordedUpload, err error) func(time.Duration) (artifactUploader, error) {
	return func(timeout time.Duration) (artifactUploader, error) {
		record.timeout = timeout
		return uploaderFunc(func(_ context.Context, name, fileName string, content []byte) error {
			record.name, record.fileName, record.content = name, fileName, content
			return err
		}), nil
	}
}

type uploaderFunc func(ctx context.Context, name, fileName string, content []byte) error

func (f uploaderFunc) Upload(ctx context.Context, name, fileName string, content []byte) error {
	return f(ctx, name, fileName, content)
}

func writeLog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.log")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUploadArtifactPublishesTheWholeFile(t *testing.T) {
	path := writeLog(t, "ready\nfatal error: thread exhaustion\n")
	var record recordedUpload
	var stdout bytes.Buffer
	err := uploadArtifactCommand(
		[]string{"--name", "bazel-cache-server-log", "--file", path, "--entry-name", "cache-server.log", "--timeout", "30s"},
		fakeUploader(&record, nil),
		&stdout,
	)
	if err != nil {
		t.Fatal(err)
	}
	if record.name != "bazel-cache-server-log" || record.fileName != "cache-server.log" || record.timeout != 30*time.Second {
		t.Fatalf("upload = %+v", record)
	}
	if string(record.content) != "ready\nfatal error: thread exhaustion\n" {
		t.Fatalf("content = %q", record.content)
	}
	if !strings.Contains(stdout.String(), `artifact "bazel-cache-server-log"`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestUploadArtifactDefaultsTheEntryNameToTheFileName(t *testing.T) {
	path := writeLog(t, "ready\n")
	var record recordedUpload
	err := uploadArtifactCommand([]string{"--name", "log", "--file", path}, fakeUploader(&record, nil), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if record.fileName != "server.log" {
		t.Fatalf("entry name = %q", record.fileName)
	}
}

// A crash dump can be far larger than the useful part of it, and the fatal
// error sits at its head while the tail holds the last goroutines.
func TestUploadArtifactKeepsHeadAndTailOfOversizedFiles(t *testing.T) {
	path := writeLog(t, "HEAD-"+strings.Repeat("x", 1000)+"-TAIL")
	var record recordedUpload
	var stdout bytes.Buffer
	err := uploadArtifactCommand(
		[]string{"--name", "log", "--file", path, "--max-bytes", "100"},
		fakeUploader(&record, nil),
		&stdout,
	)
	if err != nil {
		t.Fatal(err)
	}
	content := string(record.content)
	if !strings.HasPrefix(content, "HEAD-") || !strings.HasSuffix(content, "-TAIL") {
		t.Fatalf("content lost an end: %q", content)
	}
	if !strings.Contains(content, "[... 910 bytes omitted ...]") {
		t.Fatalf("content has no omission marker: %q", content)
	}
	if !strings.Contains(stdout.String(), "910 bytes omitted") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestUploadArtifactRejectsInvalidArguments(t *testing.T) {
	path := writeLog(t, "ready\n")
	for _, args := range [][]string{
		{"--file", path},
		{"--name", "bad name", "--file", path},
		{"--name", "log"},
		{"--name", "log", "--file", path, "extra"},
		{"--name", "log", "--file", path, "--timeout", "0s"},
		{"--name", "log", "--file", path, "--max-bytes", "0"},
		{"--name", "log", "--file", filepath.Dir(path)},
		{"--name", "log", "--file", filepath.Join(t.TempDir(), "missing.log")},
	} {
		var record recordedUpload
		if err := uploadArtifactCommand(args, fakeUploader(&record, nil), &bytes.Buffer{}); err == nil {
			t.Errorf("args %q were accepted", args)
		}
		if record.name != "" {
			t.Errorf("args %q uploaded anyway", args)
		}
	}
}

func TestUploadArtifactReportsUploadFailures(t *testing.T) {
	path := writeLog(t, "ready\n")
	var record recordedUpload
	err := uploadArtifactCommand(
		[]string{"--name", "log", "--file", path},
		fakeUploader(&record, errors.New("FinalizeArtifact returned HTTP 409")),
		&bytes.Buffer{},
	)
	if err == nil || !strings.Contains(err.Error(), "HTTP 409") {
		t.Fatalf("err = %v", err)
	}
}
