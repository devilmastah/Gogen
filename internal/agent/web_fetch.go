package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"gogen/internal/buildinfo"
	"gogen/internal/onoff"
)

const (
	webFetchTimeout      = 15 * time.Second
	webFetchDefaultMax   = 256 * 1024 // 256 KB — enough for readability to reach the article on most pages
	webFetchHardMax      = 2 * 1024 * 1024
	webFetchMaxRedirects = 3
	// webFetchOutputLimit caps the tool-result string. It is kept equal to
	// webFetchHardMax so an explicit max_bytes request is never silently cut
	// short by a second, smaller cap: the body-read cap reports truncation
	// instead (see doFetch). Default fetches stay at webFetchDefaultMax,
	// which is matched to the default MaxToolResultBytes so a truncation
	// notice passes through to the model intact.
	// The output-limit branch below is still reachable: markdown conversion
	// can expand a body (escaping, link/code markup), so a full-size 2 MB
	// body can yield more than 2 MB of text.
	webFetchOutputLimit = webFetchHardMax
)

// fetchURLValidator is swappable in tests that exercise the fetch/download
// paths against a local httptest server (the real validator blocks private
// hosts, which is exactly what a local test server is).
var fetchURLValidator = validateFetchURL

// webCfg holds all runtime web fetch/search configuration behind a single mutex.
var webCfg webCfgState

// webCfgState holds all runtime web fetch/search configuration behind a single mutex.
type webCfgState struct {
	mu            sync.RWMutex
	fetchOn       *bool    // nil until configured
	searchOn      *bool    // nil until configured
	fetchMode     string   // "https" or "all"
	fetchDomains  []string // domain allowlist for fetch
	searchBackend string   // "brave" or ""
	searchAPIKey  string
}

// envOnce is a one-shot cache for an environment variable. The raw value is
// read on first access to avoid per-call syscalls; the parsed result is
// derived from that cached raw value on every call, so the per-variable
// parsing stays cheap and each variable caches independently.
type envOnce struct {
	once sync.Once
	val  string
}

// get returns the value of key, read from the environment on first use.
func (e *envOnce) get(key string) string {
	e.once.Do(func() { e.val = os.Getenv(key) })
	return e.val
}

var (
	fetchOnEnv, searchOnEnv, fetchModeEnv, allowedDomainsEnv envOnce
)

func envFetchOn() bool {
	return onoff.Enabled(fetchOnEnv.get("GOGEN_WEB_FETCH"))
}

func envSearchOn() bool {
	return onoff.Enabled(searchOnEnv.get("GOGEN_WEB_SEARCH"))
}

// normalizeFetchMode validates a web fetch mode ("https" or "all"). Unknown
// values are clamped to "https" so a typo cannot silently permit plaintext
// HTTP — "all" is the only value that allows http.
func normalizeFetchMode(mode string) string {
	mode = strings.TrimSpace(strings.ToLower(mode))
	switch mode {
	case "", "https":
		return "https"
	case "all":
		return "all"
	default:
		fmt.Fprintf(os.Stderr, "Warning: unknown web_fetch_mode %q; using \"https\" (set \"all\" to allow http)\n", mode)
		return "https"
	}
}

// envFetchMode returns the web fetch mode from env. Cached after first read.
func envFetchMode() string {
	return normalizeFetchMode(fetchModeEnv.get("GOGEN_WEB_FETCH_MODE"))
}

func (c *webCfgState) isFetchOn() bool {
	c.mu.RLock()
	if c.fetchOn != nil {
		v := *c.fetchOn
		c.mu.RUnlock()
		return v
	}
	c.mu.RUnlock()
	return envFetchOn()
}

func (c *webCfgState) isSearchOn() bool {
	c.mu.RLock()
	if c.searchOn != nil {
		v := *c.searchOn
		c.mu.RUnlock()
		return v
	}
	c.mu.RUnlock()
	return envSearchOn()
}

