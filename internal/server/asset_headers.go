package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	remoteasset "github.com/cre4ture/bazel-github-actions-cache-v2/internal/proto/gen/build/bazel/remote/asset/v1"
)

// Bazel attaches request headers as qualifiers, generic ones under
// http_header: and per-URI ones under http_header_url:<index>:. The colon is
// used as the delimiter because a header name may not contain one.
const (
	headerQualifier    = "http_header:"
	headerURLQualifier = "http_header_url:"
)

// Headers that decide where a request goes, or how its body is framed, are
// never taken from a caller.
var refusedHeaders = map[string]struct{}{
	"host":              {},
	"content-length":    {},
	"transfer-encoding": {},
	"connection":        {},
	"upgrade":           {},
	"te":                {},
	"trailer":           {},
}

// assetHeaders holds the headers a fetch may send, keyed by the URI index they
// were scoped to. A URI-specific value wins over the generic one, which is what
// the qualifier definition requires.
type assetHeaders struct {
	generic  http.Header
	specific map[int]http.Header
}

const (
	maxAssetHeaders   = 32
	maxAssetHeaderLen = 8 * 1024
)

func parseAssetHeaders(qualifiers []*remoteasset.Qualifier, uris int) (assetHeaders, error) {
	headers := assetHeaders{generic: http.Header{}, specific: map[int]http.Header{}}
	count := 0
	for _, qualifier := range qualifiers {
		name := qualifier.GetName()
		index := -1
		switch {
		case strings.HasPrefix(name, headerURLQualifier):
			rest := strings.TrimPrefix(name, headerURLQualifier)
			position, header, found := strings.Cut(rest, ":")
			if !found {
				return assetHeaders{}, fmt.Errorf("%s qualifier has no header name", headerURLQualifier)
			}
			parsed, err := strconv.Atoi(position)
			if err != nil || parsed < 0 || parsed >= uris {
				return assetHeaders{}, fmt.Errorf("%s names uri %q, which was not supplied", headerURLQualifier, position)
			}
			index, name = parsed, header
		case strings.HasPrefix(name, headerQualifier):
			name = strings.TrimPrefix(name, headerQualifier)
		default:
			continue
		}

		if err := validateHeader(name, qualifier.GetValue()); err != nil {
			return assetHeaders{}, err
		}
		count++
		if count > maxAssetHeaders {
			return assetHeaders{}, fmt.Errorf("more than %d headers were supplied", maxAssetHeaders)
		}
		if index < 0 {
			headers.generic.Add(name, qualifier.GetValue())
			continue
		}
		if headers.specific[index] == nil {
			headers.specific[index] = http.Header{}
		}
		headers.specific[index].Add(name, qualifier.GetValue())
	}
	return headers, nil
}

func validateHeader(name, value string) error {
	if name == "" {
		return fmt.Errorf("a header qualifier has no name")
	}
	if !httpTokenValid(name) {
		return fmt.Errorf("header name %q is not a token", name)
	}
	if _, refused := refusedHeaders[strings.ToLower(name)]; refused {
		return fmt.Errorf("refusing to send a %s header", name)
	}
	if len(value) > maxAssetHeaderLen {
		return fmt.Errorf("the %s header is longer than %d bytes", name, maxAssetHeaderLen)
	}
	// A newline would let a value inject a header of its own.
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("the %s header contains a control character", name)
	}
	return nil
}

func httpTokenValid(name string) bool {
	for _, character := range name {
		if character >= 0x80 || strings.ContainsRune(" \t\"(),/:;<=>?@[\\]{}\x7f", character) || character < 0x21 {
			return false
		}
	}
	return true
}

// forURI returns the headers to send to one of the request's URIs.
func (h assetHeaders) forURI(index int) http.Header {
	merged := http.Header{}
	for name, values := range h.generic {
		merged[name] = append([]string(nil), values...)
	}
	for name, values := range h.specific[index] {
		merged[name] = append([]string(nil), values...)
	}
	return merged
}

// sensitiveHeaders must not survive a redirect to another origin, the same rule
// browsers and curl apply. Go drops these itself on a cross-domain redirect,
// but only for the ones it knows about, and only across domains rather than
// origins.
var sensitiveHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie", "Www-Authenticate"}

func carriesCredential(headers http.Header) bool {
	for _, name := range sensitiveHeaders {
		if headers.Get(name) != "" {
			return true
		}
	}
	return false
}

