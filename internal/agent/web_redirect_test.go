package agent

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// setFetchCfg pins webCfg's mode/domains for a test and restores them after.
// fetchOn must be non-nil for the accessors to read the configured fields
// instead of falling back to the environment (see webCfgState.mode).
func setFetchCfg(t *testing.T, mode string, domains []string) {
	t.Helper()
	webCfg.mu.Lock()
	oldOn, oldMode, oldDomains := webCfg.fetchOn, webCfg.fetchMode, webCfg.fetchDomains
	on := true
	webCfg.fetchOn, webCfg.fetchMode, webCfg.fetchDomains = &on, mode, domains
	webCfg.mu.Unlock()
	t.Cleanup(func() {
		webCfg.mu.Lock()
		webCfg.fetchOn, webCfg.fetchMode, webCfg.fetchDomains = oldOn, oldMode, oldDomains
		webCfg.mu.Unlock()
	})
}

// TestCheckFetchRedirectDomainAllowlist: a redirect to a host outside
// GOGEN_WEB_ALLOWED_DOMAINS must be refused. The allowlist used to be applied
// to the first URL only, so a permitted host could bounce the request anywhere.
func TestCheckFetchRedirectDomainAllowlist(t *testing.T) {
	setFetchCfg(t, "all", []string{"allowed.example"})

	via := []*http.Request{{URL: mustURL(t, "https://allowed.example/page")}}
	if err := checkFetchRedirect(&http.Request{URL: mustURL(t, "https://allowed.example/next")}, via); err != nil {
		t.Fatalf("same-domain redirect refused: %v", err)
	}
	err := checkFetchRedirect(&http.Request{URL: mustURL(t, "https://evil.example/next")}, via)
	if err == nil || !strings.Contains(err.Error(), "GOGEN_WEB_ALLOWED_DOMAINS") {
		t.Fatalf("off-allowlist redirect error = %v, want a domain refusal", err)
	}
}

// TestCheckFetchRedirectHttpsOnly: with the default (https-only) mode, a
// redirect to plain http must be refused even when the host is allowed.
func TestCheckFetchRedirectHttpsOnly(t *testing.T) {
	setFetchCfg(t, "https", nil)

	via := []*http.Request{{URL: mustURL(t, "https://example.com/page")}}
	err := checkFetchRedirect(&http.Request{URL: mustURL(t, "http://example.com/page")}, via)
	if err == nil || !strings.Contains(err.Error(), "requires https") {
		t.Fatalf("http downgrade error = %v, want an https-only refusal", err)
	}
	// In "all" mode plain http stays reachable, exactly like the initial URL.
	setFetchCfg(t, "all", nil)
	if err := checkFetchRedirect(&http.Request{URL: mustURL(t, "http://example.com/page")}, via); err != nil {
		t.Fatalf("http redirect in mode=all refused: %v", err)
	}
}

// TestCheckFetchRedirectKeepsExistingGuards: the loop, hop-count and
// private-host checks must still run (they are evaluated before the policy).
func TestCheckFetchRedirectKeepsExistingGuards(t *testing.T) {
	setFetchCfg(t, "all", nil)

	if err := checkFetchRedirect(&http.Request{URL: mustURL(t, "https://a.example/")},
		make([]*http.Request, webFetchMaxRedirects)); err == nil {
		t.Fatal("hop limit not enforced")
	}
	loop := []*http.Request{{URL: mustURL(t, "https://a.example/x")}}
	if err := checkFetchRedirect(&http.Request{URL: mustURL(t, "https://a.example/x")}, loop); err == nil {
		t.Fatal("redirect loop not detected")
	}
	if err := checkFetchRedirect(&http.Request{URL: mustURL(t, "https://127.0.0.1/x")}, loop); err == nil {
		t.Fatal("private host redirect not blocked")
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
