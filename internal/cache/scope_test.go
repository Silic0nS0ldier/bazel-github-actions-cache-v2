package cache

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func testBaseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// A pass that deletes wants to see only what it can also restore, so entries
// another reference owns must not appear at all.
func TestScopedCatalogAsksForOneReference(t *testing.T) {
	var scope string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scope = r.URL.Query().Get("ref")
		_, _ = w.Write([]byte(`{"actions_caches":[{"key":"prefix-one"}]}`))
	}))
	defer server.Close()

	catalog := &ActionsCatalog{
		baseURL:    testBaseURL(t, server.URL),
		repository: "owner/repository",
		token:      "token",
		ref:        "refs/heads/main",
		client:     server.Client(),
		logger:     log.New(discard{}, "", 0),
	}
	if _, err := catalog.List(context.Background(), "prefix-"); err != nil {
		t.Fatal(err)
	}
	if scope != "refs/heads/main" {
		t.Fatalf("ref = %q", scope)
	}
}

// A cache server must keep listing every reference: a job restores its own
// branch's entries and the default branch's.
func TestUnscopedCatalogAsksForEveryReference(t *testing.T) {
	var present bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.URL.Query()["ref"]
		_, _ = w.Write([]byte(`{"actions_caches":[]}`))
	}))
	defer server.Close()

	catalog := &ActionsCatalog{
		baseURL:    testBaseURL(t, server.URL),
		repository: "owner/repository",
		token:      "token",
		client:     server.Client(),
		logger:     log.New(discard{}, "", 0),
	}
	if _, err := catalog.List(context.Background(), "prefix-"); err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("an unscoped catalog narrowed the listing to one reference")
	}
}

// Without a ref the API deletes every reference's copy of a key, which would
// take entries this caller never listed and cannot replace.
func TestPrunerDeletesWithinOneReference(t *testing.T) {
	var query url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s", r.Method)
		}
		query = r.URL.Query()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	pruner := &ActionsPruner{
		baseURL:    testBaseURL(t, server.URL),
		repository: "owner/repository",
		token:      "token",
		ref:        "refs/heads/main",
		client:     server.Client(),
	}
	if err := pruner.Delete(context.Background(), "prefix-car-pack-v1-abc"); err != nil {
		t.Fatal(err)
	}
	if got := query.Get("ref"); got != "refs/heads/main" {
		t.Fatalf("ref = %q", got)
	}
	if got := query.Get("key"); got != "prefix-car-pack-v1-abc" {
		t.Fatalf("key = %q", got)
	}
}

func TestScopeMustBeAFullReference(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "owner/repository")
	t.Setenv("GITHUB_TOKEN", "token")
	for _, ref := range []string{"main", "../../etc", "refs/heads/../..", "refs/heads/", "refs//heads/x", "refs/heads/x.lock", ""} {
		if _, err := NewActionsPruner(time.Second, ref); err == nil {
			t.Fatalf("pruner accepted %q", ref)
		}
		if ref == "" {
			// An unscoped catalog is legitimate; an unscoped pruner is not.
			continue
		}
		if _, err := NewScopedActionsCatalog(time.Second, nil, ref); err == nil {
			t.Fatalf("catalog accepted %q", ref)
		}
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
