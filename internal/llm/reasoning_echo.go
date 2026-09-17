package llm

import (
	"os"

	"gogen/internal/onoff"
)

// reasoningEchoEnabled reports whether historical assistant reasoning is re-sent
// as reasoning_content on every request.
//
// Echoing is byte-stable — the prefix never changes as the conversation grows,
// which is what keeps a provider's prompt cache valid — and some providers
// expect their earlier thinking echoed back. It is also the single largest
// avoidable payload item on a reasoning-heavy session: reasoning can be a third
// or more of the conversation, and chat completions is stateless, so the whole
// of it is re-uploaded on EVERY round.
//
// GOGEN_REASONING_ECHO=off drops it. That keeps the bytes stable (nothing is
// ever sent, so nothing shifts) and shrinks each request; the model simply no
// longer sees its own earlier thinking, which is what several providers do
// anyway by stripping reasoning from replayed history.
//
// Deliberately all-or-nothing rather than "keep the most recent N turns":
// dropping older reasoning as the conversation grows changes the bytes of
// EARLIER messages, which invalidates the provider's cached prefix on every
// subsequent round — the opposite of the intended saving.
func reasoningEchoEnabled() bool {
	if on, ok := onoff.Parse(os.Getenv("GOGEN_REASONING_ECHO")); ok {
		return on
	}
	return true
}
