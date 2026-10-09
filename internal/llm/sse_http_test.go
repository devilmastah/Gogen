package llm

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestNewSSEHTTPClientDisablesCompression(t *testing.T) {
	t.Parallel()
	c := newSSEHTTPClient()
	tr, ok := baseHTTPTransport(c)
	if !ok {
		t.Fatalf("Transport type = %T, want *http.Transport under the SSE filter transport", c.Transport)
	}
	if !tr.DisableCompression {
		t.Fatal("DisableCompression = false, want true")
	}
}

// TestSSEHTTPClientInheritsDefaultTransport pins that the SSE client clones
// http.DefaultTransport instead of building a bare &http.Transport{} that
// silently ignores HTTPS_PROXY and leaves TLSHandshakeTimeout at 0.
func TestSSEHTTPClientInheritsDefaultTransport(t *testing.T) {
	t.Parallel()
	tr, ok := baseHTTPTransport(newSSEHTTPClient())
	if !ok {
		t.Fatal("expected *http.Transport under the SSE filter transport")
	}
	assertDefaultTransportInherited(t, tr)
}

// TestCatalogHTTPClientInheritsDefaultTransport pins the same proxy/TLS
// inheritance for the non-streaming catalog client.
func TestCatalogHTTPClientInheritsDefaultTransport(t *testing.T) {
	t.Parallel()
	tr, ok := catalogBaseHTTPTransport(newCatalogHTTPClient())
	if !ok {
		t.Fatalf("Transport type = %T, want *jsonSniffTransport around *http.Transport",
			newCatalogHTTPClient().Transport)
	}
	assertDefaultTransportInherited(t, tr)
}

// catalogBaseHTTPTransport unwraps the catalog client's transport stack (the
// API-error normalizer around the content-type sniffer) to the concrete
// *http.Transport underneath.
func catalogBaseHTTPTransport(c *http.Client) (*http.Transport, bool) {
	eb, ok := c.Transport.(*apiErrorBodyTransport)
	if !ok {
		return nil, false
	}
	st, ok := eb.base.(*jsonSniffTransport)
	if !ok {
		return nil, false
	}
	tr, ok := st.base.(*http.Transport)
	return tr, ok
}

// assertDefaultTransportInherited checks the proxy/TLS knobs a bare
// &http.Transport{} would leave unset. Compression is asserted per client:
// the SSE client disables it, the catalog client must not (see
// TestCatalogHTTPClientKeepsTransparentGzip).
func assertDefaultTransportInherited(t *testing.T, tr *http.Transport) {
	t.Helper()
	if tr.Proxy == nil {
		t.Error("Proxy = nil, want ProxyFromEnvironment (HTTPS_PROXY/HTTP_PROXY)")
	}
	def := http.DefaultTransport.(*http.Transport)
	if tr.TLSHandshakeTimeout != def.TLSHandshakeTimeout {
		t.Errorf("TLSHandshakeTimeout = %v, want %v", tr.TLSHandshakeTimeout, def.TLSHandshakeTimeout)
	}
}

// TestSSEHTTPClientKeepsCompressionDisabled pins that the SSE client still
// refuses compressed streams (gzip would make token delivery bursty).
func TestSSEHTTPClientKeepsCompressionDisabled(t *testing.T) {
	t.Parallel()
	tr, ok := baseHTTPTransport(newSSEHTTPClient())
	if !ok {
		t.Fatal("expected *http.Transport under the SSE filter transport")
	}
	if !tr.DisableCompression {
		t.Error("DisableCompression = false, want true")
	}
}

// TestCatalogHTTPClientKeepsTransparentGzip pins that the catalog client
// leaves compression ENABLED, unlike the SSE client: Go then advertises
// Accept-Encoding and transparently decompresses, which is the only way to
// read a catalog from a server that gzips the /models body unconditionally
// (ai.h-bomb.nl does, even when Accept-Encoding is absent) — the
// DisableCompression=true the catalog client used to inherit from the SSE
// setup left the body as raw gzip, so the fetch failed regardless of the
// Content-Type.
func TestCatalogHTTPClientKeepsTransparentGzip(t *testing.T) {
	t.Parallel()
	tr, ok := catalogBaseHTTPTransport(newCatalogHTTPClient())
	if !ok {
		t.Fatal("expected *http.Transport under the catalog sniffer")
	}
	if tr.DisableCompression {
		t.Error("DisableCompression = true, want false: the catalog client must decode a gzipped /models body")
	}
}

// TestStreamRetryBackoff pins the recovery-ladder delay schedule: the
// default base is 1s doubling per stage, the env knob overrides it, and
// 0/off disables the wait entirely.
func TestStreamRetryBackoff(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		stage int
		want  time.Duration
	}{
		{name: "default stage 0", want: time.Second},
		{name: "default stage 1", stage: 1, want: 2 * time.Second},
		{name: "custom base", env: "500ms", want: 500 * time.Millisecond},
		{name: "custom base stage 1", env: "500ms", stage: 1, want: time.Second},
		{name: "zero disables", env: "0", want: 0},
		{name: "off disables", env: "off", want: 0},
		{name: "invalid falls back to default", env: "banana", want: time.Second},
		{name: "negative falls back to default", env: "-1s", want: time.Second},
	}
	// t.Setenv precludes t.Parallel.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GOGEN_STREAM_RETRY_BACKOFF", tt.env)
			if got := streamRetryBackoff(tt.stage); got != tt.want {
				t.Fatalf("streamRetryBackoff(%d) = %v, want %v", tt.stage, got, tt.want)
			}
		})
	}
}

// TestWaitStreamBackoffCancelled pins that a turn cancelled mid-backoff
// returns the context error immediately instead of sleeping out the delay.
func TestWaitStreamBackoffCancelled(t *testing.T) {
	t.Parallel()
	if err := waitStreamBackoff(context.Background(), 0); err != nil {
		t.Fatalf("zero delay must not error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := waitStreamBackoff(ctx, time.Minute); err == nil {
		t.Fatal("cancelled context must return an error")
	} else if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancelled wait took %v, want immediate", elapsed)
	}
}
