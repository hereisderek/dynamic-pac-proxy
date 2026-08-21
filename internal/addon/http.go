package addon

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.starlark.net/starlark"
)

// httpRequestTimeout bounds how long a single http_request() call may block
// the calling goroutine. Starlark's own step quota (see maxExecutionSteps)
// only counts interpreted steps — it does nothing to bound a blocking Go
// builtin call like this one, so the timeout has to live on the client
// itself. A fixed, generous-enough default is deliberately not exposed as a
// per-call option: this is a guard against a slow/unresponsive remote
// server hanging a request-handling goroutine, not a feature to tune.
const httpRequestTimeout = 10 * time.Second

// httpClient is shared across every script/call — a *http.Client is safe
// for concurrent use, and reusing one means connection pooling actually
// works across repeated calls to the same host instead of paying a fresh
// TCP+TLS handshake every time.
var httpClient = &http.Client{Timeout: httpRequestTimeout}

// addonPredeclared holds the extra builtins available to every addon
// script's top level, alongside Starlark's own. This is what lets a script
// do real work — e.g. fetching a session cookie from an auth endpoint
// before rewriting a request — despite the sandbox having no import
// statement and no ambient filesystem/network access otherwise.
var addonPredeclared = starlark.StringDict{
	"http_request": starlark.NewBuiltin("http_request", httpRequestBuiltin),
}

// httpRequestBuiltin implements http_request(url, method="GET", headers=None,
// body=None), returning a response object with the same status_code/headers/
// text/content shape as flow.response — but read-only: it isn't wired to any
// real *http.Response an addon call could apply back, so mutating it (unlike
// flow.request/flow.response) has no effect.
func httpRequestBuiltin(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var urlStr string
	method := "GET"
	var headersDict *starlark.Dict
	var bodyVal starlark.Value
	if err := starlark.UnpackArgs("http_request", args, kwargs,
		"url", &urlStr, "method?", &method, "headers?", &headersDict, "body?", &bodyVal,
	); err != nil {
		return nil, err
	}

	var bodyReader io.Reader
	switch body := bodyVal.(type) {
	case nil, starlark.NoneType:
		// no body
	case starlark.String:
		bodyReader = strings.NewReader(string(body))
	case starlark.Bytes:
		bodyReader = bytes.NewReader([]byte(body))
	default:
		return nil, fmt.Errorf("http_request: body must be a string or bytes, got %s", bodyVal.Type())
	}

	req, err := http.NewRequest(strings.ToUpper(method), urlStr, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("http_request: %w", err)
	}
	if headersDict != nil {
		if err := applyHeaders(req.Header, headersDict); err != nil {
			return nil, fmt.Errorf("http_request: headers: %w", err)
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http_request: %w", err)
	}

	// Read the whole body eagerly and close the real connection now, rather
	// than leaving it to responseValue's usual lazy loadBody — nothing else
	// is ever going to touch this response to read/close it for us the way
	// the real proxy pipeline does for flow.response.
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("http_request: read response body: %w", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))

	rv := newResponseValue(resp)
	rv.bodyLoaded = true
	rv.body = data
	return rv, nil
}