// mode and allowedDomains fall back to env only when fetch itself is
// unconfigured. fetchOn is the "fetch configured" signal: ConfigureWebFetch
// sets fetchOn, fetchMode and fetchDomains atomically, so fetchOn != nil is
// exactly the per-field "configured" state for these two fetch-specific
// settings. Gating on fetchOn (not searchOn) keeps each setting's fallback
// independent, matching isFetchOn/isSearchOn.
func (c *webCfgState) mode() string {
	c.mu.RLock()
	if c.fetchOn != nil {
		m := c.fetchMode
		c.mu.RUnlock()
		return m
	}
	c.mu.RUnlock()
	return envFetchMode()
}

func (c *webCfgState) allowedDomains() []string {
	c.mu.RLock()
	if c.fetchOn != nil {
		d := c.fetchDomains
		c.mu.RUnlock()
		return d
	}
	c.mu.RUnlock()
	return envAllowedDomains()
}

// parseDomainList splits a comma-separated domain allowlist, trimming,
// lowercasing, and dropping empty entries. Returns nil for empty input.
func parseDomainList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(strings.ToLower(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// envAllowedDomains parses and caches GOGEN_WEB_ALLOWED_DOMAINS from env.
func envAllowedDomains() []string {
	return parseDomainList(allowedDomainsEnv.get("GOGEN_WEB_ALLOWED_DOMAINS"))
}

func (c *webCfgState) searchBE() string {
	c.mu.RLock()
	b := c.searchBackend
	c.mu.RUnlock()
	return b
}

func (c *webCfgState) searchKey() string {
	c.mu.RLock()
	k := c.searchAPIKey
	c.mu.RUnlock()
	return k
}

// ConfigureWebFetch applies runtime web fetch settings from merged config.
func ConfigureWebFetch(enabled bool, mode string, allowedDomains string) {
	webCfg.mu.Lock()
	defer webCfg.mu.Unlock()
	webCfg.fetchOn = &enabled
	webCfg.fetchMode = normalizeFetchMode(mode)
	webCfg.fetchDomains = parseDomainList(allowedDomains)
}

// ConfigureWebSearchEnabled sets whether web_search is allowed (independent of web_fetch).
func ConfigureWebSearchEnabled(enabled bool) {
	webCfg.mu.Lock()
	defer webCfg.mu.Unlock()
	webCfg.searchOn = &enabled
}

// ConfigureWebSearch applies runtime web search settings from merged config.
func ConfigureWebSearch(backend, apiKey string) {
	webCfg.mu.Lock()
	defer webCfg.mu.Unlock()
	webCfg.searchBackend = strings.ToLower(strings.TrimSpace(backend))
	webCfg.searchAPIKey = strings.TrimSpace(apiKey)
}

// sharedFetchClient is reused across requests for connection pooling.
// CheckRedirect enforces max redirects, loop detection, and private-host blocking.
var sharedFetchClient = newSharedFetchClient()

// newSharedFetchClient builds the shared transport. A non-zero DialContext
// conservatively disables HTTP/2, so ForceAttemptHTTP2 is set explicitly: it is
// the supported replacement for the deprecated http2.ConfigureTransport, and
// unlike it has no failure mode to guard (the h2 upgrade is configured lazily
// by net/http and falls back to HTTP/1.1 on its own).
func newSharedFetchClient() *http.Client {
	tr := &http.Transport{
		MaxIdleConns:      10,
		IdleConnTimeout:   90 * time.Second,
		DialContext:       dialContextPublicOnly,
		ForceAttemptHTTP2: true,
	}
	return &http.Client{
		Transport:     tr,
		CheckRedirect: checkFetchRedirect,
	}
}

// checkFetchRedirect is the redirect policy for every shared fetch client
// (web_fetch and download_file both use newSharedFetchClient): max hops, loop
// detection, private-host blocking, and — for EVERY hop, not just the first
// URL — the same scheme / HTTPS-only / domain-allowlist policy. Applying the
// policy only to the initial URL let a permitted https host redirect to plain
// http or to a domain outside GOGEN_WEB_ALLOWED_DOMAINS.
func checkFetchRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= webFetchMaxRedirects {
		return fmt.Errorf("too many redirects")
	}
	nextURL := req.URL.String()
	for _, prev := range via {
		if prev.URL.String() == nextURL {
			return fmt.Errorf("redirect loop detected: %s", nextURL)
		}
	}
	if isPrivateHost(req.URL.Hostname()) {
		return fmt.Errorf("redirect to private host blocked: %s", req.URL.Host)
	}
	if err := validateFetchPolicy(req.URL); err != nil {
		return fmt.Errorf("redirect to %s refused: %w", req.URL.Redacted(), err)
	}
	return nil
}

// fetchRequest describes a single HTTP fetch operation.
type fetchRequest struct {
	URL      string // target URL
	Method   string // "GET" (default) or "POST"
	UA       string // User-Agent (default buildinfo.UserAgent())
	Body     string // form-encoded body for POST requests
	MaxBytes int    // max response body bytes to read
}

// doFetch performs a single HTTP request with SSRF protection and redirect
// enforcement. It uses the shared client (dialContextPublicOnly + CheckRedirect)
// so private/internal hosts are blocked at both the dial and redirect level.
// It returns the response body, its Content-Type header, the final URL after
// redirects, and whether the body was cut short by req.MaxBytes.
func doFetch(ctx context.Context, req fetchRequest) ([]byte, string, string, bool, error) {
	resp, err := doFetchRequest(ctx, req)
	if err != nil {
		return nil, "", req.URL, false, err
	}
	defer resp.Body.Close()

	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = webFetchDefaultMax
	}
	// Read one byte past the cap so truncation is detectable instead of
	// silently looking like the complete document (which previously made
	// mid-file cuts indistinguishable from full files).
	limited := io.LimitReader(resp.Body, int64(maxBytes)+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", resp.Request.URL.String(), false, fmt.Errorf("read body: %w", err)
	}
	truncated := len(body) > maxBytes
	if truncated {
		body = body[:maxBytes]
	}
	return body, resp.Header.Get("Content-Type"), resp.Request.URL.String(), truncated, nil
}

