package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckWSOrigin(t *testing.T) {
	allowed := parseAllowedOrigins("")
	cases := []struct {
		name   string
		origin string
		host   string
		want   bool
	}{
		{"empty origin loopback", "", "127.0.0.1:8080", true},
		{"empty origin non-loopback", "", "192.168.1.5:8080", false},
		{"localhost", "http://localhost:8080", "127.0.0.1:8080", true},
		{"127.0.0.1", "http://127.0.0.1:8080", "127.0.0.1:8080", true},
		{"same host", "http://192.168.1.5:8080", "192.168.1.5:8080", true},
		{"empty origin ipv6 loopback", "", "[::1]:8080", true},
		{"ipv6 origin", "http://[::1]:8080", "[::1]:8080", true},
		{"evil", "https://evil.example", "127.0.0.1:8080", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://"+tc.host+"/ws", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if got := checkWSOrigin(req, allowed); got != tc.want {
				t.Fatalf("checkWSOrigin() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCheckWSHost pins the DNS-rebinding defence: the Host header of a
// request to a loopback-bound listener must name loopback (or a configured
// allowed origin), because an attacker-controlled page rebinding its own
// domain to 127.0.0.1 sends its own name as Host.
func TestCheckWSHost(t *testing.T) {
	allowed := parseAllowedOrigins("")
	cases := []struct {
		name string
		host string
		want bool
	}{
		{"localhost with port", "localhost:8080", true},
		{"ipv4 loopback with port", "127.0.0.1:8080", true},
		{"ipv6 loopback with port", "[::1]:8080", true},
		{"ipv6 loopback bare", "[::1]", true},
		{"rebound domain", "evil.example:8080", false},
		{"lan address", "192.168.1.5:8080", false},
		{"empty host", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://127.0.0.1:8080/ws", nil)
			req.Host = tc.host
			if got := checkWSHost(req, allowed); got != tc.want {
				t.Fatalf("checkWSHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
	// A configured allowed origin stays honoured as the proxy escape hatch.
	if got := checkWSHost(&http.Request{Host: "gogen.internal:9000"}, parseAllowedOrigins("gogen.internal")); !got {
		t.Fatal("configured allowed origin should pass the Host check")
	}
}

func TestIsLoopbackBind(t *testing.T) {
	if !IsLoopbackBind("127.0.0.1:8080") {
		t.Fatal("expected loopback")
	}
	if !IsLoopbackBind("localhost:8080") {
		t.Fatal("expected localhost loopback")
	}
	if IsLoopbackBind("0.0.0.0:8080") {
		t.Fatal("0.0.0.0 should not be loopback")
	}
	if IsLoopbackBind(":8080") {
		t.Fatal(":8080 should not be loopback")
	}
}

func TestCheckAuth(t *testing.T) {
	s := &Server{authToken: "secret"}
	req := httptest.NewRequest("GET", "http://127.0.0.1:8080/", nil)
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: "secret"})
	if !s.checkAuth(req) {
		t.Fatal("cookie token should pass")
	}
	req = httptest.NewRequest("GET", "http://127.0.0.1:8080/", nil)
	req.Header.Set("Authorization", "Bearer secret")
	if !s.checkAuth(req) {
		t.Fatal("bearer should pass")
	}
	req = httptest.NewRequest("GET", "http://127.0.0.1:8080/", nil)
	req.Header.Set("X-Gogen-Token", "secret")
	if !s.checkAuth(req) {
		t.Fatal("header token should pass")
	}
	req = httptest.NewRequest("GET", "http://127.0.0.1:8080/?token=secret", nil)
	if s.checkAuth(req) {
		t.Fatal("query token alone should not authenticate WS/API requests")
	}
	req = httptest.NewRequest("GET", "http://127.0.0.1:8080/", nil)
	if s.checkAuth(req) {
		t.Fatal("missing token should fail")
	}
}

func TestParseAllowedOriginsCustom(t *testing.T) {
	allowed := parseAllowedOrigins("https://app.example.com, staging.local")
	if _, ok := allowed["app.example.com"]; !ok {
		t.Fatal("expected app.example.com")
	}
	if _, ok := allowed["staging.local"]; !ok {
		t.Fatal("expected staging.local")
	}
	// host:port entries must normalize to the bare hostname — checkWSOrigin
	// compares Hostname() (no port), so storing the port would never match.
	allowed = parseAllowedOrigins("example.com:8080")
	if _, ok := allowed["example.com"]; !ok {
		t.Fatal("expected example.com (port stripped)")
	}
}
