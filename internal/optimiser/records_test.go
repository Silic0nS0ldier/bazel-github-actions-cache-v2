package optimiser

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/server"
)

func usageArchive(t *testing.T, report server.UsageReport) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	entry, err := archive.Create(usageFileName)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(encoded); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func testSource(t *testing.T, handler http.Handler) *RecordSource {
	t.Helper()
	service := httptest.NewServer(handler)
	t.Cleanup(service.Close)
	base, err := url.Parse(service.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &RecordSource{
		baseURL:    base,
		repository: "owner/repo",
		token:      "test-token",
		artifact:   "bazel-cache-usage",
		client:     &http.Client{Timeout: 5 * time.Second},
	}
}

func TestCollectFoldsRecordsKeyedByRun(t *testing.T) {
	archive := usageArchive(t, server.UsageReport{Entries: []server.UsageEntry{
		{Kind: "cas", Digest: "a", Downloads: 1},
	}})
	source := testSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("authorization = %q", got)
		}
		if strings.HasSuffix(r.URL.Path, "/artifacts") {
			if got := r.URL.Query().Get("name"); got != "bazel-cache-usage" {
				t.Errorf("name filter = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"artifacts": []any{
				sameRepoArtifact(1, 101),
				sameRepoArtifact(2, 102),
			}})
			return
		}
		_, _ = w.Write(archive)
	}))

	demand := NewDemand()
	collected, err := source.Collect(context.Background(), demand, 10, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if collected != 2 || demand.Runs() != 2 {
		t.Fatalf("collected %d records over %d runs", collected, demand.Runs())
	}
	if got := demand.Signature("cas", "a"); got != "101 102" {
		t.Fatalf("signature = %q", got)
	}
}

// A run started from a fork is outside the repository's trust boundary, so its
// record must not steer the layout.
func TestCollectIgnoresForkRuns(t *testing.T) {
	source := testSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/artifacts") {
			t.Errorf("downloaded a fork artifact from %s", r.URL.Path)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"artifacts": []any{
			map[string]any{
				"id": 1, "expired": false, "size_in_bytes": 100,
				"workflow_run": map[string]any{"id": 101, "repository_id": 7, "head_repository_id": 9},
			},
		}})
	}))

	demand := NewDemand()
	collected, err := source.Collect(context.Background(), demand, 10, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if collected != 0 {
		t.Fatalf("collected %d fork records", collected)
	}
}

// One unreadable record must not cost the whole window.
func TestCollectSkipsUnreadableRecords(t *testing.T) {
	good := usageArchive(t, server.UsageReport{Entries: []server.UsageEntry{
		{Kind: "cas", Digest: "a", Downloads: 1},
	}})
	source := testSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/artifacts") {
			_ = json.NewEncoder(w).Encode(map[string]any{"artifacts": []any{
				sameRepoArtifact(1, 101),
				sameRepoArtifact(2, 102),
			}})
			return
		}
		if strings.Contains(r.URL.Path, "/1/") {
			_, _ = w.Write([]byte("not a zip"))
			return
		}
		_, _ = w.Write(good)
	}))

	warnings := 0
	demand := NewDemand()
	collected, err := source.Collect(context.Background(), demand, 10, func(string, ...any) { warnings++ })
	if err != nil {
		t.Fatal(err)
	}
	if collected != 1 || warnings != 1 {
		t.Fatalf("collected %d records with %d warnings", collected, warnings)
	}
}

func TestReadUsageArchiveRejectsAnArchiveWithoutARecord(t *testing.T) {
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	if _, err := archive.Create("something-else.json"); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readUsageArchive(buffer.Bytes()); err == nil {
		t.Fatal("accepted an archive with no usage record")
	}
}

func sameRepoArtifact(id, run int) map[string]any {
	return map[string]any{
		"id": id, "expired": false, "size_in_bytes": 100,
		"workflow_run": map[string]any{"id": run, "repository_id": 7, "head_repository_id": 7},
	}
}
