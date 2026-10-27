package llm

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
)

// SSE keep-alive frames break openai-go's stream decoder. Its
// eventStreamDecoder dispatches an event on every blank line and then
// json.Unmarshals the accumulated data unconditionally: a keep-alive —
// exactly what gateways, tunnels, and inference routers emit while a model
// is queued or prefilling — dispatches an event whose payload is empty or
// whitespace-only, and json.Unmarshal on such input fails with "unexpected
// end of JSON input". The failure surfaces before any output (keep-alives
// live in the silent pre-first-token window), so it eats the whole recovery
// ladder: the streaming retry dies the same way, and the turn lands on the
// slow non-streaming fallback behind a scary "Retrying stream (…)" label.
//
// Verified against the SDK releases (Stream-layer behavior on the six
// common frame styles):
//
//	                      v1.12.0   v2.7.1    v3.59.0
//	: comment              broken    broken    ok
//	event: ping (no data)  broken    broken    ok
//	data: (empty value)    broken    broken    broken
//	event+empty data       broken    broken    broken
//
// v3 skips events with no data lines at all, but a data line with an empty
// or whitespace-only value still accumulates "\n" and is dispatched. The
// SSE spec has always allowed all of these frames — comments are explicitly
// "must be ignored" territory for a spec-compliant client — so
// sseFilterTransport filters them out of text/event-stream response bodies
// at the transport layer, independent of the SDK version: the same
// endpoint-agnostic choke point as the idle-read deadline and the gzip
// disable in newSSEHTTPClient.

// sseFilterMaxLine bounds one SSE line inside the filter. Matched to the
// openai-go decoder's own scanner cap so the filter never rejects a line the
// SDK would have accepted.
const sseFilterMaxLine = bufio.MaxScanTokenSize << 9

// sseFilterTransport wraps a RoundTripper and, for text/event-stream
// responses only, interposes the empty-event filter between the body and the
// SDK. Everything else — including the JSON error payloads the SDK parses for
// apierror — passes through untouched; those are normalized, when needed, by
// the apiErrorBodyTransport wrapped around this one (api_error_body.go).
type sseFilterTransport struct {
	base http.RoundTripper
}

func (t *sseFilterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	res, err := base.RoundTrip(req)
	if err != nil || res == nil || res.Body == nil {
		return res, err
	}
	if !isSSEContentType(res.Header.Get("Content-Type")) {
		return res, nil
	}
	res.Body = newSSEEventFilterBody(res.Body)
	return res, nil
}

// isSSEContentType reports whether a Content-Type header names an SSE body.
// Servers attach charset parameters freely ("text/event-stream;
// charset=utf-8"), so the match is prefix-based.
func isSSEContentType(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream")
}

// newSSEEventFilterBody wraps an SSE response body so the SDK's decoder only
// ever sees events that carry a data payload. The reader side of a pipe is
// returned; a goroutine filters the real body into it, so closing the
// returned reader (the SDK's stream.Close) unblocks and tears down the
// filter, which then closes the underlying body.
func newSSEEventFilterBody(rc io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		err := filterSSEEvents(rc, pw)
		_ = rc.Close()
		_ = pw.CloseWithError(err) // nil err = clean EOF
	}()
	return &sseEventFilterBody{pr: pr, rc: rc}
}

// sseEventFilterBody is the read half handed to the SDK.
type sseEventFilterBody struct {
	pr *io.PipeReader
	rc io.ReadCloser
}

func (b *sseEventFilterBody) Read(p []byte) (int, error) { return b.pr.Read(p) }

// Close tears the filter down. It closes the pipe reader AND the real
// response body: closing only the pipe leaves the filter goroutine blocked in
// Scan() on the network read, so the connection (and the goroutine) survives
// until the next byte or the idle read deadline — 30 minutes by default. Every
// early close takes that path: the reasoning-stop grace timer and the
// usage-drain timer close the stream while the provider is deliberately
// holding the connection open, and so does the tool-call early exit.
//
// The filter goroutine closes rc too when it returns; a second Close on an
// http body is a no-op error.
func (b *sseEventFilterBody) Close() error {
	err := b.pr.Close()
	if cerr := b.rc.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}

// filterSSEEvents copies an SSE body from src to dst, suppressing the frames
// the openai-go v1 decoder chokes on: comment lines and any event whose data
// payload is empty (or whitespace-only). Everything else — real data lines,
// event/id/retry fields, the [DONE] sentinel, multi-line data — is forwarded
// byte-for-byte (with CRLF normalized to LF, which the SDK's line scanner
// treats identically).
//
// Events are buffered until their blank-line terminator, so a suppressed
// event's field lines cannot leak into the next event, and emission timing
// matches the SDK decoder exactly: one write per complete event, no added
// latency. A partial event at EOF stays buffered — the SDK needs the blank
// line to dispatch too, so this drops nothing the decoder would have used.
func filterSSEEvents(src io.Reader, dst io.Writer) error {
	scn := bufio.NewScanner(src)
	scn.Buffer(make([]byte, 0, 64*1024), sseFilterMaxLine)
	var evt bytes.Buffer
	var dataSeen bool
	for scn.Scan() {
		line := scn.Bytes()
		switch {
		case len(line) == 0:
			// Event terminator: emit the event only when it carried data.
			if dataSeen && evt.Len() > 0 {
				if _, err := dst.Write(append(evt.Bytes(), '\n')); err != nil {
					return err
				}
			}
			evt.Reset()
			dataSeen = false
		case line[0] == ':':
			// Comment frame: dropped (spec: clients ignore comments).
		default:
			if name, value, _ := bytes.Cut(line, []byte(":")); string(name) == "data" {
				// A whitespace-only data value cannot decode as JSON either
				// ("unexpected end of JSON input" all the same), so it
				// counts as empty for the keep-alive test.
				if len(bytes.TrimSpace(value)) > 0 {
					dataSeen = true
				}
			}
			evt.Write(line)
			evt.WriteByte('\n')
		}
	}
	return scn.Err()
}

// baseHTTPTransport unwraps the SSE client's transport stack — the API-error
// normalizer around the SSE filter around the dial-level *http.Transport (for
// tests and diagnostics).
func baseHTTPTransport(c *http.Client) (*http.Transport, bool) {
	eb, ok := c.Transport.(*apiErrorBodyTransport)
	if !ok {
		return nil, false
	}
	ft, ok := eb.base.(*sseFilterTransport)
	if !ok {
		return nil, false
	}
	tr, ok := ft.base.(*http.Transport)
	return tr, ok
}
