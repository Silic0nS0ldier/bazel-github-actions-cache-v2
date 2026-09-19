package cache

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestActionsCatalogListsAllPagesWithinPrefix(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.URL.Query().Get("key"); got != "prefix-manifest-" {
			t.Fatalf("key = %q", got)
		}
		if got := r.URL.Query().Get("sort"); got != "created_at" {
			t.Fatalf("sort = %q", got)
		}
		if got := r.URL.Query().Get("direction"); got != "desc" {
			t.Fatalf("direction = %q", got)
		}
		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte(`{"actions_caches":[{"key":"prefix-manifest-one"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"actions_caches":[]}`))
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &ActionsCatalog{baseURL: baseURL, repository: "owner/repository", token: "token", client: server.Client()}
	keys, truncated, err := catalog.List(context.Background(), "prefix-manifest-", 10)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "prefix-manifest-one" || truncated || requests != 1 {
		t.Fatalf("keys/truncated/requests = %v/%t/%d", keys, truncated, requests)
	}
}

func TestActionsCatalogTruncatesAtLimitAndListsUnbounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"actions_caches":[{"key":"prefix-one"},{"key":"prefix-two"}]}`))
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &ActionsCatalog{baseURL: baseURL, repository: "owner/repository", token: "token", client: server.Client()}
	keys, truncated, err := catalog.List(context.Background(), "prefix-", 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "prefix-one" || !truncated {
		t.Fatalf("bounded keys/truncated = %v/%t", keys, truncated)
	}
	keys, truncated, err = catalog.List(context.Background(), "prefix-", UnboundedListing)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "prefix-one,prefix-two" || truncated {
		t.Fatalf("unbounded keys/truncated = %v/%t", keys, truncated)
	}
}

func TestActionsCatalogLogsKeysOutsideTheRequestedPrefix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"actions_caches":[{"key":"prefix-one"},{"key":"unrelated"},{"key":"another"}]}`))
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	catalog := &ActionsCatalog{
		baseURL:    baseURL,
		repository: "owner/repository",
		token:      "token",
		client:     server.Client(),
		logger:     log.New(&logged, "", 0),
	}
	keys, _, err := catalog.List(context.Background(), "prefix-", UnboundedListing)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "prefix-one" {
		t.Fatalf("keys = %v", keys)
	}
	if !strings.Contains(logged.String(), `unrelated key "unrelated"`) ||
		!strings.Contains(logged.String(), "returned 2 unrelated keys") {
		t.Fatalf("prefix filter violation was not reported: %q", logged.String())
	}
}
