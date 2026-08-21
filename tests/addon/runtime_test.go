package addon_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/derekhud/dynamic-pac-proxy/internal/addon"
)

func writeScript(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newReq(method, url, body string) *http.Request {
	return httptest.NewRequest(method, url, strings.NewReader(body))
}

func TestRunRequestHeaderMutation(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "headers.star", `
def request(flow):
    flow.request.headers["X-Injected"] = "1"
    flow.request.headers["X-Multi"] = ["a", "b"]
    flow.request.headers.pop("X-Remove", None)
`)
	rt := addon.NewRuntime()
	req := newReq("GET", "http://example.com/", "")
	req.Header.Set("X-Remove", "gone")

	if err := rt.RunRequest(dir, "headers.star", req); err != nil {
		t.Fatalf("RunRequest: %v", err)
	}
	if got := req.Header.Get("X-Injected"); got != "1" {
		t.Fatalf("X-Injected = %q, want \"1\"", got)
	}
	if got := req.Header.Values("X-Multi"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("X-Multi = %v, want [a b]", got)
	}
	if got := req.Header.Get("X-Remove"); got != "" {
		t.Fatalf("X-Remove = %q, want deleted", got)
	}
}

func TestRunRequestURLRewrite(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "rewrite.star", `
def request(flow):
    flow.request.url = "https://rewritten.example.com/new-path"
`)
	rt := addon.NewRuntime()
	req := newReq("GET", "http://example.com/old-path", "")

	if err := rt.RunRequest(dir, "rewrite.star", req); err != nil {
		t.Fatalf("RunRequest: %v", err)
	}
	if got := req.URL.String(); got != "https://rewritten.example.com/new-path" {
		t.Fatalf("URL = %q", got)
	}
	if req.Host != "rewritten.example.com" {
		t.Fatalf("Host = %q, want rewritten.example.com", req.Host)
	}
}

func TestRunResponseTextAndStatusMutation(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "resp.star", `
def response(flow):
    flow.response.text = flow.response.text.replace("ads", "")
    flow.response.status_code = 201
    flow.response.headers["X-Modified"] = "yes"
`)
	rt := addon.NewRuntime()
	resp := &http.Response{
		StatusCode: 200,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("hello ads world")),
	}

	if err := rt.RunResponse(dir, "resp.star", resp); err != nil {
		t.Fatalf("RunResponse: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "hello  world" {
		t.Fatalf("body = %q", body)
	}
	if resp.StatusCode != 201 {
		t.Fatalf("StatusCode = %d, want 201", resp.StatusCode)
	}
	if resp.Header.Get("X-Modified") != "yes" {
		t.Fatal("missing X-Modified header")
	}
}

func TestRunRequestMissingHookIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "response-only.star", `
def response(flow):
    flow.response.status_code = 204
`)
	rt := addon.NewRuntime()
	req := newReq("GET", "http://example.com/", "")
	if err := rt.RunRequest(dir, "response-only.star", req); err != nil {
		t.Fatalf("a script with no request() hook should be a no-op, got: %v", err)
	}
}

func TestRunRequestCompileErrorRecoversAfterFix(t *testing.T) {
	dir := t.TempDir()
	path := writeScript(t, dir, "broken.star", "def request(flow)\n") // missing colon: syntax error

	rt := addon.NewRuntime()
	req := newReq("GET", "http://example.com/", "")
	if err := rt.RunRequest(dir, "broken.star", req); err == nil {
		t.Fatal("expected a compile error, got nil")
	}

	// Fix the file and force its mtime forward so the mtime-gated cache
	// actually notices the edit (a same-mtime rewrite would be missed).
	if err := os.WriteFile(path, []byte("def request(flow):\n    flow.request.headers[\"X-Fixed\"] = \"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	if err := rt.RunRequest(dir, "broken.star", req); err != nil {
		t.Fatalf("expected recovery after fixing the script, got: %v", err)
	}
	if req.Header.Get("X-Fixed") != "1" {
		t.Fatal("fixed script's mutation did not apply")
	}
}

func TestRunRequestErrorLeavesRequestUnmodified(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "fails.star", `
def request(flow):
    flow.request.headers["X-Set-Before-Fail"] = "1"
    fail("deliberate failure")
`)
	rt := addon.NewRuntime()
	req := newReq("GET", "http://example.com/", "")

	if err := rt.RunRequest(dir, "fails.star", req); err == nil {
		t.Fatal("expected an error from the failing hook")
	}
	if got := req.Header.Get("X-Set-Before-Fail"); got != "" {
		t.Fatalf("request should be unmodified when the hook call errors, got X-Set-Before-Fail=%q", got)
	}
}

func TestRunRequestMissingScriptFile(t *testing.T) {
	dir := t.TempDir()
	rt := addon.NewRuntime()
	req := newReq("GET", "http://example.com/", "")

	if err := rt.RunRequest(dir, "does-not-exist.star", req); err == nil {
		t.Fatal("expected an error for a missing script file")
	}
}

func TestRunRequestPathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	rt := addon.NewRuntime()
	req := newReq("GET", "http://example.com/", "")

	if err := rt.RunRequest(dir, "../escape.star", req); err == nil {
		t.Fatal("expected a traversal attempt to be rejected")
	}
}

func TestRunRequestExecutionStepGuard(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "slow.star", `
def request(flow):
    for i in range(100000000):
        flow.request.headers["X-Loop"] = str(i)
`)
	rt := addon.NewRuntime()
	req := newReq("GET", "http://example.com/", "")

	if err := rt.RunRequest(dir, "slow.star", req); err == nil {
		t.Fatal("expected the execution-step guard to cut off a runaway script")
	}
}

func TestRunRequestConcurrent(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "concurrent.star", `
def request(flow):
    flow.request.headers["X-Hit"] = "1"
`)
	rt := addon.NewRuntime()

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := newReq("GET", "http://example.com/", "")
			if err := rt.RunRequest(dir, "concurrent.star", req); err != nil {
				errs <- err
				return
			}
			if req.Header.Get("X-Hit") != "1" {
				errs <- fmt.Errorf("missing X-Hit header after concurrent RunRequest")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
