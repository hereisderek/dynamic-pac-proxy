package addon

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"

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
	// real request with an already-drained, now-unusable body.
	v.r.Body = io.NopCloser(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	v.body = data
	return data, nil
}

// apply writes any mutations back onto the real *http.Request. Only called
// once the whole Starlark call for this addon has succeeded — a script
// that errors partway through a mutation never leaves a half-applied
// request in flight.
func (v *requestValue) apply() error {
	if v.method != v.r.Method {
		v.r.Method = v.method
	}
	if v.urlStr != v.r.URL.String() {
		u, err := url.Parse(v.urlStr)
		if err != nil {
			return fmt.Errorf("flow.request.url: invalid rewritten URL %q: %w", v.urlStr, err)
		}
		v.r.URL = u
		v.r.Host = u.Host
	}
	if err := applyHeaders(v.r.Header, v.headers); err != nil {
		return fmt.Errorf("flow.request.headers: %w", err)
	}
	if v.bodyDirty {
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

	bodyLoaded bool
	bodyDirty  bool
	body       []byte
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
	v.resp.Body = io.NopCloser(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	v.body = data
	return data, nil
}

func (v *responseValue) apply() error {
	v.resp.StatusCode = v.statusCode
	v.resp.Status = fmt.Sprintf("%d %s", v.statusCode, http.StatusText(v.statusCode))
	if err := applyHeaders(v.resp.Header, v.headers); err != nil {
		return fmt.Errorf("flow.response.headers: %w", err)
	}
	if v.bodyDirty {
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

// applyHeaders rewrites h from scratch to match d — handles both additions
// and deletions (a script doing `flow.request.headers.pop("X", None)` —
// Starlark, unlike Python, has no `del` statement) for free, since it's a
// full rebuild rather than a diff.
func applyHeaders(h http.Header, d *starlark.Dict) error {
	for k := range h {
		delete(h, k)
	}
	for _, item := range d.Items() {
		keyStr, ok := starlark.AsString(item[0])
		if !ok {
			return fmt.Errorf("header keys must be strings")
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
					return fmt.Errorf("header %q: list values must be strings", key)
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
			return fmt.Errorf("header %q: value must be a string or a list of strings", key)
		}
	}
	return nil
}
