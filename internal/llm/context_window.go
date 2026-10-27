package llm

import (
	"errors"
	"net/http"
	"strings"

	"github.com/openai/openai-go/v3"
)

// ErrContextWindowExceeded is the sentinel for a provider context-window
// refusal: the request did not fit the model's context window and the
// provider rejected it. Local token estimates are approximate (cl100k vs
// the provider's real tokenizer), so a request that looked like it would
// fit can still be refused — the agent run loop recovers from this error
// in-loop (forced compaction + one retry) instead of aborting the turn.
var ErrContextWindowExceeded = errors.New("context window exceeded")

// contextWindowError wraps a provider error that was classified as a
// context-window refusal. Unwrap exposes BOTH the sentinel and the original
// error, so errors.Is(err, ErrContextWindowExceeded) holds while the
// original error stays inspectable in the chain (never masked).
type contextWindowError struct{ err error }

func (e *contextWindowError) Error() string { return e.err.Error() }

// Unwrap returns the sentinel and the original error.
func (e *contextWindowError) Unwrap() []error { return []error{ErrContextWindowExceeded, e.err} }

// contextWindowMarkers are the string backstops for providers and
// gateways whose refusal does not carry the OpenAI error code: the
// canonical refusal phrasings, matched case-insensitively.
var contextWindowMarkers = []string{
	"maximum context length",
	"prompt is too long",
	"too many tokens",
	// Local proxies and gateways reword the refusal; this phrasing is
	// emitted by the vLLM-style "exceeds the available context size"
	// error (type "exceed_context_size_error").
	"exceeds the available context size",
	"exceed_context_size",
}

// IsContextWindowError reports whether err is a provider context-window
// refusal. It is deliberately strict so non-overflow errors never take the
// recovery path: it matches the ErrContextWindowExceeded sentinel anywhere
// in the chain, the OpenAI SDK error code "context_length_exceeded", and —
// for providers/gateways that reword the refusal — the canonical marker
// strings.
func IsContextWindowError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrContextWindowExceeded) {
		return true
	}
	var oerr *openai.Error
	if errors.As(err, &oerr) {
		if oerr.Code == "context_length_exceeded" {
			return true
		}
		// A throttle is not an overflow, whatever its wording: Bedrock-style
		// 429s say "Too many tokens, please wait…", and classifying that as a
		// context refusal triggers a lossy forced compaction instead of the
		// backoff the provider asked for.
		if oerr.StatusCode == http.StatusTooManyRequests {
			return false
		}
	}
	msg := strings.ToLower(apiErrorText(err))
	for _, marker := range contextWindowMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// apiErrorText returns the text matched against err for provider error
// classification: the error's own text plus, for an *openai.Error, the
// parsed provider payload. Since openai-go v3.81, Error() prints only a
// status summary — the raw body is withheld because it can carry secrets —
// so the marker match must read the fields explicitly (the same pattern as
// chatTemplateKwargsRejected).
func apiErrorText(err error) string {
	var oerr *openai.Error
	if !errors.As(err, &oerr) {
		return err.Error()
	}
	return err.Error() + "\n" + oerr.Message + "\n" + oerr.RawJSON()
}

// wrapContextWindowError returns err wrapped so
// errors.Is(err, ErrContextWindowExceeded) holds when it is classified as a
// context-window refusal, and err unchanged otherwise. Call it on provider
// error paths so the agent run loop can classify the refusal without
// re-parsing the message.
func wrapContextWindowError(err error) error {
	if err == nil || !IsContextWindowError(err) {
		return err
	}
	return &contextWindowError{err: err}
}
