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
	"strings"
	"time"
)

// Catalog lists immutable GitHub Actions cache keys. The packed backend uses
// it to discover manifest heads and to learn which packs still exist; cache
// bytes are still restored through the runner cache-v2 service. Listing reads
// metadata only, so it does not extend an entry's lifetime.
type Catalog interface {
	// List returns cache keys starting with keyPrefix, newest first, and how
	// many matching entries a positive limit left unlisted. UnboundedListing
	// returns every matching key and always reports zero.
	List(ctx context.Context, keyPrefix string, limit int) (keys []string, skipped int, err error)
}

// UnboundedListing lists every matching key. Use it only where a missing key
// changes a decision, because a truncated listing would silently look like
// absence.
const UnboundedListing = 0

// ActionsCatalog lists cache metadata through the public GitHub REST API.
// A normal GITHUB_TOKEN with actions: read is sufficient. It deliberately
// does not expose deletion or mutation operations.
type ActionsCatalog struct {
	baseURL    *url.URL
	repository string
	token      string
	client     *http.Client
	logger     *log.Logger
}

// NewActionsCatalog creates a manifest-discovery client from GitHub Actions
// runner environment variables.
func NewActionsCatalog(timeout time.Duration, logger *log.Logger) (*ActionsCatalog, error) {
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
	return &ActionsCatalog{
		baseURL:    baseURL,
		repository: repository,
		token:      token,
		client:     &http.Client{Timeout: timeout},
		logger:     logger,
	}, nil
}

type actionsCacheListResponse struct {
	TotalCount    int `json:"total_count"`
	ActionsCaches []struct {
		Key string `json:"key"`
	} `json:"actions_caches"`
}

// List returns cache keys whose immutable key starts with keyPrefix, newest
// first, and how many matching entries a positive limit left unlisted.
// Pagination follows the Link header rather than page sizes, so a short page
// cannot end the listing early and silently hide older DAG parents.
func (c *ActionsCatalog) List(ctx context.Context, keyPrefix string, limit int) ([]string, int, error) {
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/repos/" + c.repository + "/actions/caches"
	query := endpoint.Query()
	query.Set("key", keyPrefix)
	// Newest first, so a truncated listing keeps the most recent entries
	// instead of an arbitrary subset.
	query.Set("sort", "created_at")
	query.Set("direction", "desc")
	query.Set("per_page", "100")
	endpoint.RawQuery = query.Encode()

	next := endpoint.String()
	keys := make([]string, 0)
	unexpected := 0
	seen := 0
	for next != "" {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, 0, fmt.Errorf("create cache catalog request: %w", err)
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("Authorization", "Bearer "+c.token)
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		response, err := c.client.Do(request)
		if err != nil {
			return nil, 0, fmt.Errorf("list GitHub Actions caches: %w", err)
		}
		var result actionsCacheListResponse
		decodeErr := json.NewDecoder(response.Body).Decode(&result)
		link := response.Header.Get("Link")
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, 0, fmt.Errorf("list GitHub Actions caches: HTTP %d", response.StatusCode)
		}
		if decodeErr != nil {
			return nil, 0, fmt.Errorf("decode GitHub Actions cache list: %w", decodeErr)
		}
		for _, entry := range result.ActionsCaches {
			matches := strings.HasPrefix(entry.Key, keyPrefix)
			if matches && limit > UnboundedListing && len(keys) == limit {
				c.reportUnexpected(keyPrefix, unexpected)
				return keys, unlistedFrom(result.TotalCount, seen), nil
			}
			seen++
			if !matches {
				// GitHub documents key as an explicit key or prefix, so a
				// non-matching entry means the filter no longer behaves as this
				// catalog assumes.
				if unexpected == 0 {
					c.logf("GitHub cache listing for prefix %q returned unrelated key %q", keyPrefix, entry.Key)
				}
				unexpected++
				continue
			}
			keys = append(keys, entry.Key)
		}
		if next, err = c.nextPageURL(link); err != nil {
			return nil, 0, err
		}
	}
	c.reportUnexpected(keyPrefix, unexpected)
	return keys, 0, nil
}

// unlistedFrom counts what a limit left behind. total_count covers the whole
// query rather than one page, so it is a trustworthy remainder only while it
// exceeds the entries already consumed; otherwise only the entry that tripped
// the limit is known to be unlisted.
func unlistedFrom(totalCount, seen int) int {
	if remaining := totalCount - seen; remaining > 0 {
		return remaining
	}
	return 1
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
