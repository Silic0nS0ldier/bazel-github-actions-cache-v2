package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	remoteasset "github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/build/bazel/remote/asset/v1"
)

func qualifier(name, value string) *remoteasset.Qualifier {
	return remoteasset.Qualifier_builder{Name: name, Value: value}.Build()
}

// A URI-specific header must win over the generic one, and a generic header
// must reach every URI.
func TestAssetHeadersPreferTheUriSpecificValue(t *testing.T) {
	headers, err := parseAssetHeaders([]*remoteasset.Qualifier{
		qualifier("http_header:Accept", "*/*"),
		qualifier("http_header:Authorization", "Bearer generic"),
		qualifier("http_header_url:1:Authorization", "Bearer second"),
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := headers.forURI(0).Get("Authorization"); got != "Bearer generic" {
		t.Fatalf("uri 0 authorization = %q", got)
	}
	if got := headers.forURI(1).Get("Authorization"); got != "Bearer second" {
		t.Fatalf("uri 1 authorization = %q", got)
	}
	for _, index := range []int{0, 1} {
		if got := headers.forURI(index).Get("Accept"); got != "*/*" {
			t.Fatalf("uri %d lost the generic header: %q", index, got)
		}
	}
}

// Merging must not let one URI's headers leak into another's.
func TestAssetHeadersDoNotLeakBetweenUris(t *testing.T) {
	headers, err := parseAssetHeaders([]*remoteasset.Qualifier{
		qualifier("http_header_url:0:Authorization", "Bearer first"),
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := headers.forURI(1).Get("Authorization"); got != "" {
		t.Fatalf("uri 1 received uri 0's credential: %q", got)
	}
}

func TestAssetHeadersRejectUnusableQualifiers(t *testing.T) {
	cases := map[string][]*remoteasset.Qualifier{
		"unknown uri":      {qualifier("http_header_url:7:Accept", "*/*")},
		"negative uri":     {qualifier("http_header_url:-1:Accept", "*/*")},
		"no header name":   {qualifier("http_header_url:0", "*/*")},
		"empty name":       {qualifier("http_header:", "*/*")},
		"not a token":      {qualifier("http_header:Bad Header", "*/*")},
		"header injection": {qualifier("http_header:Accept", "*/*\r\nX-Evil: 1")},
		"routing header":   {qualifier("http_header:Host", "evil.example")},
		"framing header":   {qualifier("http_header:Content-Length", "0")},
		"oversized value":  {qualifier("http_header:Accept", strings.Repeat("a", maxAssetHeaderLen+1))},
	}
	for name, qualifiers := range cases {
		if _, err := parseAssetHeaders(qualifiers, 1); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestAssetHeadersAreBounded(t *testing.T) {
	qualifiers := make([]*remoteasset.Qualifier, 0, maxAssetHeaders+1)
	for index := 0; index <= maxAssetHeaders; index++ {
		qualifiers = append(qualifiers, qualifier("http_header:X-Many-"+string(rune('a'+index%26)), "1"))
	}
	if _, err := parseAssetHeaders(qualifiers, 1); err == nil {
		t.Fatal("an unbounded number of headers was accepted")
	}
}

// A credential issued for one origin must not follow a redirect to another,
// which is how a hostile origin would harvest it.
func TestSensitiveHeadersDoNotSurviveACrossOriginRedirect(t *testing.T) {
	origin := func(raw string) *url.URL {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	cases := []struct {
		from, to string
		kept     bool
	}{
		{"https://ghcr.io/a", "https://ghcr.io/b", true},
		{"https://ghcr.io/a", "https://evil.example/b", false},
		{"https://ghcr.io/a", "https://ghcr.io.evil.example/b", false},
		{"https://ghcr.io/a", "https://ghcr.io:8443/b", false},
	}
	for _, testCase := range cases {
		request := &http.Request{URL: origin(testCase.to), Header: http.Header{}}
		request.Header.Set("Authorization", "Bearer secret")
		request.Header.Set("Cookie", "session=secret")
		request.Header.Set("Accept", "*/*")
		stripSensitiveOnRedirect(request, []*http.Request{{URL: origin(testCase.from)}})

		if kept := request.Header.Get("Authorization") != ""; kept != testCase.kept {
			t.Errorf("%s -> %s: authorization kept = %v, want %v", testCase.from, testCase.to, kept, testCase.kept)
		}
		if kept := request.Header.Get("Cookie") != ""; kept != testCase.kept {
			t.Errorf("%s -> %s: cookie kept = %v, want %v", testCase.from, testCase.to, kept, testCase.kept)
		}
		// A header that carries no credential is never the problem.
		if request.Header.Get("Accept") != "*/*" {
			t.Errorf("%s -> %s: dropped a harmless header", testCase.from, testCase.to)
		}
	}
}

// The headers have to reach the origin, or the whole feature is inert.
func TestFetchSendsTheDeclaredHeadersToTheOrigin(t *testing.T) {
	var seen http.Header
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte("asset body"))
	}))
	defer origin.Close()

	backend := newMemoryBackend()
	server := testServer(t, backend, func(cfg *Config) {
		cfg.AssetClient = origin.Client()
	})
	blob := []byte("asset body")
	reference := referenceFor(blob)
	headers := http.Header{}
	headers.Set("Authorization", "Bearer secret")

	path, _, err := server.downloadAsset(
		t.Context(),
		origin.URL,
		assetChecksum{algorithm: sha256Algorithm, hash: reference.hash},
		headers,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	if got := seen.Get("Authorization"); got != "Bearer secret" {
		t.Fatalf("origin saw authorization %q", got)
	}
}
