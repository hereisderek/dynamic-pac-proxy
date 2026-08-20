module("luci.controller.dynamic-pac-proxy", package.seeall)

local uci = require "luci.model.uci".cursor()
local sys = require "luci.sys"
local http = require "luci.http"
local dsp = require "luci.dispatcher"

function index()
	if not nixio.fs.access("/etc/config/dynamic-pac-proxy") then
		return
	end

	entry({"admin", "services", "dynamic-pac-proxy"},
		firstchild(), _("Dynamic PAC Proxy"), 50).dependent = false

	entry({"admin", "services", "dynamic-pac-proxy", "overview"},
		cbi("dynamic-pac-proxy/overview"), _("Overview"), 10)

	entry({"admin", "services", "dynamic-pac-proxy", "hosts"},
		cbi("dynamic-pac-proxy/hosts"), _("Hosts"), 20)

	entry({"admin", "services", "dynamic-pac-proxy", "action"},
		call("action_service")).leaf = true

	entry({"admin", "services", "dynamic-pac-proxy", "live_status"},
		call("live_status")).leaf = true
end

-- action_service runs the requested init.d verb and redirects back to the
-- overview page. The "do" value is checked against an exact whitelist
-- before ever reaching a shell command.
function action_service()
	local action = http.formvalue("do")
	local allowed = { start = true, stop = true, restart = true }
	if allowed[action] then
		sys.call(string.format("/etc/init.d/dynamic-pac-proxy %s >/dev/null 2>&1", action))
	end
	http.redirect(dsp.build_url("admin", "services", "dynamic-pac-proxy", "overview"))
end

-- live_status proxies the daemon's own /status JSON so the overview page
-- can poll it without a full page reload.
function live_status()
	local addr = uci:get("dynamic-pac-proxy", "main", "listen_addr") or ":8080"
	local port = addr:match(":(%d+)$") or "8080"
	local out = sys.exec(string.format("wget -qO- http://127.0.0.1:%s/status 2>/dev/null", port))

	http.prepare_content("application/json")
	if out and #out > 0 then
		http.write(out)
	else
		http.write("[]")
	end
end
