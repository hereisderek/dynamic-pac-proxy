# deploy/addons/netflix/cookie.star
#
# Sample addon for the "netflix" site in deploy/config.yaml — demonstrates
# request/response header modification. Addon scripts are Starlark (a
# sandboxed, Python-syntax *subset* — not real Python), so only the
# builtins Starlark itself provides are available; see the "Addon
# scripts" section of README.md for the full flow API this relies on.
#
# Scenario: netflix.com issues Set-Cookie headers scoped to its own
# Domain attribute. Proxied through to a browser talking to
# netflix.mydomain.com instead, those cookies would never actually be
# accepted (a cookie's Domain must match the site the browser thinks
# it's talking to) — so this rewrites Domain to the mirror's own domain
# on the way back, and tags both directions with a header so it's easy
# to confirm from devtools that this addon actually ran.

MIRROR_DOMAIN = "netflix.mydomain.com"

# http_request(url, method="GET", headers=None, body=None) lets an addon do
# real work before rewriting a request/response — e.g. fetching a fresh
# session cookie from an internal auth endpoint. It returns an object with
# the same status_code/headers/text/content shape as flow.response. Left
# disabled by default (AUTH_COOKIE_URL == "") so this sample never makes a
# real network call on its own; set the URL to try it.
AUTH_COOKIE_URL = ""

def request(flow):
    flow.request.headers["X-Proxied-By"] = "dynamic-pac-proxy"

    if AUTH_COOKIE_URL != "":
        auth = http_request(AUTH_COOKIE_URL)
        if auth.status_code == 200:
            flow.request.headers["Cookie"] = auth.headers.get("Set-Cookie", auth.text)

def response(flow):
    flow.response.headers["X-Modified-By"] = "dynamic-pac-proxy"

    set_cookie = flow.response.headers.get("Set-Cookie")
    if set_cookie == None:
        return

    # A response with exactly one Set-Cookie header reads back as a plain
    # string; two or more (multiple cookies set at once) read back as a
    # list of strings — normalize to a list either way so the rewrite
    # logic below only needs to handle one shape.
    cookies = set_cookie if type(set_cookie) == "list" else [set_cookie]

    rewritten = []
    for cookie in cookies:
        before, sep, after = cookie.partition("Domain=")
        if sep == "":
            rewritten.append(cookie)
            continue
        _, semi, rest = after.partition(";")
        new_cookie = before + "Domain=" + MIRROR_DOMAIN
        if semi != "":
            new_cookie += ";" + rest
        rewritten.append(new_cookie)

    flow.response.headers["Set-Cookie"] = rewritten if len(rewritten) > 1 else rewritten[0]