// doFetchStream is doFetch's streaming counterpart: it performs the same
// SSRF-checked request but copies the response body into w instead of
// buffering it on the heap, so large downloads never materialize in memory.
// It returns the body's Content-Type, the final URL after redirects, the
// number of bytes copied, and whether the body exceeded req.MaxBytes. At most
// maxBytes+1 bytes are copied (the +1 is the same truncation probe doFetch
// uses); when truncated is true the caller must discard whatever it sent to w.
func doFetchStream(ctx context.Context, req fetchRequest, w io.Writer) (string, string, int64, bool, error) {
	resp, err := doFetchRequest(ctx, req)
	if err != nil {
		return "", req.URL, 0, false, err
	}
	defer resp.Body.Close()

	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = webFetchDefaultMax
	}
	limited := io.LimitReader(resp.Body, int64(maxBytes)+1)
	n, err := io.Copy(w, limited)
	if err != nil {
		return "", resp.Request.URL.String(), n, false, fmt.Errorf("read body: %w", err)
	}
	return resp.Header.Get("Content-Type"), resp.Request.URL.String(), n, n > int64(maxBytes), nil
}

// doFetchRequest builds and executes the HTTP request described by req with
// the shared SSRF-guarded client (dialContextPublicOnly + CheckRedirect), so
// private/internal hosts are blocked at both the dial and redirect level. The
// caller owns the returned response body and must close it. 4xx/5xx statuses
// are reported as errors so callers never treat an error page as content.
func doFetchRequest(ctx context.Context, req fetchRequest) (*http.Response, error) {
	if req.Method == "" {
		req.Method = http.MethodGet
	}
	ua := req.UA
	if ua == "" {
		ua = buildinfo.UserAgent()
	}

	var httpReq *http.Request
	var err error

	if req.Method == http.MethodPost {
		httpReq, err = http.NewRequestWithContext(ctx, http.MethodPost, req.URL,
			strings.NewReader(req.Body))
	} else {
		httpReq, err = http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}

	httpReq.Header.Set("User-Agent", ua)
	httpReq.Header.Set("Accept", "text/html,text/plain,application/xhtml+xml")
	if req.Method == http.MethodPost {
		httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := sharedFetchClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return resp, nil
}

// isInternalIP reports whether ip is a private/internal address that the
// fetch/dial layers must never connect to. dialContextPublicOnly uses it to
// skip internal addresses and dial only verified public ones; a host is
// blocked entirely when ALL of its resolved addresses are internal.
func isInternalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast()
}