// assetRoute is a URI pattern whose credentials this server is permitted to
// forward. Anything an operator has not named is refused rather than fetched
// without its credential, so a private asset never reaches a shared cache.
type assetRoute struct {
	host       string
	wildcard   bool
	pathPrefix string
}

// ParseAssetRoutes accepts patterns such as https://ghcr.io/v2/acme/* and
// https://*.example.com/. A trailing * makes the path a prefix; without one the
// match still has to fall on a path segment boundary.
//
// Entries are separated by newlines rather than commas, because a comma is a
// legal URL character and a list of these is read by people.
func ParseAssetRoutes(patterns []string) ([]assetRoute, error) {
	routes := make([]assetRoute, 0, len(patterns))
	for _, entry := range patterns {
		for _, pattern := range strings.Split(entry, "\n") {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" {
				continue
			}
			route, err := parseAssetRoute(pattern)
			if err != nil {
				return nil, err
			}
			routes = append(routes, route)
		}
	}
	return routes, nil
}

func parseAssetRoute(pattern string) (assetRoute, error) {
	parsed, err := url.Parse(pattern)
	if err != nil {
		return assetRoute{}, fmt.Errorf("asset route %q is not a URL", pattern)
	}
	if parsed.Scheme != "https" {
		return assetRoute{}, fmt.Errorf("asset route %q must be https", pattern)
	}
	if parsed.User != nil {
		return assetRoute{}, fmt.Errorf("asset route %q must not carry userinfo", pattern)
	}
	host := strings.ToLower(parsed.Host)
	route := assetRoute{host: host, pathPrefix: parsed.Path}
	if rest, found := strings.CutPrefix(host, "*."); found {
		if rest == "" || !strings.Contains(rest, ".") {
			return assetRoute{}, fmt.Errorf("asset route %q must wildcard a domain, not a suffix", pattern)
		}
		route.host, route.wildcard = rest, true
	}
	if route.host == "" {
		return assetRoute{}, fmt.Errorf("asset route %q has no host", pattern)
	}
	if route.pathPrefix == "" {
		route.pathPrefix = "/"
	}
	return route, nil
}

func (r assetRoute) matches(target *url.URL) bool {
	host := strings.ToLower(target.Host)
	if r.wildcard {
		// A label boundary is required, so evil-ghcr.io does not match *.ghcr.io.
		if !strings.HasSuffix(host, "."+r.host) {
			return false
		}
	} else if host != r.host {
		return false
	}
	path := target.EscapedPath()
	if prefix, found := strings.CutSuffix(r.pathPrefix, "*"); found {
		return strings.HasPrefix(path, prefix)
	}
	if path == r.pathPrefix {
		return true
	}
	// Without a wildcard the prefix only covers whole segments, so /v2/acme
	// does not admit /v2/acme-private.
	return strings.HasPrefix(path, strings.TrimSuffix(r.pathPrefix, "/")+"/")
}

func routeAllows(routes []assetRoute, target *url.URL) bool {
	for _, route := range routes {
		if route.matches(target) {
			return true
		}
	}
	return false
}

// permittedHeaders drops a credential the operator has not opted into sending.
// It reports an error rather than fetching without it, so that a private asset
// is left to Bazel's own downloader instead of landing in a shared cache.
func permittedHeaders(headers http.Header, target *url.URL, routes []assetRoute) (http.Header, error) {
	if !carriesCredential(headers) {
		return headers, nil
	}
	if !routeAllows(routes, target) {
		return nil, fmt.Errorf(
			"refusing to send credentials to %s; add it to asset-header-routes to cache authenticated assets",
			safeURIString(target.String()),
		)
	}
	return headers, nil
}

// sameOrigin reports whether credentials may be carried from one URL to
// another. Scheme, host and port must all match.
func sameOrigin(from, to *url.URL) bool {
	return from.Scheme == to.Scheme && from.Host == to.Host
}

// stripSensitiveOnRedirect removes credentials once a redirect leaves the
// origin they were issued for.
func stripSensitiveOnRedirect(request *http.Request, via []*http.Request) {
	if len(via) == 0 {
		return
	}
	if sameOrigin(via[0].URL, request.URL) {
		return
	}
	for _, name := range sensitiveHeaders {
		request.Header.Del(name)
	}
}
