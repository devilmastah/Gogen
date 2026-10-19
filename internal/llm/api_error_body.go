package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
)

// apiErrorMaxMessageBytes bounds the message synthesized from an error body:
// the body can be a whole HTML gateway page, and the SDK's error text inlines
// the payload into the turn error the UI renders.
const apiErrorMaxMessageBytes = 1024

// apiErrorMaxBodyBytes bounds how much of a >=400 body this layer buffers
// for inspection (reading one byte past the cap detects oversize). The
// synthesized message is clipped far below this anyway, and the SDK reads
// the whole body itself to build its apierror; past the cap the body is
// spliced back together and streams on, so a multi-MB HTML gateway page is
// never materialized twice.
const apiErrorMaxBodyBytes = 1 << 20

// apiErrorBodyTransport rewrites non-standard error payloads into the one
// shape openai-go's apierror parser can read, so the provider's own message
// reaches the user instead of a Go decoding error.
//
// requestconfig parses every >=400 body by extracting the top-level "error"
// member with gjson and unmarshalling it into apierror.Error. Only a JSON
// object under "error" survives that path; the shapes OpenAI-compatible
// servers actually send fail or lose the message in two ways (verified
// against openai-go v3.59.0):
//
//	{"error":{"message":"..."}}          parsed; left byte-for-byte untouched
//	{"error":"model not found"}          HARD FAILURE: json: cannot unmarshal
//	                                     string into Go value of type
//	                                     apierror.Error — the decode error
//	                                     REPLACES the provider's text
//	                                     (Ollama and assorted gateways)
//	{"error":123} / ["x"] / true         HARD FAILURE, same reason
//	{"error":null}                       parsed, text is just "null"
//	{"error":""}                         HARD FAILURE (empty string)
//	{"object":"error","message":"..."}   parsed EMPTY: the SDK text reads
//	                                     "400 Bad Request " with no detail
//	                                     (vLLM)
//	{"detail":"..."}                     parsed empty (FastAPI-fronted servers)
//	"model not found"                    parsed empty (bare JSON string body)
//	<plain text / HTML gateway page>     parsed empty
//
// The hard failures additionally burn the whole recovery ladder (the decode
// error is not an *openai.Error, so retryableStreamError treats it as a
// transport drop and re-requests the turn) and silence every error-text
// consumer — context-window classification, stream retry labels, and the
// final turn error all lose the provider's words.
//
// The replacement is minimal: {"error":{"message":<extracted>}}. Only the
// message is synthesized because the message is all the user ever sees
// (openai-go >= v3.81 prints just a status summary from Error(), and
// apiErrorSurface re-surfaces the parsed message — see below), and a code
// copied into the error object must not risk its own decode failure (vLLM
// sends a numeric "code" where apierror.Error expects a string). Bodies the
// SDK already renders (the object shape) pass through byte-for-byte, as do
// every sub-400 response, any body still compressed at this layer (the
// stdlib only decodes gzip it advertised, and the SSE client disables
// compression, so a compressed error page is not inspectable here), and any
// body larger than apiErrorMaxBodyBytes (only the cap is read; the rest
// streams on for the SDK, which reads the whole body itself).
type apiErrorBodyTransport struct {
	base http.RoundTripper
}

func (t *apiErrorBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	res, err := base.RoundTrip(req)
	if err != nil || res == nil || res.Body == nil || res.StatusCode < 400 {
		return res, err
	}
	// A body this layer cannot decode (see the type comment) must stay
	// compressed: reading it here would hand the SDK the raw encoded bytes.
	if enc := strings.ToLower(strings.TrimSpace(res.Header.Get("Content-Encoding"))); enc != "" && enc != "identity" {
		return res, nil
	}
	// Read at most the cap (peeking one byte past it): the head is inspected
	// and rewritten only when it is the whole body. An oversize body is left
	// to the SDK, with the bytes already read spliced back in front.
	head, err := io.ReadAll(io.LimitReader(res.Body, apiErrorMaxBodyBytes+1))
	if err != nil {
		_ = res.Body.Close()
		return nil, err
	}
	if len(head) > apiErrorMaxBodyBytes {
		res.Body = splicedBody{io.MultiReader(bytes.NewReader(head), res.Body), res.Body}
		return res, nil
	}
	_ = res.Body.Close()
	// Restore the body unconditionally: the SDK still needs a readable
	// stream even when nothing is rewritten.
	res.Body = io.NopCloser(bytes.NewReader(head))
	res.ContentLength = int64(len(head))
	if rewritten, ok := normalizeAPIErrorBody(head); ok {
		res.Body = io.NopCloser(bytes.NewReader(rewritten))
		res.ContentLength = int64(len(rewritten))
		// Keep the header consistent with the field. The SDK reads the
		// body, not the header, but a stale length is a trap for a future
		// consumer.
		if res.Header.Get("Content-Length") != "" {
			res.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
		}
	}
	return res, nil
}

