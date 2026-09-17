package llm

import (
	"encoding/json"
	"sync/atomic"

	"gogen/internal/debuglog"
)

// Wire-byte accounting: what a session actually uploads, split by contributor.
//
// The point of the measurement is to attribute traffic rather than guess at it.
// gogen's per-round upload is dominated by whatever the session carries: a
// screenshot (re-sent every round as a data URL), echoed reasoning content, or
// plain history. Only one of those is worth attacking, and which one it is
// depends entirely on the session — so the numbers come from the session.
//
// Cost control: everything here is gated on debug logging being enabled
// (debuglog.Enabled). When it is off, the only cost is one RLock-guarded path
// read per round.
//
// Categories are measured from the request inputs (message strings + tool
// definitions) and reconciled against the exact encoded body size, so an
// unmeasured contributor (JSON framing, escaping, the model name, extra
// fields) shows up as Framing rather than silently vanishing.

// wireBytes is one request's upload split by contributor, in bytes.
type wireBytes struct {
	System    int // system message content
	History   int // user/assistant/tool content, tool-call names/ids/arguments
	Reasoning int // echoed reasoning_content on historical assistant messages
	Images    int // image data URLs (base64) and their detail hint
	Tools     int // tool definitions (schemas)
	Framing   int // everything else: JSON syntax, escaping, model, extras
	Total     int // exact encoded body size

	// ReasoningDropped is reasoning the echo setting kept OFF the wire. It is
	// deliberately outside the reconciliation (Total never included it); it
	// exists so the log shows what the setting is saving.
	ReasoningDropped int
}

// measureWireBytes sums the content bytes a request will carry. It runs once
// per round (not per attempt) and only when debug logging is enabled.
func measureWireBytes(messages []Message, tools []Tool) wireBytes {
	var w wireBytes
	// Reasoning is only on the wire when the echo is enabled; counting it
	// otherwise would make the parts exceed the exact total (negative
	// Framing) and misattribute the saving.
	echoReasoning := reasoningEchoEnabled()
	for i := range messages {
		m := &messages[i]
		if m.Role == "system" {
			w.System += len(m.Content)
			continue
		}
		w.History += len(m.Content) + len(m.ToolCallID)
		for j := range m.ToolCalls {
			tc := &m.ToolCalls[j]
			w.History += len(tc.ArgsStr) + len(tc.Name) + len(tc.ID)
		}
		if echoReasoning {
			w.Reasoning += len(m.Reasoning)
		} else {
			w.ReasoningDropped += len(m.Reasoning)
		}
		for j := range m.Images {
			w.Images += len(m.Images[j].DataURL) + len(m.Images[j].Detail)
		}
	}
	if len(tools) > 0 {
		if raw, err := json.Marshal(tools); err == nil {
			w.Tools = len(raw)
		}
	}
	return w
}

// wireCounters accumulates a provider's (session's) cumulative upload. The
// totals are logged with every request so the debug log is self-summarizing:
// the last entry for a session answers "how much did this session send, and
// what was it" without post-processing the whole log.
//
// The counts are per REPORTED WIRE VARIANT, not per HTTP attempt: the stream
// ladder's muted retry reuses the memoized body and is not counted again, so
// `requests` is a round/variant count and the totals undercount a retried
// round's actual upload. Diagnostic-only; the exact body size (Total) is the
// variant's own.
type wireCounters struct {
	requests  atomic.Int64
	total     atomic.Int64
	system    atomic.Int64
	history   atomic.Int64
	reasoning atomic.Int64
	images    atomic.Int64
	tools     atomic.Int64
}

func (c *wireCounters) record(w wireBytes) {
	c.requests.Add(1)
	c.total.Add(int64(w.Total))
	c.system.Add(int64(w.System))
	c.history.Add(int64(w.History))
	c.reasoning.Add(int64(w.Reasoning))
	c.images.Add(int64(w.Images))
	c.tools.Add(int64(w.Tools))
}

// logWireBytes emits one request's byte breakdown plus the session's cumulative
// totals. data carries the exact body size (Total) and the measured parts;
// Framing reconciles the two.
func (p *OpenAIProvider) logWireBytes(w wireBytes, variant string) {
	if !debuglog.Enabled() {
		return
	}
	p.wire.record(w)
	session := ""
	if p.sessionHdr != nil {
		session = p.sessionHdr.uuid()
	}
	debuglog.Write("llm/wire_bytes", "chat request upload by contributor", "", map[string]any{
		"variant":   variant,
		"model":     p.currentModel(),
		"session":   session,
		"total":     w.Total,
		"system":    w.System,
		"history":   w.History,
		"reasoning": w.Reasoning,
		// Bytes the echo setting kept off the wire; not part of the sum above.
		"reasoningDropped": w.ReasoningDropped,
		"images":           w.Images,
		"tools":            w.Tools,
		"framing":          w.Framing,
		"sessionRequests":  p.wire.requests.Load(),
		"sessionTotal":     p.wire.total.Load(),
		"sessionSystem":    p.wire.system.Load(),
		"sessionHistory":   p.wire.history.Load(),
		"sessionReasoning": p.wire.reasoning.Load(),
		"sessionImages":    p.wire.images.Load(),
		"sessionTools":     p.wire.tools.Load(),
	})
}
