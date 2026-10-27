package llm

import (
	"errors"
	"net/http"
	"testing"

	"github.com/openai/openai-go/v3"
)

// TestPermanentStreamError pins which failures skip the recovery ladder: a
// client-side refusal the streaming retry already calls non-transient would pay
// the fallback backoff and a third full upload before surfacing the same error.
func TestPermanentStreamError(t *testing.T) {
	apiErr := func(status int) error {
		return &openai.Error{
			StatusCode: status,
			Message:    "refused",
			Request:    &http.Request{Method: "POST"},
			Response:   &http.Response{StatusCode: status},
		}
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"transport error", errors.New("unexpected end of JSON input"), false},
		{"bad request", apiErr(http.StatusBadRequest), true},
		{"unauthorized", apiErr(http.StatusUnauthorized), true},
		{"forbidden", apiErr(http.StatusForbidden), true},
		{"unknown model", apiErr(http.StatusNotFound), true},
		{"unprocessable", apiErr(http.StatusUnprocessableEntity), true},
		{"request timeout (retryable)", apiErr(http.StatusRequestTimeout), false},
		{"rate limited", apiErr(http.StatusTooManyRequests), false},
		{"server error", apiErr(http.StatusInternalServerError), false},
		{"context window keeps its own recovery", openAIContextError("context_length_exceeded", "too long"), false},
		{"context window marker keeps its own recovery", errors.New("maximum context length exceeded"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := permanentStreamError(tc.err); got != tc.want {
				t.Fatalf("permanentStreamError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
