local sys = require "luci.sys"

local m, s, o

m = Map("dynamic-pac-proxy", translate("Dynamic PAC Proxy - Hosts"),
	translate("Each host gets its own fixed server_port on this router. " ..
		"Point a device's proxy settings (or a PAC URL at " ..
		"/proxy/&lt;name&gt;.pac) at this router's advertise_host:server_port " ..
		"— traffic is forwarded to that host's proxy when it's reachable " ..
		"via mDNS, or DIRECT when it's not."))

s = m:section(TypedSection, "host", translate("Hosts"))
s.addremove = true
s.anonymous = true
s.template = "cbi/tblsection"

o = s:option(Value, "name", translate("Name"),
	translate("Identifier used in URLs (/proxy/&lt;name&gt;.pac) and " ..
		"status output. Letters, digits, - and _ only; must be unique."))
o.rmempty = false

o = s:option(Value, "host_name", translate("mDNS hostname"),
	translate("Bonjour hostname to resolve, e.g. \"some-machine.local\"."))
o.rmempty = false

o = s:option(Value, "host_port", translate("Proxy port"),
	translate("The port the proxy (e.g. Charles) listens on, on the " ..
		"resolved host."))
o.datatype = "port"
o.rmempty = false

o = s:option(Value, "server_port", translate("Server port"),
	translate("The fixed port THIS router listens on for this host. " ..
		"Must be unique and different from the Listen address's port."))
o.datatype = "port"
o.rmempty = false

o = s:option(Value, "refresh_interval", translate("Refresh interval"),
	translate("Overrides the global default for this host only. Leave " ..
		"blank to inherit it."))

o = s:option(Value, "mdns_timeout", translate("mDNS timeout"),
	translate("Overrides the global default for this host only. Leave " ..
		"blank to inherit it."))

o = s:option(Value, "dial_timeout", translate("Dial timeout"),
	translate("Overrides the global default for this host only. Leave " ..
		"blank to inherit it."))

o = s:option(Value, "failure_cooldown", translate("Failure cooldown"),
	translate("Overrides the global default for this host only. Leave " ..
		"blank to inherit it."))

function m.on_after_commit(map)
	sys.call("/etc/init.d/dynamic-pac-proxy reload >/dev/null 2>&1")
end

return m
