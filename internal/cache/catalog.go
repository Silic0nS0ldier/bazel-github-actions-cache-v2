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
	"strconv"
	"strings"
	"time"
)

// Catalog lists immutable GitHub Actions cache keys. The packed backend uses
// it to discover manifest heads and to learn which packs still exist; cache
// bytes are still restored through the runner cache-v2 service. Listing reads
// metadata only, so it does not extend an entry's lifetime.
type Catalog interface {
	// List returns cache keys starting with keyPrefix, newest first. A positive
	// limit stops the listing early and reports truncation; UnboundedListing
	// returns every matching key.
	List(ctx context.Context, keyPrefix string, limit int) (keys []string, truncated bool, err error)
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
	ActionsCaches []struct {
		Key string `json:"key"`
	} `json:"actions_caches"`
}

// List returns cache keys whose immutable key starts with keyPrefix, newest
// first, and reports whether a positive limit truncated the result. GitHub's
// API pagination is followed explicitly so a later writer does not silently
// hide older DAG parents.
func (c *ActionsCatalog) List(ctx context.Context, keyPrefix string, limit int) ([]string, bool, error) {
	page := 1
	keys := make([]string, 0)
	unexpected := 0
	for {
		endpoint := *c.baseURL
		endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/repos/" + c.repository + "/actions/caches"
		query := endpoint.Query()
		query.Set("key", keyPrefix)
		// Newest first, so a truncated listing keeps the most recent entries
		// instead of an arbitrary subset.
		query.Set("sort", "created_at")
		query.Set("direction", "desc")
		query.Set("per_page", "100")
		query.Set("page", strconv.Itoa(page))
		endpoint.RawQuery = query.Encode()

		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return nil, false, fmt.Errorf("create cache catalog request: %w", err)
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("Authorization", "Bearer "+c.token)
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		response, err := c.client.Do(request)
		if err != nil {
			return nil, false, fmt.Errorf("list GitHub Actions caches: %w", err)
		}
		var result actionsCacheListResponse
		decodeErr := json.NewDecoder(response.Body).Decode(&result)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, false, fmt.Errorf("list GitHub Actions caches: HTTP %d", response.StatusCode)
		}
		if decodeErr != nil {
			return nil, false, fmt.Errorf("decode GitHub Actions cache list: %w", decodeErr)
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
			if limit > UnboundedListing && len(keys) == limit {
				c.reportUnexpected(keyPrefix, unexpected)
				return keys, true, nil
			}
			keys = append(keys, entry.Key)
		}
		if len(result.ActionsCaches) < 100 {
			c.reportUnexpected(keyPrefix, unexpected)
			return keys, false, nil
		}
		page++
	}
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
