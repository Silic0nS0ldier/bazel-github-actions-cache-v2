package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func mustRoutes(t *testing.T, patterns ...string) []assetRoute {
	t.Helper()
	routes, err := ParseAssetRoutes(patterns)
	if err != nil {
		t.Fatal(err)
	}
	return routes
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestAssetRoutesMatchOnlyWhatTheyName(t *testing.T) {
	routes := mustRoutes(t,
		"https://ghcr.io/v2/acme/*",
		"https://registry.example.com/",
		"https://*.jfrog.example/artifactory/",
	)
	allowed := []string{
		"https://ghcr.io/v2/acme/app/manifests/sha256:abc",
		"https://ghcr.io/v2/acme/",
		"https://registry.example.com/",
		"https://registry.example.com/v2/anything",
		"https://team.jfrog.example/artifactory/repo/file.tgz",
		"https://a.b.jfrog.example/artifactory/",
	}
	for _, uri := range allowed {
		if !routeAllows(routes, mustURL(t, uri)) {
			t.Errorf("%s should be allowed", uri)
		}
	}

	refused := []string{
		// A different repository on the same registry.
		"https://ghcr.io/v2/other/app/manifests/sha256:abc",
		// Prefix without a segment boundary.
		"https://ghcr.io/v2/acme-private/app",
		// Lookalike hosts.
		"https://ghcr.io.evil.example/v2/acme/app",
		"https://evil-ghcr.io/v2/acme/app",
		"https://notregistry.example.com/",
		// The wildcard covers subdomains, not the apex or a suffix match.
		"https://jfrog.example/artifactory/",
		"https://eviljfrog.example/artifactory/",
		// A port change is a different origin.
		"https://registry.example.com:8443/",
		// Right host, wrong path.
		"https://team.jfrog.example/private/file.tgz",
	}
	for _, uri := range refused {
		if routeAllows(routes, mustURL(t, uri)) {
			t.Errorf("%s should be refused", uri)
		}
	}
}

// A comma is a legal URL character, so entries are separated by newlines and a
// pattern containing one has to survive.
func TestAssetRoutesAreNewlineSeparated(t *testing.T) {
	routes, err := ParseAssetRoutes([]string{
		"\n  https://ghcr.io/v2/acme/*  \r\n\nhttps://registry.example.com/a,b/\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 {
		t.Fatalf("parsed %d routes, want 2", len(routes))
	}
	if !routeAllows(routes, mustURL(t, "https://ghcr.io/v2/acme/app")) {
		t.Error("the first route was lost to whitespace")
	}
	if !routeAllows(routes, mustURL(t, "https://registry.example.com/a,b/thing")) {
		t.Error("a comma inside a pattern was treated as a separator")
	}
}

func TestAssetRoutesRejectUnusablePatterns(t *testing.T) {
	for _, pattern := range []string{
		"http://ghcr.io/",            // not https
		"ghcr.io/v2/",                // no scheme
		"https://user:pass@ghcr.io/", // userinfo
		"https://*./",                // wildcards a suffix, not a domain
		"https://*.io/",              // wildcards a public suffix
		"https:///v2/",               // no host
	} {
		if _, err := ParseAssetRoutes([]string{pattern}); err == nil {
			t.Errorf("%q was accepted", pattern)
		}
	}
}

// Nothing is allowed until an operator says so, so a private asset is left to
// Bazel rather than landing in a cache the whole repository reads.
func TestCredentialsAreRefusedUntilARouteAllowsThem(t *testing.T) {
	credentialed := http.Header{}
	credentialed.Set("Authorization", "Bearer secret")
	target := mustURL(t, "https://ghcr.io/v2/acme/app/blobs/sha256:abc")

	if _, err := permittedHeaders(credentialed, target, nil); err == nil {
		t.Fatal("credentials were sent with no route configured")
	}
	if _, err := permittedHeaders(credentialed, target, mustRoutes(t, "https://ghcr.io/v2/other/*")); err == nil {
		t.Fatal("credentials were sent to a route that was not allowed")
	}
	got, err := permittedHeaders(credentialed, target, mustRoutes(t, "https://ghcr.io/v2/acme/*"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "Bearer secret" {
		t.Fatalf("an allowed route lost its credential: %q", got.Get("Authorization"))
	}
}

// A header that carries no credential says nothing about whether the content is
// private, so it needs no opt-in.
func TestHeadersWithoutCredentialsNeedNoRoute(t *testing.T) {
	plain := http.Header{}
	plain.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	got, err := permittedHeaders(plain, mustURL(t, "https://ghcr.io/v2/acme/app"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("Accept") == "" {
		t.Fatal("a harmless header was dropped")
	}
}

// The refusal is what a person acts on, so it has to say what to change.
func TestRefusalNamesTheSettingAndNotTheCredential(t *testing.T) {
	credentialed := http.Header{}
	credentialed.Set("Authorization", "Bearer super-secret-token")
	_, err := permittedHeaders(credentialed, mustURL(t, "https://ghcr.io/v2/acme/app?token=leaky"), nil)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "asset-header-routes") {
		t.Fatalf("the refusal does not say what to change: %v", err)
	}
	for _, secret := range []string{"super-secret-token", "leaky"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("the refusal leaked %q: %v", secret, err)
		}
	}
}
