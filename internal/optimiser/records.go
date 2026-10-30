package optimiser

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cre4ture/bazel-github-actions-cache-v2/internal/server"
)

// maxRecordBytes bounds one decompressed usage record. The archive comes from
// a workflow run, so it is only as trustworthy as whoever could start one.
const maxRecordBytes = 64 * 1024 * 1024

// usageFileName is the entry each job's teardown writes into its artifact.
const usageFileName = "cache-usage.json"

// RecordSource reads the usage records jobs published as workflow artifacts.
// It needs actions: read and nothing more.
type RecordSource struct {
	baseURL    *url.URL
	repository string
	token      string
	artifact   string
	client     *http.Client
}

func NewRecordSource(artifact string, timeout time.Duration) (*RecordSource, error) {
	repository := os.Getenv("GITHUB_REPOSITORY")
	if len(strings.Split(repository, "/")) != 2 || strings.HasPrefix(repository, "/") || strings.HasSuffix(repository, "/") {
		return nil, errors.New("GITHUB_REPOSITORY must be owner/repository")
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, errors.New("GITHUB_TOKEN is required to read usage records")
	}
	baseURLText := os.Getenv("GITHUB_API_URL")
	if baseURLText == "" {
		baseURLText = "https://api.github.com"
	}
	baseURL, err := url.Parse(baseURLText)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, fmt.Errorf("invalid GITHUB_API_URL: %w", err)
	}
	if artifact == "" {
		return nil, errors.New("the usage artifact name is required")
	}
	return &RecordSource{
		baseURL:    baseURL,
		repository: repository,
		token:      token,
		artifact:   artifact,
		client:     &http.Client{Timeout: timeout},
	}, nil
}

type artifactListResponse struct {
	Artifacts []artifactSummary `json:"artifacts"`
}

type artifactSummary struct {
	ID          int64     `json:"id"`
	Expired     bool      `json:"expired"`
	SizeInBytes int64     `json:"size_in_bytes"`
	CreatedAt   time.Time `json:"created_at"`
	WorkflowRun struct {
		ID               int64 `json:"id"`
		RepositoryID     int64 `json:"repository_id"`
		HeadRepositoryID int64 `json:"head_repository_id"`
	} `json:"workflow_run"`
}

const (
	artifactsPerPage = 100
	// maxArtifactPages bounds how far back a listing pages. A repository busy
	// enough to exceed it within the eviction window gets no eviction rather
	// than an unbounded pass.
	maxArtifactPages = 50
)

// Window asks for every record since a point in time, on top of the newest
// ones a plan is built from.
type Window struct {
	Since  time.Time
	Recent *Demand
}

// Coverage says whether Recent holds every record since the window opened.
// Only then does "no record used this entry" mean nothing used it.
type Coverage struct {
	Records  int
	Complete bool
	Gap      string
}

// Collect folds the newest records into demand and reports how many it read.
// A record that cannot be read is skipped rather than fatal: the plan simply
// rests on a smaller window.
func (s *RecordSource) Collect(ctx context.Context, demand *Demand, limit int, warn func(string, ...any)) (int, error) {
	collected, _, err := s.CollectWindow(ctx, demand, limit, Window{}, warn)
	return collected, err
}

// CollectWindow is Collect, and also folds every record created since
// window.Since into window.Recent.
func (s *RecordSource) CollectWindow(ctx context.Context, demand *Demand, limit int, window Window, warn func(string, ...any)) (int, Coverage, error) {
	summaries, reached, err := s.list(ctx, limit, window.Since)
	if err != nil {
		return 0, Coverage{}, err
	}
	coverage := Coverage{}
	gap := func(reason string) {
		if coverage.Gap == "" {
			coverage.Gap = reason
		}
	}
	if !window.Since.IsZero() && !reached {
		gap(fmt.Sprintf("the usage records listed do not reach back to %s", window.Since.Format(time.RFC3339)))
	}
	collected := 0
	for _, summary := range summaries {
		run := summary.WorkflowRun.ID
		inWindow := !window.Since.IsZero() && !summary.CreatedAt.Before(window.Since)
		if !window.Since.IsZero() && summary.CreatedAt.IsZero() {
			gap(fmt.Sprintf("the record from run %d has no creation time", run))
		}
		if summary.Expired || summary.SizeInBytes > maxRecordBytes {
			if inWindow {
				gap(fmt.Sprintf("the record from run %d has expired or is too large", run))
			}
			continue
		}
		forPlan := collected < limit
		if !forPlan && !inWindow {
			continue
		}
		report, err := s.download(ctx, summary)
		if err != nil {
			warn("skipping usage record from run %d: %v", run, err)
			if inWindow {
				gap(fmt.Sprintf("the record from run %d could not be read", run))
			}
			continue
		}
		// Every job publishes its own record, often under the same name as the
		// other jobs of its run, and each job restores packs for itself.
		id := recordID(summary)
		if forPlan {
			demand.Observe(id, report)
			collected++
		}
		if inWindow && window.Recent != nil {
			window.Recent.Observe(id, report)
			coverage.Records++
		}
	}
	coverage.Complete = !window.Since.IsZero() && coverage.Gap == ""
	return collected, coverage, nil
}