func dialContextPublicOnly(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no addresses resolved for %s", host)
	}
	// Filter to the VERIFIED public addresses. A dual-stack / split-horizon
	// host can legitimately resolve to a mix of public and private (or ULA)
	// addresses — rejecting the whole host on the first private hit broke
	// such sites even though a public address exists. The security property
	// is preserved: only addresses checked here (and found public) are ever
	// dialed, so a DNS-rebinding resolver cannot smuggle a private address
	// past the check (we dial the checked IPs, not the hostname).
	var publicIPs []net.IPAddr
	for _, ipAddr := range ips {
		ip := ipAddr.IP
		if isInternalIP(ip) {
			continue
		}
		publicIPs = append(publicIPs, ipAddr)
	}
	if len(publicIPs) == 0 {
		return nil, fmt.Errorf("requests to private/internal hosts are blocked: %s resolves only to private addresses", host)
	}
	// Dial the VERIFIED public IPs directly instead of the hostname (see the
	// comment above on DNS rebinding). TLS SNI and certificate validation are
	// driven by the URL host (http.Transport), not by the dialed address, so
	// dialing an IP is transparent to HTTPS; iterating every verified IP
	// preserves multi-A/AAAA failover that dialing the hostname would
	// otherwise get from the OS resolver.
	d := net.Dialer{Timeout: webFetchTimeout}
	var dialErrs []error
	for _, ipAddr := range publicIPs {
		conn, err := d.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		dialErrs = append(dialErrs, err)
	}
	return nil, errors.Join(dialErrs...)
}

// isPrivateHost reports whether host is a private/internal host that must not
// be fetched. It is a purely syntactic check: localhost names, and IP literals
// (v4 and v6) that are loopback/private/link-local/unspecified/multicast.
//
// Hostnames are not classified from their text. A name that merely starts with
// a private-IP prefix — "10.example.com", "127.example.com",
// "192.168.example.com", "fe80.example.com" — is an ordinary public hostname.
// Every hostname is enforced at DIAL time instead by dialContextPublicOnly,
// which resolves the name with the caller's context, discards the internal
// addresses, refuses the request when no public address remains, and dials only
// the verified public IPs. Dialing the checked IPs rather than the name is what
// closes DNS rebinding, so matching the host's text here added false-positive
// risk with no security value.
//
// For the same reason this function deliberately does NOT resolve hostnames:
// resolution happens once, with the caller's context, in dialContextPublicOnly.
// A second lookup here would be a blocking, context-less net.LookupIP call that
// can outlive the tool call's own deadline — and it previously mangled IPv6
// literals (strings.LastIndex(host, ":") then resolving the bracket), so every
// IPv6-literal URL was wrongly rejected as private/internal.
func isPrivateHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" || host == "localhost" || host == "localhost.localdomain" {
		return true
	}
	// Strip an optional port. URL.Host carries "host:port" (bracketing an IPv6
	// literal), while URL.Hostname() does not. net.SplitHostPort handles
	// "[::1]:443"; a bare literal ("::1") or a bracketed literal with no port
	// ("[::1]") errors and is handled by the bracket trim below.
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if host == "" {
		return true
	}
	// A literal IP (v4 or v6) resolves to itself, so classify it directly
	// instead of sending a bracketed/mangled literal to the resolver.
	if ip := net.ParseIP(host); ip != nil {
		return isInternalIP(ip)
	}
	// A hostname: not known to be private from syntax alone. dialContextPublicOnly
	// resolves it with the caller's context and blocks it if every resolved
	// address is internal.
	return false
}

