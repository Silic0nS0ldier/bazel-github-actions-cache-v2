package optimiser

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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
	if got := demand.Signature("cas", "a"); got != "101/1 102/2" {
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

func datedArtifact(id, run int, created time.Time) map[string]any {
	artifact := sameRepoArtifact(id, run)
	artifact["created_at"] = created.UTC().Format(time.RFC3339)
	return artifact
}

func windowSource(t *testing.T, pages [][]any, archive []byte) (*RecordSource, *[]string) {
	t.Helper()
	var downloaded []string
	source := testSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/artifacts") {
			page := 1
			if text := r.URL.Query().Get("page"); text != "" {
				page, _ = strconv.Atoi(text)
			}
			artifacts := []any{}
			if page <= len(pages) {
				artifacts = pages[page-1]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"artifacts": artifacts})
			return
		}
		downloaded = append(downloaded, r.URL.Path)
		_, _ = w.Write(archive)
	}))
	return source, &downloaded
}

// Eviction reads every record in its window, not just the newest few, and only
// trusts the window once the listing has reached past its start.
func TestCollectWindowReadsEveryRecordSinceItOpened(t *testing.T) {
	now := time.Now()
	archive := usageArchive(t, server.UsageReport{Entries: []server.UsageEntry{{Kind: "cas", Digest: "a", Downloads: 1}}})
	source, downloaded := windowSource(t, [][]any{{
		datedArtifact(1, 101, now.Add(-time.Hour)),
		datedArtifact(2, 102, now.Add(-48*time.Hour)),
		datedArtifact(3, 103, now.Add(-100*time.Hour)),
		datedArtifact(4, 104, now.Add(-200*time.Hour)),
	}}, archive)

	plan, recent := NewDemand(), NewDemand()
	collected, coverage, err := source.CollectWindow(context.Background(), plan, 1,
		Window{Since: now.Add(-168 * time.Hour), Recent: recent}, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if collected != 1 || plan.Runs() != 1 {
		t.Fatalf("planned from %d records", collected)
	}
	if !coverage.Complete || coverage.Records != 3 || recent.Runs() != 3 {
		t.Fatalf("coverage = %+v over %d runs", coverage, recent.Runs())
	}
	if len(*downloaded) != 3 {
		t.Fatalf("downloaded %v, want only the three records the window or plan needs", *downloaded)
	}
}

// Retention may have deleted older records, so running out is not proof that
// nothing in the window is missing.
func TestCollectWindowIsIncompleteUntilItReachesPastItsStart(t *testing.T) {
	now := time.Now()
	archive := usageArchive(t, server.UsageReport{})
	source, _ := windowSource(t, [][]any{{datedArtifact(1, 101, now.Add(-time.Hour))}}, archive)

	_, coverage, err := source.CollectWindow(context.Background(), NewDemand(), 25,
		Window{Since: now.Add(-168 * time.Hour), Recent: NewDemand()}, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if coverage.Complete || coverage.Gap == "" {
		t.Fatalf("coverage = %+v, want incomplete", coverage)
	}
}

// A record in the window that cannot be read could be the one that used an
// entry, so the window is not to be trusted.
func TestCollectWindowIsIncompleteWithAnExpiredRecordInside(t *testing.T) {
	now := time.Now()
	archive := usageArchive(t, server.UsageReport{})
	expired := datedArtifact(2, 102, now.Add(-48*time.Hour))
	expired["expired"] = true
	source, _ := windowSource(t, [][]any{{
		datedArtifact(1, 101, now.Add(-time.Hour)),
		expired,
		datedArtifact(3, 103, now.Add(-200*time.Hour)),
	}}, archive)

	_, coverage, err := source.CollectWindow(context.Background(), NewDemand(), 25,
		Window{Since: now.Add(-168 * time.Hour), Recent: NewDemand()}, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if coverage.Complete || !strings.Contains(coverage.Gap, "102") {
		t.Fatalf("coverage = %+v, want the expired record named", coverage)
	}
}

func TestCollectWindowPagesThroughTheListing(t *testing.T) {
	now := time.Now()
	archive := usageArchive(t, server.UsageReport{})
	first := make([]any, 0, artifactsPerPage)
	for index := range artifactsPerPage {
		first = append(first, datedArtifact(index+1, 1000+index, now.Add(-time.Duration(index+1)*time.Minute)))
	}
	source, _ := windowSource(t, [][]any{first, {datedArtifact(500, 5000, now.Add(-200*time.Hour))}}, archive)

	_, coverage, err := source.CollectWindow(context.Background(), NewDemand(), 25,
		Window{Since: now.Add(-168 * time.Hour), Recent: NewDemand()}, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if !coverage.Complete || coverage.Records != artifactsPerPage {
		t.Fatalf("coverage = %+v", coverage)
	}
}

// Every job of a run publishes its own record, usually under the same name.
// Keeping one per run would hide what the others used, and eviction would then
// take it.
func TestCollectKeepsEveryJobsRecordFromOneRun(t *testing.T) {
	now := time.Now()
	first := usageArchive(t, server.UsageReport{Entries: []server.UsageEntry{{Kind: "cas", Digest: "a", Downloads: 1}}})
	second := usageArchive(t, server.UsageReport{Entries: []server.UsageEntry{{Kind: "cas", Digest: "b", PresenceChecks: 1}}})
	source := testSource(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/artifacts") {
			artifacts := []any{}
			if r.URL.Query().Get("page") == "1" {
				artifacts = []any{
					datedArtifact(1, 101, now.Add(-time.Hour)),
					datedArtifact(2, 101, now.Add(-2*time.Hour)),
					datedArtifact(3, 100, now.Add(-200*time.Hour)),
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"artifacts": artifacts})
			return
		}
		if strings.Contains(r.URL.Path, "/2/") {
			_, _ = w.Write(second)
			return
		}
		_, _ = w.Write(first)
	}))

	recent := NewDemand()
	_, coverage, err := source.CollectWindow(context.Background(), NewDemand(), 25,
		Window{Since: now.Add(-168 * time.Hour), Recent: recent}, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if !coverage.Complete || coverage.Records != 2 {
		t.Fatalf("coverage = %+v, want both jobs' records", coverage)
	}
	if !recent.Used("cas", "a") || !recent.Used("cas", "b") {
		t.Fatal("an entry one job of the run used looks unused")
	}
}