// list returns the records worth reading, newest first: at least limit usable
// ones, and everything since since. It reports whether the listing reached a
// record older than since, which is the only proof nothing in between is
// missing. Running out of records proves nothing, since retention may have
// deleted older ones.
func (s *RecordSource) list(ctx context.Context, limit int, since time.Time) ([]artifactSummary, bool, error) {
	selected := make([]artifactSummary, 0, limit)
	seen := make(map[int64]struct{})
	usable := 0
	reached := false
	for page := 1; page <= maxArtifactPages; page++ {
		listed, err := s.listPage(ctx, page)
		if err != nil {
			return nil, false, err
		}
		for _, summary := range listed {
			if !since.IsZero() && !summary.CreatedAt.IsZero() && summary.CreatedAt.Before(since) {
				reached = true
			}
			// A run started from a fork is outside this repository's trust
			// boundary and could otherwise steer the layout by publishing a
			// shaped record.
			if summary.WorkflowRun.HeadRepositoryID != summary.WorkflowRun.RepositoryID {
				continue
			}
			// Pages can shift while being read, so the same record may be listed
			// twice; it must not count twice. Records of other jobs in the same
			// run are not duplicates: dropping them would hide what those jobs
			// used, and eviction would take it.
			if _, duplicate := seen[summary.ID]; duplicate {
				continue
			}
			seen[summary.ID] = struct{}{}
			selected = append(selected, summary)
			if !summary.Expired && summary.SizeInBytes <= maxRecordBytes {
				usable++
			}
		}
		if len(listed) < artifactsPerPage || usable >= limit && (since.IsZero() || reached) {
			break
		}
	}
	sort.SliceStable(selected, func(i, j int) bool { return selected[i].CreatedAt.After(selected[j].CreatedAt) })
	return selected, reached, nil
}

// recordID names the job a record came from: its run, and the artifact the
// job published.
func recordID(summary artifactSummary) string {
	return strconv.FormatInt(summary.WorkflowRun.ID, 10) + "/" + strconv.FormatInt(summary.ID, 10)
}

func (s *RecordSource) listPage(ctx context.Context, page int) ([]artifactSummary, error) {
	endpoint := *s.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/repos/" + s.repository + "/actions/artifacts"
	query := endpoint.Query()
	query.Set("name", s.artifact)
	query.Set("per_page", strconv.Itoa(artifactsPerPage))
	query.Set("page", strconv.Itoa(page))
	endpoint.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	s.authorize(request)
	response, err := s.client.Do(request)
	if err != nil {
		return nil, errors.New("listing usage artifacts failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing usage artifacts returned HTTP %d", response.StatusCode)
	}
	var listed artifactListResponse
	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil {
		return nil, fmt.Errorf("decode usage artifact list: %w", err)
	}
	return listed.Artifacts, nil
}

func (s *RecordSource) download(ctx context.Context, summary artifactSummary) (server.UsageReport, error) {
	endpoint := *s.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") +
		"/repos/" + s.repository + "/actions/artifacts/" + strconv.FormatInt(summary.ID, 10) + "/zip"

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return server.UsageReport{}, err
	}
	s.authorize(request)
	response, err := s.client.Do(request)
	if err != nil {
		return server.UsageReport{}, errors.New("downloading the usage artifact failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return server.UsageReport{}, fmt.Errorf("downloading the usage artifact returned HTTP %d", response.StatusCode)
	}
	archive, err := io.ReadAll(io.LimitReader(response.Body, maxRecordBytes+1))
	if err != nil {
		return server.UsageReport{}, err
	}
	if int64(len(archive)) > maxRecordBytes {
		return server.UsageReport{}, errors.New("the usage artifact is larger than the record limit")
	}
	return readUsageArchive(archive)
}

func readUsageArchive(archive []byte) (server.UsageReport, error) {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return server.UsageReport{}, fmt.Errorf("open usage artifact: %w", err)
	}
	for _, file := range reader.File {
		if file.Name != usageFileName {
			continue
		}
		// Bounded independently of the compressed size, so a small archive
		// cannot expand into an unbounded allocation.
		entry, err := file.Open()
		if err != nil {
			return server.UsageReport{}, err
		}
		data, err := io.ReadAll(io.LimitReader(entry, maxRecordBytes+1))
		closeErr := entry.Close()
		if err != nil {
			return server.UsageReport{}, err
		}
		if closeErr != nil {
			return server.UsageReport{}, closeErr
		}
		if int64(len(data)) > maxRecordBytes {
			return server.UsageReport{}, errors.New("the usage record is larger than the record limit")
		}
		var report server.UsageReport
		if err := json.Unmarshal(data, &report); err != nil {
			return server.UsageReport{}, fmt.Errorf("decode usage record: %w", err)
		}
		return report, nil
	}
	return server.UsageReport{}, fmt.Errorf("the artifact holds no %s", usageFileName)
}

func (s *RecordSource) authorize(request *http.Request) {
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+s.token)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}
