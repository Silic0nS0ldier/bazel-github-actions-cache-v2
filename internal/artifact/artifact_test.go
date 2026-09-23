package artifact

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testToken(t *testing.T, scope string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"scp": scope})
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestBackendIDsComeFromTheRuntimeToken(t *testing.T) {
	token := testToken(t, "Actions.Something:x Actions.Results:run-42:job-7")
	runID, jobID, err := backendIDs(token)
	if err != nil {
		t.Fatal(err)
	}
	if runID != "run-42" || jobID != "job-7" {
		t.Fatalf("ids = %q/%q", runID, jobID)
	}

	for _, invalid := range []string{
		"not-a-jwt",
		"header.!!!.signature",
		testToken(t, "Actions.Results:onlyone"),
		testToken(t, "Actions.Other:run:job"),
	} {
		if _, _, err := backendIDs(invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestUploadCreatesPutsAndFinalizes(t *testing.T) {
	var calls []string
	var uploaded []byte
	var finalize map[string]any

	blobs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "put")
		if got := r.Header.Get("x-ms-blob-type"); got != "BlockBlob" {
			t.Errorf("blob type = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		uploaded = body
		w.WriteHeader(http.StatusCreated)
	}))
	defer blobs.Close()

	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/"+servicePrefix)
		calls = append(calls, method)
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Errorf("authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["workflow_run_backend_id"] != "run-42" || body["workflow_job_run_backend_id"] != "job-7" {
			t.Errorf("backend ids = %v", body)
		}
		switch method {
		case "CreateArtifact":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "signed_upload_url": blobs.URL + "/blob"})
		case "FinalizeArtifact":
			finalize = body
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			t.Errorf("unexpected method %q", method)
		}
	}))
	defer service.Close()

	uploader := &Uploader{
		baseURL: service.URL + "/",
		token:   testToken(t, "Actions.Results:run-42:job-7"),
		runID:   "run-42",
		jobID:   "job-7",
		client:  &http.Client{Timeout: 5 * time.Second},
	}
	content := []byte(`{"entries":[]}`)
	if err := uploader.Upload(context.Background(), "cache-usage", "usage.json", content); err != nil {
		t.Fatal(err)
	}

	// Ordering matters: the blob has to exist before it is finalized.
	want := []string{"CreateArtifact", "put", "FinalizeArtifact"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	if finalize["size"].(float64) != float64(len(uploaded)) {
		t.Fatalf("finalized size %v against %d uploaded bytes", finalize["size"], len(uploaded))
	}
	if hash, _ := finalize["hash"].(string); !strings.HasPrefix(hash, "sha256:") {
		t.Fatalf("hash = %q", hash)
	}

	archive, err := zip.NewReader(bytes.NewReader(uploaded), int64(len(uploaded)))
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.File) != 1 || archive.File[0].Name != "usage.json" {
		t.Fatalf("archive = %+v", archive.File)
	}
	entry, err := archive.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer entry.Close()
	stored, _ := io.ReadAll(entry)
	if string(stored) != string(content) {
		t.Fatalf("stored = %q", stored)
	}
}

func TestUploadReportsServiceRefusal(t *testing.T) {
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false})
	}))
	defer service.Close()

	uploader := &Uploader{
		baseURL: service.URL + "/",
		token:   testToken(t, "Actions.Results:run:job"),
		runID:   "run",
		jobID:   "job",
		client:  &http.Client{Timeout: 5 * time.Second},
	}
	err := uploader.Upload(context.Background(), "cache-usage", "usage.json", []byte("{}"))
	if err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("error = %v", err)
	}
}

func TestNewRequiresTheRunnerEnvironment(t *testing.T) {
	t.Setenv("ACTIONS_RESULTS_URL", "")
	t.Setenv("ACTIONS_RUNTIME_TOKEN", "")
	if _, err := New(time.Second); err == nil {
		t.Fatal("accepted an environment with no artifact service")
	}

	t.Setenv("ACTIONS_RESULTS_URL", "https://results.example.invalid/")
	t.Setenv("ACTIONS_RUNTIME_TOKEN", testToken(t, "Actions.Results:run:job"))
	uploader, err := New(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if uploader.baseURL != "https://results.example.invalid/" {
		t.Fatalf("base url = %q", uploader.baseURL)
	}
}
