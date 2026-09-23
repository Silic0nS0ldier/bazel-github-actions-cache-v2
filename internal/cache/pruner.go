package cache

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ActionsPruner deletes GitHub Actions cache entries. It is deliberately a
// separate type from ActionsCatalog: deleting needs actions: write, and the
// cache server must never be handed a client that can.
type ActionsPruner struct {
	baseURL    *url.URL
	repository string
	token      string
	ref        string
	client     *http.Client
}

// NewActionsPruner deletes only within one Git reference. Without a ref the API
// removes every reference's copy of a key, which would take entries this caller
// never listed and cannot replace.
func NewActionsPruner(timeout time.Duration, ref string) (*ActionsPruner, error) {
	repository := os.Getenv("GITHUB_REPOSITORY")
	if len(strings.Split(repository, "/")) != 2 || strings.HasPrefix(repository, "/") || strings.HasSuffix(repository, "/") {
		return nil, errors.New("GITHUB_REPOSITORY must be owner/repository")
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, errors.New("GITHUB_TOKEN is required to delete cache entries")
	}
	baseURLText := os.Getenv("GITHUB_API_URL")
	if baseURLText == "" {
		baseURLText = "https://api.github.com"
	}
	baseURL, err := url.Parse(baseURLText)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, fmt.Errorf("invalid GITHUB_API_URL: %w", err)
	}
	if ref == "" || !validRef(ref) {
		return nil, fmt.Errorf("deleting needs a full Git reference such as refs/heads/main, got %q", ref)
	}
	return &ActionsPruner{
		baseURL:    baseURL,
		repository: repository,
		token:      token,
		ref:        ref,
		client:     &http.Client{Timeout: timeout},
	}, nil
}

// Delete removes the cache entry with exactly this key. An entry that is
// already gone is not an error: the goal is its absence.
func (p *ActionsPruner) Delete(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("refusing to delete without a key")
	}
	endpoint := *p.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/repos/" + p.repository + "/actions/caches"
	// Both are exact matches. Without the ref the API would delete every
	// reference's copy of this key.
	query := url.Values{}
	query.Set("key", key)
	query.Set("ref", p.ref)
	endpoint.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+p.token)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := p.client.Do(request)
	if err != nil {
		return errors.New("deleting the cache entry failed")
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return fmt.Errorf("deleting the cache entry returned HTTP %d", response.StatusCode)
	}
}
