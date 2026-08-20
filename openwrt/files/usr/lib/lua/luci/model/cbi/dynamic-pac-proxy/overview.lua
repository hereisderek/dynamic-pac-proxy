local sys = require "luci.sys"
local dsp = require "luci.dispatcher"

local m, s, o

m = Map("dynamic-pac-proxy", translate("Dynamic PAC Proxy"),
	translate("Forwards traffic to Charles (or another HTTP(S) proxy) " ..
		"resolved via mDNS when it's reachable, falling back to DIRECT " ..
		"otherwise. See the \"Hosts\" tab to configure which machines " ..
		"to proxy for."))

-- Service status + start/stop/restart controls.
s = m:section(SimpleSection, translate("Service"))

local running = (sys.call("/etc/init.d/dynamic-pac-proxy running >/dev/null 2>&1") == 0)
local action_url = dsp.build_url("admin", "services", "dynamic-pac-proxy", "action")

o = s:option(DummyValue, "_status", translate("Status"))
o.rawhtml = true
o.value = string.format(
	'<span style="font-weight:bold;color:%s">%s</span>' ..
	'&#160;&#160;' ..
	'<a class="cbi-button cbi-button-apply" href="%s?do=start">%s</a> ' ..
	'<a class="cbi-button cbi-button-reset" href="%s?do=stop">%s</a> ' ..
	'<a class="cbi-button cbi-button-save" href="%s?do=restart">%s</a>',
	running and "green" or "red",
	running and translate("RUNNING") or translate("STOPPED"),
	action_url, translate("Start"),
	action_url, translate("Stop"),
	action_url, translate("Restart")
)

local status_url = dsp.build_url("admin", "services", "dynamic-pac-proxy", "live_status")

o = s:option(DummyValue, "_live", translate("Host health"))
o.rawhtml = true
o.value = string.format([[
<div id="dpp-live-status">%s</div>
<script type="text/javascript">
(function() {
	var el = document.getElementById("dpp-live-status");
	function esc(s) {
		return String(s == null ? "" : s).replace(/[&<>]/g, function(c) {
			return c === "&" ? "&amp;" : (c === "<" ? "&lt;" : "&gt;");
		});
	}
	function refresh() {
		var xhr = new XMLHttpRequest();
		xhr.open("GET", "%s", true);
		xhr.onload = function() {
			if (xhr.status !== 200) { return; }
			var hosts;
			try { hosts = JSON.parse(xhr.responseText); } catch (e) { return; }
			var html = '<table class="table"><tr class="tr table-titles">' +
				'<th class="th">Name</th><th class="th">mDNS hostname</th>' +
				'<th class="th">Resolved IP</th><th class="th">Reachable</th>' +
				'<th class="th">Last check</th><th class="th">Error</th></tr>';
			for (var i = 0; i < hosts.length; i++) {
				var h = hosts[i];
				html += '<tr class="tr">' +
					'<td class="td">' + esc(h.name) + '</td>' +
					'<td class="td">' + esc(h.hostname) + '</td>' +
					'<td class="td">' + esc(h.resolved_ip || '-') + '</td>' +
					'<td class="td" style="color:' + (h.reachable ? 'green' : 'red') + '">' +
					(h.reachable ? 'yes' : 'no') + '</td>' +
					'<td class="td">' + esc(h.last_check || '-') + '</td>' +
					'<td class="td">' + esc(h.error || '') + '</td>' +
					'</tr>';
			}
			html += '</table>';
			if (hosts.length === 0) {
				html = '<em>no hosts configured</em>';
			}
			el.innerHTML = html;
		};
		xhr.send();
	}
	refresh();
	setInterval(refresh, 5000);
})();
</script>
]], translate("Loading…"), status_url)

-- Global settings, backed by the UCI "main" section.
s = m:section(NamedSection, "main", "main", translate("Settings"))

o = s:option(Flag, "enabled", translate("Enabled"))
o.rmempty = false
o.default = "1"

o = s:option(Value, "listen_addr", translate("Listen address"),
	translate("Where the status/PAC HTTP server listens, e.g. \":8080\". " ..
		"Changing this needs a service restart."))
o.rmempty = false
o.default = ":8080"

o = s:option(Value, "advertise_host", translate("Advertise host"),
	translate("The LAN IP or hostname client devices use to reach this " ..
		"router — this is what gets baked into the PAC files. Leave " ..
		"blank to auto-detect."))
o.rmempty = true

o = s:option(Value, "refresh_interval", translate("Refresh interval"),
	translate("Default health-check cache TTL for hosts that don't " ..
		"override it, e.g. \"15s\"."))
o.default = "15s"

o = s:option(Value, "mdns_timeout", translate("mDNS timeout"),
	translate("Default mDNS reply timeout, e.g. \"2s\"."))
o.default = "2s"

o = s:option(Value, "dial_timeout", translate("Dial timeout"),
	translate("Default TCP dial timeout, e.g. \"1s\"."))
o.default = "1s"

o = s:option(Value, "failure_cooldown", translate("Failure cooldown"),
	translate("Minimum time between failure-triggered early rechecks, " ..
		"e.g. \"5s\"."))
o.default = "5s"

function m.on_after_commit(map)
	sys.call("/etc/init.d/dynamic-pac-proxy reload >/dev/null 2>&1")
end

return m
