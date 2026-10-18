package server

// Login-flow integration tests: the full onboarding/auth lifecycle over a
// real HTTP + WebSocket mux, with the real handlers (HandleStatic,
// HandleWS, HandleWSEditor), real pairing-code state, and real cookies.
//
// Covers: token bootstrap, pairing bootstrap, all failure paths (wrong /
// expired / exhausted / missing code, wrong token, rate limit), every
// credential method (cookie, Bearer, X-Gogen-Token), the rule that the
// query token never authenticates API/WS requests, and the restart
// lifecycle (login persists because the token is stable; the pairing code
// is per-boot and dies on restart).

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	llmtest "gogen/internal/llm/llmtest"
)

// loginFlowServer is a real server with token auth (and optionally a
// pairing code) behind the real mux paths.
type loginFlowServer struct {
	s   *Server
	srv *httptest.Server
}

// newLoginFlowServer builds a server authenticating with token. When
// pairCode != "", the given pairing code is installed with pairExpiry.
func newLoginFlowServer(t *testing.T, token, pairCode string, pairExpiry time.Time) *loginFlowServer {
	t.Helper()
	dir := t.TempDir()
	prov := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.DefaultSettings())
	a := agent.NewAgent(prov, exec, ctxMgr)
	s := newTestServer(t, a, &config.Config{WebAuthToken: token})
	if pairCode != "" {
		s.SetPairingCode(pairCode, pairExpiry)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ws":
			s.HandleWS(w, r)
		case "/ws/editor":
			s.HandleWSEditor(w, r)
		default:
			s.HandleStatic(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &loginFlowServer{s: s, srv: srv}
}

func (l *loginFlowServer) get(t *testing.T, path string, cookies []*http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", l.srv.URL+path, nil)
	// Pairing exchanges only consume the code for real navigations; the
	// login-flow tests exercise the browser flow, so mark GETs as
	// navigations (callers can override via headers).
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	l.srv.Config.Handler.ServeHTTP(rec, req)
	return rec
}

func authCookie(token string) *http.Cookie {
	return &http.Cookie{Name: authCookieName, Value: token}
}

// assertPairingPage asserts the sign-in page was served: form present, no
// unreplaced placeholder, and the given message fragment in the body.
func assertPairingPage(t *testing.T, rec *httptest.ResponseRecorder, msgFragment string) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="pair"`) {
		t.Fatalf("body should contain the pairing-code input, got: %s", body)
	}
	if !strings.Contains(body, "Sign in") {
		t.Fatalf("body should contain the submit button, got: %s", body)
	}
	if strings.Contains(body, "{{MESSAGE}}") {
		t.Fatal("body must not contain the unreplaced {{MESSAGE}} placeholder")
	}
	if msgFragment != "" && !strings.Contains(body, msgFragment) {
		t.Fatalf("body should contain %q, got: %s", msgFragment, body)
	}
}

// dialWSAuth opens a websocket to path with the given extra headers,
// returning the connection and HTTP status (0 when the dial succeeded).
func dialWSAuth(t *testing.T, l *loginFlowServer, path string, headers map[string]string) (*websocket.Conn, int) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(l.srv.URL, "http") + path
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	d := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	conn, resp, err := d.Dial(url, h)
	if err != nil {
		if resp != nil {
			return nil, resp.StatusCode
		}
		t.Fatalf("dial %s: %v", path, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, 0
}

// TestLoginFlowFullBrowserChainWithCookieJar is the end-to-end referee for
// the "pairing accepted but the browser never keeps the cookie" report: a
// real HTTP server (the real mux), a real redirect-following client whose
// request reproduces the phone's trace byte-for-byte (plain HTTP — no
// Sec-Fetch-* headers are sent to non-trustworthy origins — plus the exact
// Accept, Upgrade-Insecure-Requests and User-Agent), and a real
// net/http/cookiejar as an independent RFC 6265 referee. If the server
// emitted an unstorable or malformed Set-Cookie, this fails here instead
// of in a user's pocket. Both the QR path form and the typed-code form
// are covered, with a production-shaped token (64 hex chars).
func TestLoginFlowFullBrowserChainWithCookieJar(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const code = "fedcba0987654321fedcba0987654321fedcba0987654321fedcba0987654321"
	l := newLoginFlowServer(t, token, code, time.Now().Add(PairingTTL))

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	client := &http.Client{Jar: jar}

	// phoneScan reproduces the exact GET the phone's browser made.
	phoneScan := func(url string) *http.Response {
		t.Helper()
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
		req.Header.Set("Upgrade-Insecure-Requests", "1")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Mobile Safari/537.36")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	// 1. Scan the QR (path form). The 302's Set-Cookie must land in the jar.
	resp := phoneScan(l.srv.URL + "/pair/" + code)
	if resp.Request.URL.Path != "/" {
		t.Fatalf("client did not follow the redirect to /, ended at %s", resp.Request.URL)
	}
	u, _ := url.Parse(l.srv.URL)
	var stored *http.Cookie
	for _, c := range jar.Cookies(u) {
		if c.Name == authCookieName {
			stored = c
		}
	}
	if stored == nil {
		t.Fatalf("cookie jar did not store %s — the Set-Cookie is not storable by an RFC 6265 client", authCookieName)
	}
	if stored.Value != token {
		t.Fatalf("stored cookie value mismatch (len %d, want %d)", len(stored.Value), len(token))
	}
	if stored.Secure {
		t.Fatal("plain-HTTP exchange must not set the Secure attribute (browsers reject Secure cookies over HTTP)")
	}
	// 2. The redirect-follow itself must already be authenticated: the jar
	// echoes the cookie on the very next request — exactly what the phone
	// failed to do one second after its accepted exchange.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("final status=%d, want 200 (cookie must authenticate the redirect follow)", resp.StatusCode)
	}

	// 3. A later navigation in the same context stays logged in.
	if resp := phoneScan(l.srv.URL + "/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("subsequent navigation status=%d, want 200", resp.StatusCode)
	}

	// 4. The typed-code form (the sign-in page's GET form) works the same.
	jar2, _ := cookiejar.New(nil)
	client.Jar = jar2
	resp = phoneScan(l.srv.URL + "/?pair=" + code)
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/" {
		t.Fatalf("typed-code exchange: status=%d path=%s, want 200 at /", resp.StatusCode, resp.Request.URL.Path)
	}
}

// ---- Token bootstrap ----

func TestLoginFlowTokenBootstrap(t *testing.T) {
	const token = "tok-12345"
	l := newLoginFlowServer(t, token, "", time.Time{})

	// No credentials at all → 401 + pairing page (with the code form).
	rec := l.get(t, "/", nil, nil)
	assertPairingPage(t, rec, "Enter the pairing code")

	// Correct token → 302 + HttpOnly cookie, redirect strips the query.
	rec = l.get(t, "/?token="+token, nil, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Fatalf("Location=%q, want /", loc)
	}
	c := getCookie(t, rec)
	if c.Value != token {
		t.Fatalf("cookie value=%q, want %q", c.Value, token)
	}
	if c.MaxAge != 30*24*60*60 {
		t.Fatalf("cookie MaxAge=%d, want 30 days", c.MaxAge)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie flags: HttpOnly=%v SameSite=%v, want strict+httponly", c.HttpOnly, c.SameSite)
	}

	// The cookie now authenticates the app.
	rec = l.get(t, "/", []*http.Cookie{authCookie(token)}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("cookie-authenticated request status=%d, want 200", rec.Code)
	}

	// Wrong token → 401, no cookie.
	rec = l.get(t, "/?token=wrong", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-token status=%d, want 401", rec.Code)
	}
}

// ---- Pairing bootstrap ----

func TestLoginFlowPairingBootstrap(t *testing.T) {
	const token = "tok-pair"
	const code = "paircode-123"
	l := newLoginFlowServer(t, token, code, time.Now().Add(PairingTTL))

	// The QR encodes the code in the PATH (/pair/<code>) because phone
	// camera apps and in-app browsers strip query strings. Scanning must
	// log the phone in directly — no page, no typing.
	rec := l.get(t, "/pair/"+code, nil, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("scan status=%d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Fatalf("Location=%q, want /", loc)
	}
	if c := getCookie(t, rec); c.Value != token {
		t.Fatalf("cookie value=%q, want the auth token %q", c.Value, token)
	}

	// The phone is now logged in.
	rec = l.get(t, "/", []*http.Cookie{authCookie(token)}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("logged-in request status=%d, want 200", rec.Code)
	}

	// The query form still works (manual entry, sign-in page form).
	rec = l.get(t, "/?pair="+code, nil, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("query exchange status=%d, want 302", rec.Code)
	}
}

func TestLoginFlowPairingPathFailures(t *testing.T) {
	const token = "tok-pathfail"
	const code = "paircode-pathfail"

	// Expired code via the path → explanatory page, no cookie. (With an
	// expired installed code every candidate reports "expired": the expiry
	// check runs before the match — the code is dead, period.)
	expired := newLoginFlowServer(t, token, code, time.Now().Add(-time.Minute))
	rec := expired.get(t, "/pair/"+code, nil, nil)
	assertPairingPage(t, rec, "expired")

	// Wrong code against a valid code → invalid page.
	l := newLoginFlowServer(t, token, code, time.Now().Add(PairingTTL))
	rec = l.get(t, "/pair/not-the-code", nil, nil)
	assertPairingPage(t, rec, "invalid")

	// No code in the path → enter-code page.
	rec = l.get(t, "/pair/", nil, nil)
	assertPairingPage(t, rec, "Enter the pairing code")
}

func TestLoginFlowPairingFailures(t *testing.T) {
	const token = "tok-fail"
	const code = "paircode-fail"
	now := time.Now()

	cases := []struct {
		name        string
		pairCode    string
		expiry      time.Time
		preConsume  int // successful consumes before the request
		requestCode string
		msgFragment string
	}{
		{"wrong code", code, now.Add(PairingTTL), 0, "not-the-code", "invalid"},
		{"expired code", code, now.Add(-time.Minute), 0, code, "expired"},
		{"code from earlier server start", "", now, 0, code, "expired"},
		{"exhausted code", code, now.Add(PairingTTL), maxPairingUses, code, "already been used"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLoginFlowServer(t, token, tc.pairCode, tc.expiry)
			for i := 0; i < tc.preConsume; i++ {
				if l.s.consumePairingCode(tc.pairCode) != pairingAccepted {
					t.Fatalf("pre-consume %d failed", i)
				}
			}
			rec := l.get(t, "/?pair="+tc.requestCode, nil, nil)
			assertPairingPage(t, rec, tc.msgFragment)
			// A rejected exchange must never set the cookie.
			if got := rec.Result().Cookies(); len(got) != 0 {
				t.Fatalf("rejected exchange set cookies: %v", got)
			}
		})
	}
}

// ---- Credential methods over the real endpoints ----

func TestLoginFlowAuthMethods(t *testing.T) {
	const token = "tok-methods"
	l := newLoginFlowServer(t, token, "", time.Time{})

	// Cookie.
	if rec := l.get(t, "/", []*http.Cookie{authCookie(token)}, nil); rec.Code != http.StatusOK {
		t.Fatalf("cookie: status=%d, want 200", rec.Code)
	}
	// X-Gogen-Token header.
	if rec := l.get(t, "/", nil, map[string]string{"X-Gogen-Token": token}); rec.Code != http.StatusOK {
		t.Fatalf("header: status=%d, want 200", rec.Code)
	}
	// Authorization: Bearer.
	if rec := l.get(t, "/", nil, map[string]string{"Authorization": "Bearer " + token}); rec.Code != http.StatusOK {
		t.Fatalf("bearer: status=%d, want 200", rec.Code)
	}
	// No credentials.
	if rec := l.get(t, "/", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("none: status=%d, want 401", rec.Code)
	}
}

func TestLoginFlowWebSocketAuth(t *testing.T) {
	const token = "tok-ws"
	l := newLoginFlowServer(t, token, "", time.Time{})

	// Authenticated upgrades succeed (cookie, header, bearer).
	for name, hdr := range map[string]map[string]string{
		"cookie": {"Cookie": authCookieName + "=" + token},
		"header": {"X-Gogen-Token": token},
		"bearer": {"Authorization": "Bearer " + token},
	} {
		conn, status := dialWSAuth(t, l, "/ws", hdr)
		if status != 0 {
			t.Fatalf("%s: dial status=%d, want success", name, status)
		}
		conn.Close()
	}

	// Editor socket is equally protected.
	if conn, status := dialWSAuth(t, l, "/ws/editor", map[string]string{"Cookie": authCookieName + "=" + token}); status != 0 {
		t.Fatalf("editor cookie: dial status=%d, want success", status)
	} else {
		conn.Close()
	}

	// No credentials → 401 at the upgrade.
	if _, status := dialWSAuth(t, l, "/ws", nil); status != http.StatusUnauthorized {
		t.Fatalf("no creds: dial status=%d, want 401", status)
	}
	// The token in the query string must NOT authenticate API/WS requests.
	if _, status := dialWSAuth(t, l, "/ws?token="+token, nil); status != http.StatusUnauthorized {
		t.Fatalf("query token: dial status=%d, want 401", status)
	}
}

// ---- Restart lifecycle: login persists, pairing code does not ----

func TestLoginFlowRestartLifecycle(t *testing.T) {
	const token = "tok-restart"
	codeBoot1 := "code-boot-1"
	codeBoot2 := "code-boot-2"

	// Boot 1: phone scans the fresh QR (path form) and gets a cookie.
	boot1 := newLoginFlowServer(t, token, codeBoot1, time.Now().Add(PairingTTL))
	rec := boot1.get(t, "/pair/"+codeBoot1, nil, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("boot1 scan status=%d, want 302", rec.Code)
	}

	// Restart: same token (persisted), fresh pairing code.
	boot2 := newLoginFlowServer(t, token, codeBoot2, time.Now().Add(PairingTTL))

	// The login persists: the pre-restart cookie still works.
	rec = boot2.get(t, "/", []*http.Cookie{authCookie(token)}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-restart cookie after restart: status=%d, want 200 (login must persist)", rec.Code)
	}

	// The old pairing code is dead (per-boot) → explanatory page.
	rec = boot2.get(t, "/pair/"+codeBoot1, nil, nil)
	assertPairingPage(t, rec, "invalid")

	// The new boot's code works.
	rec = boot2.get(t, "/pair/"+codeBoot2, nil, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("boot2 scan status=%d, want 302", rec.Code)
	}
}

// ---- Rate limiting on the bootstrap endpoints ----

func TestLoginFlowBootstrapRateLimit(t *testing.T) {
	const token = "tok-ratelimit"
	dir := t.TempDir()
	prov := llmtest.NewMockProvider()
	exec := agent.NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.DefaultSettings())
	a := agent.NewAgent(prov, exec, ctxMgr)
	s := newTestServer(t, a, &config.Config{WebAuthToken: token})
	s.bootstrapLimiter = newIPLimiter(1, 1) // burst 1: second attempt is throttled
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.HandleStatic(w, r)
	}))
	defer srv.Close()

	do := func() int {
		req := httptest.NewRequest("GET", srv.URL+"/?token=wrong", nil)
		rec := httptest.NewRecorder()
		srv.Config.Handler.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := do(); got != http.StatusUnauthorized {
		t.Fatalf("first attempt status=%d, want 401", got)
	}
	if got := do(); got != http.StatusTooManyRequests {
		t.Fatalf("second attempt status=%d, want 429", got)
	}
}

// ---- Sanity: token bootstrap via the token state file path ----

func TestLoginFlowNoTokenLoopbackMode(t *testing.T) {
	// The state file itself is main-package territory (web_token_test.go);
	// here we pin the contract the web server depends on: with no token
	// configured (loopback bind) the UI is open — not a broken 401 loop.
	// The GOGEN_WEB_TOKEN env fallback in NewServer must not leak into this
	// assertion, so it is cleared for the duration of the test.
	old, had := os.LookupEnv("GOGEN_WEB_TOKEN")
	if had {
		t.Setenv("GOGEN_WEB_TOKEN", "")
		defer os.Setenv("GOGEN_WEB_TOKEN", old)
	}
	l := newLoginFlowServer(t, "", "", time.Time{})
	rec := l.get(t, "/", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-token server: status=%d, want 200 (loopback mode has no auth)", rec.Code)
	}
}
