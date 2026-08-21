package edge

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"

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
			log.Printf("site %q: proxy error for %s: %v", siteName, r.URL, err)
			http.Error(w, "proxy error: "+err.Error(), http.StatusBadGateway)
		},
	}
}
