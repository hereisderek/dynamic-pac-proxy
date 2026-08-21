package addon

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"

	"go.starlark.net/starlark"
)

// flowValue is the "flow" object passed to a script's request(flow)/
// response(flow) hooks, mirroring mitmproxy's own flow.request/
// flow.response shape closely enough that simple mitmproxy addons can be
// hand-ported. It's a throwaway, per-call scratch object — never cached or
// shared across goroutines — so Freeze is a deliberate no-op rather than a
// real deep-freeze; nothing outside this one Starlark call ever sees it.
type flowValue struct {
	request  *requestValue
	response *responseValue // nil during the request() hook
}

func (f *flowValue) String() string        { return "<flow>" }
func (f *flowValue) Type() string          { return "flow" }
func (f *flowValue) Freeze()               {}
func (f *flowValue) Truth() starlark.Bool  { return starlark.True }
func (f *flowValue) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable type: flow") }

func (f *flowValue) Attr(name string) (starlark.Value, error) {
	switch name {
	case "request":
		if f.request == nil {
			return starlark.None, nil
		}
		return f.request, nil
	case "response":
		if f.response == nil {
			return starlark.None, nil
		}
		return f.response, nil
	}
	return nil, nil
}

func (f *flowValue) AttrNames() []string { return []string{"request", "response"} }

var (
	_ starlark.HasAttrs = (*flowValue)(nil)
)

// requestValue is a Go-backed, mutable Starlark value wrapping *http.Request
// — read fresh at the start of a hook call, and (for the request hook only)
// written back onto the real request via apply() if the whole Starlark call
// succeeds. Body access is lazy: a script that never touches .text/.content
// never pays for buffering it.
type requestValue struct {
	r       *http.Request
	method  string
	urlStr  string
	headers *starlark.Dict

	bodyLoaded bool
	bodyDirty  bool
	body       []byte
	// bodyEncoding is the Content-Encoding loadBody transparently decoded
	// away to produce body, or "" if the body was already identity-encoded
	// (or never loaded). apply() uses it to drop a now-stale
	// Content-Encoding header when the script committed a plain-decoded
	// replacement body without touching headers itself.
	bodyEncoding string
}

func newRequestValue(r *http.Request) *requestValue {
	return &requestValue{
		r:       r,
		method:  r.Method,
		urlStr:  r.URL.String(),
		headers: headerToDict(r.Header),
	}
}

func (v *requestValue) String() string        { return "<request>" }
func (v *requestValue) Type() string          { return "request" }
func (v *requestValue) Freeze()               {}
func (v *requestValue) Truth() starlark.Bool  { return starlark.True }
func (v *requestValue) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable type: request") }

func (v *requestValue) Attr(name string) (starlark.Value, error) {
	switch name {
	case "method":
		return starlark.String(v.method), nil
	case "url":
		return starlark.String(v.urlStr), nil
	case "headers":
		return v.headers, nil
	case "text":
		body, err := v.loadBody()
		if err != nil {
			return nil, err
		}
		return starlark.String(string(body)), nil
	case "content":
		body, err := v.loadBody()
		if err != nil {
			return nil, err
		}
		return starlark.Bytes(body), nil
	}
	return nil, nil
}

func (v *requestValue) AttrNames() []string {
	return []string{"method", "url", "headers", "text", "content"}
}

func (v *requestValue) SetField(name string, val starlark.Value) error {
	switch name {
	case "method":
		s, ok := starlark.AsString(val)
		if !ok {
			return fmt.Errorf("flow.request.method must be a string")
		}
		v.method = s
		return nil
	case "url":
		s, ok := starlark.AsString(val)
		if !ok {
			return fmt.Errorf("flow.request.url must be a string")
		}
		v.urlStr = s
		return nil
	case "text":
		s, ok := starlark.AsString(val)
		if !ok {
			return fmt.Errorf("flow.request.text must be a string")
		}
		v.body = []byte(s)
		v.bodyLoaded = true
		v.bodyDirty = true
		return nil
	case "content":
		b, ok := val.(starlark.Bytes)
		if !ok {
			return fmt.Errorf("flow.request.content must be bytes")
		}
		v.body = []byte(b)
		v.bodyLoaded = true
		v.bodyDirty = true
		return nil
	case "headers":
		return fmt.Errorf("flow.request.headers cannot be reassigned directly — mutate flow.request.headers[...] instead")
	}
	return starlark.NoSuchAttrError(fmt.Sprintf("request has no attribute %q", name))
}

