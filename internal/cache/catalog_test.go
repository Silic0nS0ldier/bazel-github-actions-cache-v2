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

func TestActionsCatalogFollowsShortPagesToTheFinalLink(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Errorf("authorization = %q", got)
		}
		query := r.URL.Query()
		if got := query.Get("key"); got != "prefix-manifest-" {
			t.Errorf("key = %q", got)
		}
		if got := query.Get("sort"); got != "created_at" {
			t.Errorf("sort = %q", got)
		}
		if got := query.Get("direction"); got != "desc" {
			t.Errorf("direction = %q", got)
		}
		// A page far shorter than per_page must not end the listing while the
		// server still advertises a next page.
		if query.Get("page") != "2" {
			query.Set("page", "2")
			next := "http://" + r.Host + "/repos/owner/repository/actions/caches?" + query.Encode()
			w.Header().Set("Link", `<`+next+`>; rel="next", <`+next+`>; rel="last"`)
			_, _ = w.Write([]byte(`{"total_count":2,"actions_caches":[{"key":"prefix-manifest-one"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"total_count":2,"actions_caches":[{"key":"prefix-manifest-two"}]}`))
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &ActionsCatalog{baseURL: baseURL, repository: "owner/repository", token: "token", client: server.Client()}
	keys, err := catalog.List(context.Background(), "prefix-manifest-")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "prefix-manifest-one,prefix-manifest-two" || requests != 2 {
		t.Fatalf("keys/requests = %v/%d", keys, requests)
	}
}

func TestActionsCatalogRejectsPaginationLinkToAnotherHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<https://attacker.example/repos/owner/repository/actions/caches?page=2>; rel="next"`)
		_, _ = w.Write([]byte(`{"total_count":1,"actions_caches":[{"key":"prefix-one"}]}`))
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	catalog := &ActionsCatalog{baseURL: baseURL, repository: "owner/repository", token: "token", client: server.Client()}
	if _, err := catalog.List(context.Background(), "prefix-"); err == nil {
		t.Fatal("pagination followed a link to another host")
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
	keys, err := catalog.List(context.Background(), "prefix-")
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
