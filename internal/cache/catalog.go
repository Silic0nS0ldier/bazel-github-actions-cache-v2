package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Catalog lists immutable GitHub Actions cache keys. The packed backend uses
// it to discover manifest heads and to learn which packs still exist; cache
// bytes are still restored through the runner cache-v2 service. Listing reads
// metadata only, so it does not extend an entry's lifetime.
type Catalog interface {
	// List returns every cache key starting with keyPrefix, newest first.
	List(ctx context.Context, keyPrefix string) ([]string, error)
}

// ActionsCatalog lists cache metadata through the public GitHub REST API.
// A normal GITHUB_TOKEN with actions: read is sufficient. It deliberately
// does not expose deletion or mutation operations.
type ActionsCatalog struct {
	baseURL    *url.URL
	repository string
	token      string
	// ref restricts listings to one Git reference. Empty lists every reference,
	// which is what a cache server needs: a job restores its own branch's
	// entries and the default branch's.
	ref    string
	client *http.Client
	logger *log.Logger
}

// refPattern admits the full references GitHub documents for this parameter.
var refPattern = regexp.MustCompile(`^refs/[A-Za-z0-9._/-]{1,200}$`)

// validRef applies the parts of git's own refname rules that keep a reference
// from naming something other than itself.
func validRef(ref string) bool {
	return refPattern.MatchString(ref) &&
		!strings.Contains(ref, "..") &&
		!strings.Contains(ref, "//") &&
		!strings.HasSuffix(ref, "/") &&
		!strings.HasSuffix(ref, ".lock")
}

// NewActionsCatalog creates a manifest-discovery client from GitHub Actions
// runner environment variables.
func NewActionsCatalog(timeout time.Duration, logger *log.Logger) (*ActionsCatalog, error) {
	return NewScopedActionsCatalog(timeout, logger, "")
}

// NewScopedActionsCatalog restricts every listing to one Git reference, so that
// entries another reference owns never appear. A caller that deletes wants this:
// it can restore exactly what it lists.
func NewScopedActionsCatalog(timeout time.Duration, logger *log.Logger, ref string) (*ActionsCatalog, error) {
	repository := os.Getenv("GITHUB_REPOSITORY")
	if len(strings.Split(repository, "/")) != 2 || strings.HasPrefix(repository, "/") || strings.HasSuffix(repository, "/") {
		return nil, errors.New("GITHUB_REPOSITORY must be owner/repository for packed-cache manifest discovery")
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, errors.New("GITHUB_TOKEN is required for packed-cache manifest discovery")
	}
	baseURLText := os.Getenv("GITHUB_API_URL")
	if baseURLText == "" {
		baseURLText = "https://api.github.com"
	}
	baseURL, err := url.Parse(baseURLText)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, fmt.Errorf("invalid GITHUB_API_URL: %w", err)
	}
	if ref != "" && !validRef(ref) {
		return nil, fmt.Errorf("a cache scope must be a full Git reference such as refs/heads/main, got %q", ref)
	}
	return &ActionsCatalog{
		baseURL:    baseURL,
		repository: repository,
		token:      token,
		ref:        ref,
		client:     &http.Client{Timeout: timeout},
		logger:     logger,
	}, nil
}

type actionsCacheListResponse struct {
	ActionsCaches []struct {
		Key       string    `json:"key"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"actions_caches"`
}

// CatalogEntry is a listed cache entry with the metadata a deletion decision
// needs. Age is the only thing distinguishing a pack whose manifest was lost
// from one a running job has not committed a manifest for yet.
type CatalogEntry struct {
	Key       string
	CreatedAt time.Time
}

// List returns every cache key whose immutable key starts with keyPrefix,
// newest first.
func (c *ActionsCatalog) List(ctx context.Context, keyPrefix string) ([]string, error) {
	entries, err := c.ListEntries(ctx, keyPrefix)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		keys = append(keys, entry.Key)
	}
	return keys, nil
}

// List returns every cache key whose immutable key starts with keyPrefix,
// newest first. Pagination follows the Link header rather than page sizes, so
// a short page cannot end the listing early and silently hide older DAG
// parents.
func (c *ActionsCatalog) ListEntries(ctx context.Context, keyPrefix string) ([]CatalogEntry, error) {
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/repos/" + c.repository + "/actions/caches"
	query := endpoint.Query()
	query.Set("key", keyPrefix)
	// Newest first, so a caller that reads only part of the listing keeps the
	// most recent entries rather than an arbitrary subset.
	query.Set("sort", "created_at")
	query.Set("direction", "desc")
	query.Set("per_page", "100")
	if c.ref != "" {
		query.Set("ref", c.ref)
	}
	endpoint.RawQuery = query.Encode()

	next := endpoint.String()
	keys := make([]CatalogEntry, 0)
	unexpected := 0
	for next != "" {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, fmt.Errorf("create cache catalog request: %w", err)
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("Authorization", "Bearer "+c.token)
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		response, err := c.client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("list GitHub Actions caches: %w", err)
		}
		var result actionsCacheListResponse
		decodeErr := json.NewDecoder(response.Body).Decode(&result)
		link := response.Header.Get("Link")
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("list GitHub Actions caches: HTTP %d", response.StatusCode)
		}
		if decodeErr != nil {
			return nil, fmt.Errorf("decode GitHub Actions cache list: %w", decodeErr)
		}
		for _, entry := range result.ActionsCaches {
			if !strings.HasPrefix(entry.Key, keyPrefix) {
				// GitHub documents key as an explicit key or prefix, so a
				// non-matching entry means the filter no longer behaves as this
				// catalog assumes.
				if unexpected == 0 {
					c.logf("GitHub cache listing for prefix %q returned unrelated key %q", keyPrefix, entry.Key)
				}
				unexpected++
				continue
			}
			keys = append(keys, CatalogEntry{Key: entry.Key, CreatedAt: entry.CreatedAt})
		}
		if next, err = c.nextPageURL(link); err != nil {
			return nil, err
		}
	}
	c.reportUnexpected(keyPrefix, unexpected)
	return keys, nil
}

// nextPageURL returns the rel="next" target of a Link header, or an empty
// string on the final page.
func (c *ActionsCatalog) nextPageURL(header string) (string, error) {
	for _, section := range strings.Split(header, ",") {
		parts := strings.Split(strings.TrimSpace(section), ";")
		if len(parts) < 2 {
			continue
		}
		target := strings.TrimSpace(parts[0])
		if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
			continue
		}
		isNext := false
		for _, parameter := range parts[1:] {
			switch strings.TrimSpace(parameter) {
			case `rel="next"`, "rel=next":
				isNext = true
			}
		}
		if !isNext {
			continue
		}
		parsed, err := url.Parse(target[1 : len(target)-1])
		if err != nil {
			return "", fmt.Errorf("parse cache listing pagination link: %w", err)
		}
		// Pagination must never leave the configured API host.
		if parsed.Scheme != c.baseURL.Scheme || parsed.Host != c.baseURL.Host {
			return "", fmt.Errorf("cache listing pagination link leaves %s", c.baseURL.Host)
		}
		return parsed.String(), nil
	}
	return "", nil
}

func (c *ActionsCatalog) reportUnexpected(keyPrefix string, unexpected int) {
	if unexpected > 1 {
		c.logf("GitHub cache listing for prefix %q returned %d unrelated keys", keyPrefix, unexpected)
	}
}

func (c *ActionsCatalog) logf(format string, args ...any) {
	if c.logger == nil {
		return
	}
	c.logger.Printf(format, args...)
}