// splicedBody re-joins an already-read prefix with the unread remainder of a
// response body, so an oversize error body streams on byte-for-byte; Close
// closes the underlying stream.
type splicedBody struct {
	io.Reader
	io.Closer
}

// normalizeAPIErrorBody returns the {"error":{"message":…}} replacement for a
// >=400 body whose message would otherwise be replaced by a decode error or
// dropped, and ok=false when the SDK's own parse already yields it (see
// apiErrorMessage for the per-shape rules).
func normalizeAPIErrorBody(body []byte) ([]byte, bool) {
	msg, ok := apiErrorMessage(bytes.TrimSpace(body))
	if !ok {
		return nil, false
	}
	if msg = strings.TrimSpace(msg); msg == "" {
		return nil, false
	}
	// Encoder (not json.Marshal) so SetEscapeHTML(false) keeps HTML gateway
	// pages readable instead of \u003c-escaped.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	payload := struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}{}
	payload.Error.Message = clipAPIErrorMessage(msg)
	if err := enc.Encode(payload); err != nil {
		return nil, false // unreachable: the payload is a plain string
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), true
}

// apiErrorMessage extracts the provider's message from a >=400 body:
//
//   - a JSON string under "error" is used as-is;
//   - a number/bool/array under "error" is kept as its JSON text;
//   - an object under "error" is the SDK's own shape and must not be
//     rewritten (ok=false);
//   - otherwise the first non-empty top-level "message", "detail", or "msg"
//     string is used (vLLM, FastAPI-fronted servers);
//   - a bare JSON string body is the message itself;
//   - anything else textual (a plain-text or HTML gateway page, or an
//     unrecognized JSON envelope) is kept whole.
//
// ok=false only when nothing textual can be salvaged: an empty body, or bytes
// that are not valid UTF-8 text (a binary payload — rewriting it would embed
// noise where the SDK at least reports the bare HTTP status).
func apiErrorMessage(body []byte) (string, bool) {
	// Raw members (not a typed envelope): a member of an unexpected type
	// must not defeat the extraction of the other members.
	var members map[string]json.RawMessage
	if err := json.Unmarshal(body, &members); err == nil {
		raw := bytes.TrimSpace(members["error"])
		switch {
		case len(raw) > 0 && raw[0] == '{':
			// The SDK's own error shape: its text carries the raw object.
			return "", false
		case len(raw) > 0 && raw[0] == '"':
			var s string
			if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
				return s, true
			}
			// An empty string member is still a decode failure for the SDK
			// ("cannot unmarshal string"); fall through to the whole body.
		case len(raw) > 0 && raw[0] != 'n': // number, bool, or array (null falls through)
			return string(raw), true
		}
		for _, key := range []string{"message", "detail", "msg"} {
			var s string
			if err := json.Unmarshal(members[key], &s); err == nil && strings.TrimSpace(s) != "" {
				return s, true
			}
		}
	}
	var s string
	if err := json.Unmarshal(body, &s); err == nil {
		return s, true
	}
	if utf8.Valid(body) && !bytes.ContainsRune(body, 0) {
		return string(body), true
	}
	return "", false
}

// clipAPIErrorMessage bounds a synthesized message on a rune boundary, with
// the same "(truncated)" marker the context manager uses.
func clipAPIErrorMessage(s string) string {
	if len(s) <= apiErrorMaxMessageBytes {
		return s
	}
	cut := apiErrorMaxMessageBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "… (truncated)"
}

// apiErrorSurface wraps an *openai.Error so the provider's message is part
// of the error text. Since openai-go v3.81, apierror.Error.Error() prints
// only a status summary ("OpenAI API error: 400 Bad Request") — the raw
// body is deliberately withheld because it can carry secrets — so without
// this wrapper the provider's words would be lost from every user-facing
// error (turn errors, stream retry labels). The parsed Message field is the
// SDK's own sanctioned surface for the provider's text; the original error
// stays reachable through Unwrap, so errors.As(err, &*openai.Error) keeps
// working.
type apiErrorSurface struct{ err error }

func (e *apiErrorSurface) Error() string {
	var oerr *openai.Error
	if !errors.As(e.err, &oerr) {
		return e.err.Error()
	}
	msg := strings.TrimSpace(oerr.Message)
	if msg == "" || strings.Contains(e.err.Error(), msg) {
		return e.err.Error()
	}
	return e.err.Error() + ": " + clipAPIErrorMessage(msg)
}

func (e *apiErrorSurface) Unwrap() error { return e.err }

// surfaceAPIError wraps err so the provider's message is part of the error
// text (see apiErrorSurface). It is applied at the provider boundary where
// an SDK error escapes to hosts; errors that are not an *openai.Error, or
// whose text already carries the message, pass through unchanged.
func surfaceAPIError(err error) error {
	if err == nil {
		return nil
	}
	var oerr *openai.Error
	if !errors.As(err, &oerr) {
		return err
	}
	if oerr.Message == "" || strings.Contains(err.Error(), oerr.Message) {
		return err
	}
	return &apiErrorSurface{err: err}
}