func (v *requestValue) loadBody() ([]byte, error) {
	if v.bodyLoaded {
		return v.body, nil
	}
	v.bodyLoaded = true
	if v.r.Body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(v.r.Body)
	v.r.Body.Close()
	// Restore a fresh reader over whatever was actually read, even on
	// error, so a script that fails partway through doesn't leave the
	// real request with an already-drained, now-unusable body. This
	// restores the raw (still-encoded) bytes — decoding below only
	// affects what the script sees via .text/.content, never the wire
	// body unless the script actually commits a replacement.
	v.r.Body = io.NopCloser(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	decoded, enc, err := decodeBody(v.r.Header, data)
	if err != nil {
		return nil, fmt.Errorf("flow.request: %w", err)
	}
	v.body = decoded
	v.bodyEncoding = enc
	return decoded, nil
}

// apply writes any mutations back onto the real *http.Request. Only called
// once the whole Starlark call for this addon has succeeded — a script
// that errors partway through a mutation never leaves a half-applied
// request in flight.
//
// Every fallible step (parsing the rewritten URL, validating the header
// dict) is done before anything is written to the real request, so an
// error here — e.g. an invalid header value — leaves v.r completely
// untouched instead of partially rewritten.
func (v *requestValue) apply() error {
	newHeaders, err := buildHeaders(v.headers)
	if err != nil {
		return fmt.Errorf("flow.request.headers: %w", err)
	}

	var newURL *url.URL
	urlChanged := v.urlStr != v.r.URL.String()
	if urlChanged {
		newURL, err = url.Parse(v.urlStr)
		if err != nil {
			return fmt.Errorf("flow.request.url: invalid rewritten URL %q: %w", v.urlStr, err)
		}
		if (newURL.Scheme != "http" && newURL.Scheme != "https") || newURL.Host == "" {
			return fmt.Errorf("flow.request.url: rewritten URL %q must be an absolute http(s) URL with a host", v.urlStr)
		}
	}

	if v.bodyDirty && v.bodyEncoding != "" && newHeaders.Get("Content-Encoding") == v.bodyEncoding {
		// The script committed a plain-decoded body but never touched
		// the still-stale Content-Encoding header itself — drop it so
		// whatever sends this request on doesn't try to re-decode an
		// already-decoded body.
		newHeaders.Del("Content-Encoding")
	}

	// Nothing above can fail past this point — commit everything.
	v.r.Method = v.method
	if urlChanged {
		v.r.URL = newURL
		v.r.Host = newURL.Host
	}
	commitHeaders(v.r.Header, newHeaders)
	if v.bodyDirty {
		if v.r.Body != nil {
			v.r.Body.Close()
		}
		v.r.Body = io.NopCloser(bytes.NewReader(v.body))
		v.r.ContentLength = int64(len(v.body))
		v.r.Header.Set("Content-Length", strconv.Itoa(len(v.body)))
		v.r.Header.Del("Transfer-Encoding")
	}
	return nil
}

// responseValue is responseValue's counterpart for *http.Response.
type responseValue struct {
	resp       *http.Response
	statusCode int
	headers    *starlark.Dict

	bodyLoaded   bool
	bodyDirty    bool
	body         []byte
	bodyEncoding string // see requestValue.bodyEncoding
}

func newResponseValue(resp *http.Response) *responseValue {
	return &responseValue{
		resp:       resp,
		statusCode: resp.StatusCode,
		headers:    headerToDict(resp.Header),
	}
}

func (v *responseValue) String() string        { return "<response>" }
func (v *responseValue) Type() string          { return "response" }
func (v *responseValue) Freeze()               {}
func (v *responseValue) Truth() starlark.Bool  { return starlark.True }
func (v *responseValue) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable type: response") }

func (v *responseValue) Attr(name string) (starlark.Value, error) {
	switch name {
	case "status_code":
		return starlark.MakeInt(v.statusCode), nil
	case "headers":
		return v.headers, nil
	case "text":
		body, err := v.loadBody()
		if err != nil {
			return nil, err
		}
		return starlark.String(string(body)), nil
	case "content":
		body, err := v.loadBody()
		if err != nil {
			return nil, err
		}
		return starlark.Bytes(body), nil
	}
	return nil, nil
}

func (v *responseValue) AttrNames() []string {
	return []string{"status_code", "headers", "text", "content"}
}

func (v *responseValue) SetField(name string, val starlark.Value) error {
	switch name {
	case "status_code":
		i, ok := val.(starlark.Int)
		if !ok {
			return fmt.Errorf("flow.response.status_code must be an int")
		}
		n, ok := i.Int64()
		if !ok || n < 100 || n > 599 {
			return fmt.Errorf("flow.response.status_code must be a valid HTTP status code")
		}
		v.statusCode = int(n)
		return nil
	case "text":
		s, ok := starlark.AsString(val)
		if !ok {
			return fmt.Errorf("flow.response.text must be a string")
		}
		v.body = []byte(s)
		v.bodyLoaded = true
		v.bodyDirty = true
		return nil
	case "content":
		b, ok := val.(starlark.Bytes)
		if !ok {
			return fmt.Errorf("flow.response.content must be bytes")
		}
		v.body = []byte(b)
		v.bodyLoaded = true
		v.bodyDirty = true
		return nil
	case "headers":
		return fmt.Errorf("flow.response.headers cannot be reassigned directly — mutate flow.response.headers[...] instead")
	}
	return starlark.NoSuchAttrError(fmt.Sprintf("response has no attribute %q", name))
}

func (v *responseValue) loadBody() ([]byte, error) {
	if v.bodyLoaded {
		return v.body, nil
	}
	v.bodyLoaded = true
	if v.resp.Body == nil {
		return nil, nil
	}
	data, err := io.ReadAll(v.resp.Body)
	v.resp.Body.Close()
	// Same "restore a fresh reader over whatever was read" rule as
	// requestValue.loadBody — a read error here must not leave the real
	// response's body half-consumed for whatever sends it on afterward.
	// This restores the raw (still-encoded) bytes; decoding below only
	// affects what the script sees via .text/.content.
	v.resp.Body = io.NopCloser(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	decoded, enc, err := decodeBody(v.resp.Header, data)
	if err != nil {
		return nil, fmt.Errorf("flow.response: %w", err)
	}
	v.body = decoded
	v.bodyEncoding = enc
	return decoded, nil
}

// apply is requestValue.apply's response counterpart: headers are fully
// validated into a temporary before anything is written to the real
// response, so an invalid header value can't leave status/headers/body
// partially committed.
func (v *responseValue) apply() error {
	newHeaders, err := buildHeaders(v.headers)
	if err != nil {
		return fmt.Errorf("flow.response.headers: %w", err)
	}

	if v.bodyDirty && v.bodyEncoding != "" && newHeaders.Get("Content-Encoding") == v.bodyEncoding {
		newHeaders.Del("Content-Encoding")
	}

	v.resp.StatusCode = v.statusCode
	v.resp.Status = fmt.Sprintf("%d %s", v.statusCode, http.StatusText(v.statusCode))
	commitHeaders(v.resp.Header, newHeaders)
	if v.bodyDirty {
		if v.resp.Body != nil {
			v.resp.Body.Close()
		}
		v.resp.Body = io.NopCloser(bytes.NewReader(v.body))
		v.resp.ContentLength = int64(len(v.body))
		v.resp.Header.Set("Content-Length", strconv.Itoa(len(v.body)))
		v.resp.Header.Del("Transfer-Encoding")
	}
	return nil
}

// headerToDict builds a fresh Starlark dict from an http.Header — a header
// with exactly one value becomes a plain string, one with two or more
// (e.g. repeated Set-Cookie) becomes a list of strings.
func headerToDict(h http.Header) *starlark.Dict {
	d := starlark.NewDict(len(h))
	for k, vals := range h {
		key := textproto.CanonicalMIMEHeaderKey(k)
		if len(vals) == 1 {
			d.SetKey(starlark.String(key), starlark.String(vals[0]))
			continue
		}
		list := starlark.NewList(nil)
		for _, val := range vals {
			list.Append(starlark.String(val))
		}
		d.SetKey(starlark.String(key), list)
	}
	return d
}

// buildHeaders validates d and returns a brand-new http.Header built from
// it, without touching any real request/response header map — kept
// separate from committing so a script that sets an invalid header value
// (a script can mutate flow.*.headers[...] directly, bypassing SetField's
// own checks) fails apply() before anything real is mutated, rather than
// after some headers have already been cleared/rewritten.
func buildHeaders(d *starlark.Dict) (http.Header, error) {
	h := make(http.Header, d.Len())
	for _, item := range d.Items() {
		keyStr, ok := starlark.AsString(item[0])
		if !ok {
			return nil, fmt.Errorf("header keys must be strings")
		}
		key := textproto.CanonicalMIMEHeaderKey(keyStr)
		switch val := item[1].(type) {
		case starlark.String:
			h.Set(key, string(val))
		case *starlark.List:
			first := true
			iter := val.Iterate()
			var elem starlark.Value
			for iter.Next(&elem) {
				s, ok := starlark.AsString(elem)
				if !ok {
					iter.Done()
					return nil, fmt.Errorf("header %q: list values must be strings", key)
				}
				if first {
					h.Set(key, s)
					first = false
				} else {
					h.Add(key, s)
				}
			}
			iter.Done()
		default:
			return nil, fmt.Errorf("header %q: value must be a string or a list of strings", key)
		}
	}
	return h, nil
}

// commitHeaders replaces h's contents with src, in place — h keeps its
// identity (the real *http.Request/*http.Response's Header map), only its
// contents change, and only once src has been fully built and validated.
func commitHeaders(h http.Header, src http.Header) {
	for k := range h {
		delete(h, k)
	}
	for k, vals := range src {
		h[k] = vals
	}
}

// decodeBody transparently decodes raw according to header's
// Content-Encoding, so flow.*.text/.content read the same bytes a client
// would see after transport decompression instead of raw compressed bytes
// masquerading as text. Returns the encoding it decoded (or "" for an
// already-identity body) so apply() can drop a now-stale Content-Encoding
// header when a script replaces the body without touching headers itself.
// An encoding this daemon can't decode (e.g. br, zstd) fails loudly rather
// than silently handing a script compressed bytes as "text".
func decodeBody(header http.Header, raw []byte) (data []byte, encoding string, err error) {
	enc := strings.ToLower(strings.TrimSpace(header.Get("Content-Encoding")))
	switch enc {
	case "", "identity":
		return raw, "", nil
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, "", fmt.Errorf("decode gzip body: %w", err)
		}
		defer zr.Close()
		data, err := io.ReadAll(zr)
		if err != nil {
			return nil, "", fmt.Errorf("decode gzip body: %w", err)
		}
		return data, enc, nil
	case "deflate":
		zr := flate.NewReader(bytes.NewReader(raw))
		defer zr.Close()
		data, err := io.ReadAll(zr)
		if err != nil {
			return nil, "", fmt.Errorf("decode deflate body: %w", err)
		}
		return data, enc, nil
	default:
		return nil, "", fmt.Errorf("body has unsupported Content-Encoding %q; can't safely expose .text/.content", enc)
	}
}
