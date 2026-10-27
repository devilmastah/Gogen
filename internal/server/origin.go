package server

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

var defaultAllowedHosts = map[string]struct{}{
	"localhost": {},
	"127.0.0.1": {},
	"::1":       {},
}

func parseAllowedOrigins(raw string) map[string]struct{} {
	// Entries are stored as bare hostnames: checkWSOrigin compares
	// u.Hostname() (which strips brackets) and parseAllowedOrigins trims
	// brackets below, so a bracketed "[::1]" key could never match — the
	// bare "::1" form is the only one that works.
	raw = strings.TrimSpace(raw)
	if raw == "" {
		out := make(map[string]struct{}, len(defaultAllowedHosts))
		for k, v := range defaultAllowedHosts {
			out[k] = v
		}
		return out
	}
	out := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// checkWSOrigin compares Hostname() (no port), so store hostnames —
		// not host:port strings — or entries like "example.com:8080" would
		// silently never match. Handle both the schemed form (url.Parse) and
		// a bare "host:port" (which parses as an opaque URL with no Host).
		host := ""
		if u, err := url.Parse(part); err == nil && u.Host != "" {
			host = u.Hostname()
		} else if h, _, err := net.SplitHostPort(part); err == nil && h != "" {
			host = h
		}
		if host == "" {
			host = strings.TrimPrefix(part, ".")
		}
		// IPv6 entries may carry brackets ("[::1]") — Hostname() strips them,
		// so store the bare form for a consistent lookup.
		out[strings.ToLower(strings.Trim(host, "[]"))] = struct{}{}
	}
	return out
}

// loopbackHosts are the Host-header names that address a loopback listener.
// Bracket stripping is done by checkWSHost, so "::1" is enough.
var loopbackHosts = map[string]struct{}{
	"localhost": {},
	"127.0.0.1": {},
	"::1":       {},
}

// hostOnly strips the port (and IPv6 brackets) from a Host-header value.
func hostOnly(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	// A bare "[::1]" (no port) leaves the brackets: strip them.
	return strings.Trim(host, "[]")
}

// checkWSHost rejects a WebSocket request whose Host header does not address
// the loopback listener the server is bound to (see Server.loopbackBind).
//
// This is the DNS-rebinding defence the Origin check cannot provide: under
// rebinding the attacker controls BOTH the Origin and the Host header, so
// checkWSOrigin's "Origin host == Host" fallback passes while the request
// actually reaches 127.0.0.1 with Host: evil.example. The Host still names
// the attacker's domain, which is what this rejects. allowed
// (web_allowed_origins) is honoured as an escape hatch for a loopback bind
// fronted by a proxy.
func checkWSHost(r *http.Request, allowed map[string]struct{}) bool {
	host := hostOnly(r.Host)
	if _, ok := loopbackHosts[host]; ok {
		return true
	}
	_, ok := allowed[host]
	return ok
}

func checkWSOrigin(r *http.Request, allowed map[string]struct{}) bool {
	if len(allowed) == 0 {
		allowed = defaultAllowedHosts
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	// Host header may carry a port ("localhost:8081") and, for IPv6, brackets
	// ("[::1]:8081"). Splitting on ":" alone turns "[::1]:8081" into "[" and
	// broke the loopback / same-host checks for IPv6 clients. SplitHostPort
	// handles both; trim brackets for the comparison.
	reqHost := hostOnly(r.Host)
	loopback := reqHost == "localhost" || reqHost == "127.0.0.1" || reqHost == "::1"
	if origin == "" {
		// Non-browser clients (curl, etc.) omit Origin. Only allow that on loopback.
		return loopback
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if _, ok := allowed[host]; ok {
		return true
	}
	// Allow the UI served from the same host (Origin host:port vs Host header).
	return host == reqHost
}
