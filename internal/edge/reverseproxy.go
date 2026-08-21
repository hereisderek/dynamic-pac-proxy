package edge

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/derekhud/dynamic-pac-proxy/internal/addon"
)

// newReverseProxy builds the httputil.ReverseProxy for one serve-enabled
// site: forward to backend, running addonPaths' request(flow) hooks in
// order before the request goes out, and their response(flow) hooks in
// reverse order (mitmproxy convention) on the way back. Fail-open at every
// step — a broken addon must never take down a public site that would
// otherwise work fine without it.
func newReverseProxy(siteName string, backend *url.URL, addons *addon.Runtime, addonsDir string, addonPaths []string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = backend.Scheme
			r.URL.Host = backend.Host
			r.Host = backend.Host
			// backend's own path/query (e.g. backend: https://example.com/api)
			// prefixes the incoming request's, the same joining behavior
			// httputil.NewSingleHostReverseProxy uses — serve.backend is
			// documented as a full URL, not just an origin.
			r.URL.Path, r.URL.RawPath = joinURLPath(backend, r.URL)
			if backend.RawQuery == "" || r.URL.RawQuery == "" {
				r.URL.RawQuery = backend.RawQuery + r.URL.RawQuery
			} else {
				r.URL.RawQuery = backend.RawQuery + "&" + r.URL.RawQuery
			}
			for _, p := range addonPaths {
				if err := addons.RunRequest(addonsDir, p, r); err != nil {
					log.Printf("site %q: addon %s: request(): %v", siteName, p, err)
				}
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			for i := len(addonPaths) - 1; i >= 0; i-- {
				if err := addons.RunResponse(addonsDir, addonPaths[i], resp); err != nil {
					log.Printf("site %q: addon %s: response(): %v", siteName, addonPaths[i], err)
				}
			}
			// Never return a non-nil error here: httputil.ReverseProxy
			// replaces the response with a generic error page if
			// ModifyResponse errors, which would defeat the entire
			// "one bad addon can't break the site" point of fail-open.
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// The detailed error (which can include the backend's
			// hostname, IP, port, or dial/DNS failure specifics) is only
			// logged, never sent to the client — this listener is
			// directly reachable from the public internet.
			log.Printf("site %q: proxy error for %s: %v", siteName, r.URL, err)
			http.Error(w, "proxy error", http.StatusBadGateway)
		},
	}
}

// singleJoiningSlash and joinURLPath mirror the unexported helpers
// net/http/httputil.NewSingleHostReverseProxy uses to join a fixed backend
// path with an incoming request's path — reimplemented here since the
// stdlib doesn't export them and this Director needs the same behavior for
// a backend URL with its own path/query (e.g. "https://example.com/api").
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

func joinURLPath(a, b *url.URL) (path, rawpath string) {
	if a.RawPath == "" && b.RawPath == "" {
		return singleJoiningSlash(a.Path, b.Path), ""
	}
	apath := a.EscapedPath()
	bpath := b.EscapedPath()

	aslash := strings.HasSuffix(apath, "/")
	bslash := strings.HasPrefix(bpath, "/")

	switch {
	case aslash && bslash:
		return a.Path + b.Path[1:], apath + bpath[1:]
	case !aslash && !bslash:
		return a.Path + "/" + b.Path, apath + "/" + bpath
	}
	return a.Path + b.Path, apath + bpath
}