// WebFetchOptions configures a web_fetch call. Selector and Query are the
// agent-driven extraction primitives for unseen pages: instead of relying
// on pre-tuned boilerplate removal, the caller names exactly what it wants
// (see extractBySelector and searchExtractedText).
type WebFetchOptions struct {
	// MaxBytes caps the response body read (default webFetchDefaultMax,
	// capped at webFetchHardMax).
	MaxBytes int
	// Selector is an optional CSS selector: when set, only matching
	// elements are converted to markdown.
	Selector string
	// Query is an optional case-insensitive text search over the extracted
	// content: when set, matches with surrounding context lines are
	// returned instead of the full text. Composes with Selector.
	Query string
	// Context is the number of context lines around each Query match
	// (default webFetchSearchDefaultContext, capped at
	// webFetchSearchMaxContextLines).
	Context int
}

func (e *Executor) WebFetch(ctx context.Context, rawURL string, opts WebFetchOptions) (string, error) {
	if !webCfg.isFetchOn() {
		return "", fmt.Errorf("web_fetch is disabled (set GOGEN_WEB_FETCH=on to re-enable)")
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", fmt.Errorf("url is required")
	}

	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = webFetchDefaultMax
	}
	if maxBytes > webFetchHardMax {
		maxBytes = webFetchHardMax
	}

	parsed, err := fetchURLValidator(rawURL)
	if err != nil {
		return "", err
	}

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, webFetchTimeout)
	defer cancel()

	body, contentType, finalURL, truncated, err := doFetch(ctx, fetchRequest{
		URL:      parsed.String(),
		MaxBytes: maxBytes,
	})
	if err != nil {
		return "", err
	}

	text, readable, readabilityFailed, err := extractWebContent(body, contentType, finalURL, opts)
	if err != nil {
		return "", err
	}

	if opts.Query != "" {
		result := searchExtractedText(text, opts.Query, opts.Context)
		if truncated {
			result = fmt.Sprintf("Note: body exceeded max_bytes and was cut before extraction; raise max_bytes (up to %d) if the match region is missing.\n\n", webFetchHardMax) + result
		}
		return result, nil
	}

	return buildFetchResult(text, truncated, maxBytes, opts.Selector != "" || readable, readabilityFailed, finalURL, parsed.String()), nil
}

// extractWebContent converts a fetched body to text: CSS-selector extraction
// when requested, readability main-content extraction for HTML, or full-page
// conversion. Returns the text plus flags describing which path ran.
func extractWebContent(body []byte, contentType, finalURL string, opts WebFetchOptions) (text string, readable, readabilityFailed bool, err error) {
	if opts.Selector != "" {
		text, err = extractBySelector(body, opts.Selector, finalURL)
		if err != nil {
			return "", false, false, err
		}
		return text, false, false, nil
	}
	if classifyResponse(contentType, finalURL, body) == kindHTML {
		// HTML: prefer readability main-content extraction. When no single
		// article is found (list pages, docs indexes, tables, ...) fall back
		// to full-page conversion.
		if md, ok := extractReadable(body, finalURL); ok {
			return md, true, false, nil
		}
		return extractResponseText(contentType, finalURL, body), false, true, nil
	}
	return extractResponseText(contentType, finalURL, body), false, false, nil
}

