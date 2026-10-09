package llm

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// streamReadIdleTimeout is the per-read deadline for SSE response bodies.
// llama.cpp often stops sending without closing the connection or sending [DONE];
// this bounds how long we block waiting for the next byte. The same window
// also covers the wait for the FIRST byte: long prompt processing (llama.cpp
// re-processes the whole prompt each round) and queued requests on a busy
// backend send no bytes until the first token, so the default must tolerate
// multi-minute (occasionally tens-of-minutes) prefills. Set
// GOGEN_STREAM_IDLE_TIMEOUT=0 to disable (wait indefinitely).
func streamReadIdleTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("GOGEN_STREAM_IDLE_TIMEOUT"))
	if raw == "" {
		return 30 * time.Minute
	}
	if raw == "0" || strings.EqualFold(raw, "off") || strings.EqualFold(raw, "false") {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 30 * time.Minute
	}
	return d
}

// streamStallAfter is how long the SSE read loop may see no chunk before
// OnStreamStall fires so hosts can surface a "still waiting" state — a long
// prefill (llama.cpp re-processes the whole prompt each round) or a
// server-side stall is otherwise indistinguishable from a dead UI. The
// callback is informational: it never interrupts the stream (the per-read
// idle deadline in idleTimeoutConn remains the hard bound). Set
// GOGEN_STREAM_STALL=0/off to disable the signal.
func streamStallAfter() time.Duration {
	raw := strings.TrimSpace(os.Getenv("GOGEN_STREAM_STALL"))
	if raw == "" {
		return 10 * time.Second
	}
	if raw == "0" || strings.EqualFold(raw, "off") || strings.EqualFold(raw, "false") {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 10 * time.Second
	}
	return d
}

// streamRetryBackoff is the delay inserted before a recovery re-request:
// stage 0 is the muted streaming retry, stage 1 the non-streaming
// fallback. The ladder used to fire each re-request within milliseconds
// of the failed attempt, so a failure condition that persists for even a
// short window (the router re-dialing a dead upstream, a LAN blip)
// swallowed the retry AND the fallback, landing every such failure on the
// slowest path. The delay doubles per stage (base, 2×base). Set
// GOGEN_STREAM_RETRY_BACKOFF=0 to disable (immediate, as before).
func streamRetryBackoff(stage int) time.Duration {
	raw := strings.TrimSpace(os.Getenv("GOGEN_STREAM_RETRY_BACKOFF"))
	var base time.Duration
	switch {
	case raw == "":
		base = time.Second
	case raw == "0" || strings.EqualFold(raw, "off") || strings.EqualFold(raw, "false"):
		return 0
	default:
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			base = time.Second
		} else {
			base = d
		}
	}
	if stage < 0 {
		stage = 0
	}
	return base * time.Duration(1<<uint(stage))
}

// waitStreamBackoff sleeps for the recovery backoff, returning early with
// the context error when the turn is cancelled mid-wait so a stop request
// is never held up by the delay.
func waitStreamBackoff(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type idleTimeoutConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleTimeoutConn) Read(b []byte) (int, error) {
	if c.timeout > 0 {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	}
	return c.Conn.Read(b)
}

// newSSEHTTPClient returns an HTTP client tuned for Server-Sent Events.
// http.DefaultClient advertises Accept-Encoding: gzip, which makes many
// backends (including llama.cpp) compress the stream; the gzip reader then
// delivers tokens in bursts instead of incrementally.
func newSSEHTTPClient() *http.Client {
	idle := streamReadIdleTimeout()
	// Keep dial short: catalog lookups use modelsCatalogTimeout (~8s), and a
	// 30s TCP timeout was a common "startup felt frozen" failure mode when the
	// provider host was unreachable (blackhole / wrong LAN IP). Caller contexts
	// still win when they are shorter.
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	// Clone DefaultTransport so HTTPS_PROXY/HTTP_PROXY (ProxyFromEnvironment)
	// and its TLS handshake timeout still apply: a bare &http.Transport{} has
	// no Proxy function (so the environment proxy is ignored, and a probe that
	// succeeds through the proxy is followed by chat requests that cannot) and
	// leaves TLSHandshakeTimeout at 0, bounding a stalled handshake only by the
	// SSE idle read deadline. Only compression and dialing are overridden.
	//
	// ForceAttemptHTTP2 comes along with the clone (DefaultTransport sets it),
	// so a stream may now negotiate h2 where the bare transport stayed on
	// HTTP/1.1 — consistent with propsHTTPClient, which already cloned. Set
	// tr.ForceAttemptHTTP2 = false here to pin HTTP/1.1 for an endpoint whose
	// SSE behaves badly over h2.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableCompression = true
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if idle > 0 {
			return &idleTimeoutConn{Conn: conn, timeout: idle}, nil
		}
		return conn, nil
	}
	return &http.Client{
		// sseFilterTransport drops the SSE keep-alive frames that openai-go's
		// stream decoder cannot digest (unexpected end of JSON input — see
		// sse_filter.go for the per-version gap analysis).
		// apiErrorBodyTransport (outermost) normalizes the >=400 JSON error
		// payloads the SDK's apierror parser chokes on or loses (see
		// api_error_body.go).
		Transport: &apiErrorBodyTransport{base: &sseFilterTransport{base: tr}},
	}
}

// newCatalogHTTPClient is for /v1/models and similar non-stream calls.
// A hard client Timeout prevents startup/ListModels from sitting on the SSE
// idle read deadline (default 30m) when a provider stalls after headers.
// The transport normalizes a JSON body served under the wrong Content-Type
// (see jsonSniffTransport), so a server that answers /models with
// "text/plain; charset=utf-8" still yields a model list instead of
// openai-go's opaque content-type error.
//
// Unlike the SSE client, compression stays ENABLED: some catalogs gzip the
// /models body unconditionally, and the standard library only decodes a
// gzipped response when the transport itself advertised Accept-Encoding
// (which DisableCompression suppresses) — otherwise the body stays raw gzip
// and fails to parse. Catalog responses never stream, so there is no
// token-batching cost here.
func newCatalogHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	// Clone DefaultTransport (as propsHTTPClient does) so HTTPS_PROXY and
	// TLSHandshakeTimeout apply; a bare &http.Transport{} bypasses the
	// environment proxy and leaves the handshake unbounded.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = dialer.DialContext
	return &http.Client{
		Timeout: modelsCatalogTimeout,
		// jsonSniffTransport normalizes a JSON body served under the wrong
		// media type; apiErrorBodyTransport (outermost) normalizes the
		// non-standard error payloads on top of that (see api_error_body.go).
		Transport: &apiErrorBodyTransport{base: &jsonSniffTransport{base: tr}},
	}
}
