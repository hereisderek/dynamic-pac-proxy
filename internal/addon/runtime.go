// Package addon runs small user-supplied Starlark scripts ("addons")
// against decrypted HTTP requests/responses passing through this daemon,
// to inject or rewrite headers/content in transit — used by both the
// public reverse-proxy mirror (internal/edge) and, in the future, the
// existing intercept_ssl pipeline (internal/proxy).
//
// Scripts use the ".star" extension and a mitmproxy-flavored shape
// (top-level `def request(flow):` / `def response(flow):` functions,
// `flow.request`/`flow.response` objects) so a simple mitmproxy addon can
// be hand-ported — but this is Starlark (a Python-syntax subset, sandboxed,
// pure Go), not real Python or real mitmproxy compatibility: no imports,
// no classes, no filesystem/network access from a script, and no shared
// mutable state across requests (see compile/Freeze below).
package addon

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.starlark.net/starlark"
)

// maxExecutionSteps bounds how much work a single hook call may do.
// Starlark has no built-in step/time limit; without this, a pathological
// or accidentally-slow script (e.g. a large nested loop) would hang the
// request-handling goroutine indefinitely. The default OnMaxSteps behavior
// (thread.Cancel, surfaced as an EvalError from starlark.Call) is exactly
// what's wanted here — no custom watchdog goroutine needed.
const maxExecutionSteps = 10_000_000

// Runtime owns every compiled addon script's cached, frozen globals, keyed
// by absolute script path, so the parse/compile/Freeze cost is paid once
// per script file and amortized across every request that hits it — not
// once per request. One Runtime is shared process-wide.
type Runtime struct {
	mu      sync.RWMutex
	scripts map[string]*compiledScript
}

// compiledScript caches one script's compiled hook functions (or its
// compile error) alongside the mtime it was compiled from, so a broken
// script isn't recompiled on literally every request — only when its
// mtime moves, mirroring config.Store.ReloadIfChanged's own mtime-gated
// reload, but per-script and triggered by traffic rather than a poller.
type compiledScript struct {
	modTime  time.Time
	err      error
	request  starlark.Value // nil if the script defines no request() hook
	response starlark.Value // nil if the script defines no response() hook
}

func NewRuntime() *Runtime {
	return &Runtime{scripts: make(map[string]*compiledScript)}
}

// resolvePath joins addonsDir with relPath, rejecting any path that would
// escape addonsDir — mirrors the /certs/<name> traversal guard in
// internal/webui/certs.go. Unlike that guard, "/" is allowed (addons may
// live in subdirectories, e.g. "netflix/cookie.star"), only ".." is not.
func resolvePath(addonsDir, relPath string) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("addon: empty script path")
	}
	cleaned := filepath.Clean(relPath)
	if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("addon: script path %q escapes the addons directory", relPath)
	}
	return filepath.Join(addonsDir, cleaned), nil
}

// getCompiled returns the cached compile of addonsDir/relPath, recompiling
// if it's never been loaded or its mtime has changed since the cached
// compile. A compile error is cached too (so it isn't retried every
// request) and returned as this call's error.
func (rt *Runtime) getCompiled(addonsDir, relPath string) (*compiledScript, error) {
	absPath, err := resolvePath(addonsDir, relPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("addon %s: %w", relPath, err)
	}

	rt.mu.RLock()
	cached, ok := rt.scripts[absPath]
	rt.mu.RUnlock()
	if ok && cached.modTime.Equal(info.ModTime()) {
		return cached, cached.err
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()
	// Someone else may have recompiled while we were waiting for the lock.
	if cached, ok := rt.scripts[absPath]; ok && cached.modTime.Equal(info.ModTime()) {
		return cached, cached.err
	}

	compiled := compileScript(absPath)
	compiled.modTime = info.ModTime()
	rt.scripts[absPath] = compiled
	return compiled, compiled.err
}

// compileScript parses+executes a script's top level once to collect its
// request/response hook functions, then freezes the resulting globals.
//
// Freezing is required, not optional: ExecFile's returned StringDict holds
// mutable Starlark values until Freeze() is called, and *starlark.Thread
// is documented as not safe for concurrent use. Caching the compiled
// function values for concurrent per-request calls (each with its own
// fresh Thread, see RunRequest/RunResponse) is only safe once those values
// are frozen — which is also what turns a script that tries to keep
// mutable state across requests (e.g. a module-level counter mutated
// inside request()) into a loud "cannot mutate frozen value" error instead
// of a silent data race.
func compileScript(absPath string) *compiledScript {
	thread := &starlark.Thread{Name: "addon-compile:" + absPath}
	thread.SetMaxExecutionSteps(maxExecutionSteps)

	globals, err := starlark.ExecFile(thread, absPath, nil, nil)
	if err != nil {
		return &compiledScript{err: fmt.Errorf("addon %s: compile: %w", absPath, err)}
	}
	globals.Freeze()

	cs := &compiledScript{}
	if fn, ok := globals["request"]; ok {
		if _, isCallable := fn.(starlark.Callable); !isCallable {
			return &compiledScript{err: fmt.Errorf("addon %s: top-level \"request\" must be a function", absPath)}
		}
		cs.request = fn
	}
	if fn, ok := globals["response"]; ok {
		if _, isCallable := fn.(starlark.Callable); !isCallable {
			return &compiledScript{err: fmt.Errorf("addon %s: top-level \"response\" must be a function", absPath)}
		}
		cs.response = fn
	}
	return cs
}

// RunRequest runs addonsDir/relPath's request(flow) hook (if the script
// defines one — a response-only script is not an error) against r,
// mutating r in place only if the whole call succeeds. Never panics: a bug
// in this package's own flow-object glue is recovered and returned as an
// error, same as a Starlark-level compile/runtime error — callers are
// expected to log and continue (fail open), since one broken addon must
// never break a request that would otherwise have worked fine without it.
func (rt *Runtime) RunRequest(addonsDir, relPath string, r *http.Request) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("addon %s: panicked: %v", relPath, p)
		}
	}()

	cs, err := rt.getCompiled(addonsDir, relPath)
	if err != nil {
		return err
	}
	if cs.request == nil {
		return nil
	}

	reqVal := newRequestValue(r)
	flow := &flowValue{request: reqVal}

	thread := &starlark.Thread{Name: "addon-request:" + relPath}
	thread.SetMaxExecutionSteps(maxExecutionSteps)
	if _, err := starlark.Call(thread, cs.request, starlark.Tuple{flow}, nil); err != nil {
		return fmt.Errorf("addon %s: request(): %w", relPath, err)
	}
	return reqVal.apply()
}

// RunResponse is RunRequest's response-hook counterpart. flow.request is
// exposed alongside flow.response for context (mirroring mitmproxy), built
// from resp.Request — but only flow.response's mutations are ever applied
// back; by the time a response exists, the request has already been sent.
func (rt *Runtime) RunResponse(addonsDir, relPath string, resp *http.Response) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("addon %s: panicked: %v", relPath, p)
		}
	}()

	cs, err := rt.getCompiled(addonsDir, relPath)
	if err != nil {
		return err
	}
	if cs.response == nil {
		return nil
	}

	flow := &flowValue{response: newResponseValue(resp)}
	if resp.Request != nil {
		flow.request = newRequestValue(resp.Request)
	}

	thread := &starlark.Thread{Name: "addon-response:" + relPath}
	thread.SetMaxExecutionSteps(maxExecutionSteps)
	if _, err := starlark.Call(thread, cs.response, starlark.Tuple{flow}, nil); err != nil {
		return fmt.Errorf("addon %s: response(): %w", relPath, err)
	}
	return flow.response.apply()
}