// buildFetchResult assembles the final web_fetch output, marking truncated
// bodies and over-limit text explicitly (a silent mid-file cut previously
// looked like the complete document).
func buildFetchResult(text string, truncated bool, maxBytes int, extraction, readabilityFailed bool, finalURL, originalURL string) string {
	var b strings.Builder
	if finalURL != originalURL {
		fmt.Fprintf(&b, "Final URL (after redirects): %s\n\n", finalURL)
	}
	if readabilityFailed {
		fmt.Fprintf(&b, "Note: no single main article found; returning the full page instead.\n\n")
	}
	if extraction && truncated {
		fmt.Fprintf(&b, "Note: body exceeded max_bytes and was cut before extraction; raise max_bytes (up to %d) if the result seems incomplete.\n\n", webFetchHardMax)
	}
	switch {
	case truncated:
		// The body was cut by max_bytes. Report it explicitly — a silent
		// mid-file cut previously looked like the complete document, and
		// the marker keeps this result intact under MaxToolResultBytes.
		if !extraction {
			fmt.Fprintf(&b, "Content (first %d of more than %d bytes):\n", len(text), maxBytes)
			b.WriteString(text)
			fmt.Fprintf(&b, "\n\n… truncated (body exceeds %d bytes; pass max_bytes up to %d to fetch more)", maxBytes, webFetchHardMax)
		} else {
			// Selector/readability mode: extraction already ran on the bytes
			// we read; just say the body was cut.
			b.WriteString(text)
			fmt.Fprintf(&b, "\n\n… body exceeded %d bytes and was cut before extraction (pass max_bytes up to %d to fetch more)", maxBytes, webFetchHardMax)
		}
	case len(text) > webFetchOutputLimit:
		fmt.Fprintf(&b, "Content (first %d of %d bytes):\n", webFetchOutputLimit, len(text))
		b.WriteString(text[:webFetchOutputLimit])
		fmt.Fprintf(&b, "\n\n… truncated (%d bytes total)", len(text))
	case text == "":
		b.WriteString("(empty body)")
	default:
		b.WriteString(text)
	}
	return b.String()
}

func validateFetchURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	// Block private/internal hosts on the initial URL (redirects are also checked).
	// Hostname() strips the port and IPv6 brackets; hostname resolution is
	// enforced at dial time by dialContextPublicOnly.
	if isPrivateHost(u.Hostname()) {
		return nil, fmt.Errorf("requests to private/internal hosts are blocked: %s", u.Hostname())
	}
	if err := validateFetchPolicy(u); err != nil {
		return nil, err
	}
	return u, nil
}

// validateFetchPolicy applies the scheme, HTTPS-only and domain-allowlist rules
// to an already-parsed URL. It is the SPOT policy half of validateFetchURL,
// split out so CheckRedirect can re-apply it to every hop — a redirect is a
// second request the user's configuration was never checked against.
func validateFetchPolicy(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("redirect without a URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("web_fetch only supports http/https URLs (got %q)", u.Scheme)
	}
	// Enforce HTTPS-only unless "all" is explicitly configured. Fails closed:
	// any value other than "all" (normalizeFetchMode clamps unknown ones) blocks http.
	if webCfg.mode() != "all" && u.Scheme != "https" {
		return fmt.Errorf("web_fetch requires https (got %s). Set GOGEN_WEB_FETCH_MODE=all for http", u.Scheme)
	}
	if allowedDomains := webCfg.allowedDomains(); len(allowedDomains) > 0 {
		host := strings.ToLower(u.Hostname())
		for _, d := range allowedDomains {
			if host == d || strings.HasSuffix(host, "."+d) {
				return nil
			}
		}
		return fmt.Errorf("domain %q is not in GOGEN_WEB_ALLOWED_DOMAINS", u.Hostname())
	}
	return nil
}
