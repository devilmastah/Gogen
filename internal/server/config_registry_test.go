package server

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestConfigFieldProjections is the permanent drift guard between the
// runtime overlay and the two projections: every registry field must
// reflect the overlay in the persistence snapshot (effectiveConfig) and,
// for client-settable fields, the decorated push must be sensitive to the
// overlay value (a field dropped from decorateConfig would stop changing
// when the overlay changes).
func TestConfigFieldProjections(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)

	for name, f := range configFields {
		t.Run(name, func(t *testing.T) {
			// Set a sample value on the overlay.
			r := s.ws.GetRuntimeConfig()
			f.set(&r, f.sample)
			s.ws.SetRuntimeConfig(r)

			// Persistence projection: effectiveConfig must reflect it.
			eff := s.effectiveConfig()
			if eff == nil {
				t.Fatal("effectiveConfig nil with a startup config")
			}
			if got, want := f.get(eff), f.sample; !reflect.DeepEqual(got, want) {
				t.Fatalf("effectiveConfig = %v, want %v", got, want)
			}

			if f.fromMsg == nil {
				return // overlay-only field: no client push projection
			}

			// Client push projection: the decorated msg must differ
			// between the sample and the zero overlay value.
			var setMsg WSMessage
			s.decorateConfig(&setMsg)
			zeroVal := reflect.Zero(reflect.TypeOf(f.sample)).Interface()
			r0 := s.ws.GetRuntimeConfig()
			f.set(&r0, zeroVal)
			s.ws.SetRuntimeConfig(r0)
			var zeroMsg WSMessage
			s.decorateConfig(&zeroMsg)
			if reflect.DeepEqual(f.msgGet(&setMsg), f.msgGet(&zeroMsg)) {
				t.Fatalf("push projection insensitive to overlay value (msgGet = %v)", f.msgGet(&setMsg))
			}
		})
	}
}

// TestRuntimeConfigFieldErrors pins the EXACT validation error string for
// every client-settable runtime-config option (the settings modal displays
// these verbatim). It is the behavior oracle for the config-field registry
// refactor: the registry's normalize closures must reproduce these strings
// byte-for-byte.
func TestRuntimeConfigFieldErrors(t *testing.T) {
	cases := []struct {
		name string
		msg  WSMessage
		want string
	}{
		{"commandSafety", WSMessage{Type: "config", ConfigFields: []string{"commandSafety"}, CommandSafetyMode: "maybe"},
			`Error: invalid commandSafety "maybe" (want blocklist, allowlist, or off)`},
		{"deleteApproval", WSMessage{Type: "config", ConfigFields: []string{"deleteApproval"}, DeleteApproval: "maybe"},
			`Error: invalid deleteApproval "maybe" (want required or off)`},
		{"commandApproval", WSMessage{Type: "config", ConfigFields: []string{"commandApproval"}, CommandApproval: "maybe"},
			`Error: invalid commandApproval "maybe" (want required or off)`},
		{"commandSandbox", WSMessage{Type: "config", ConfigFields: []string{"commandSandbox"}, CommandSandbox: "maybe"},
			`Error: invalid commandSandbox "maybe" (want off or bwrap)`},
		{"commandIdleTimeoutSecs", WSMessage{Type: "config", ConfigFields: []string{"commandIdleTimeoutSecs"}, CommandIdleTimeoutSecs: -1},
			"Error: commandIdleTimeoutSecs must be >= 0"},
		{"contextLimit", WSMessage{Type: "config", ConfigFields: []string{"contextLimit"}, ContextLimitConfig: -1},
			"Error: contextLimit must be >= 0"},
		{"compactThreshold", WSMessage{Type: "config", ConfigFields: []string{"compactThreshold"}, CompactThreshold: 1.5},
			"Error: compactThreshold must be between 0 and 1"},
		{"compactLastResort", WSMessage{Type: "config", ConfigFields: []string{"compactLastResort"}, CompactLastResort: "maybe"},
			`Error: invalid compactLastResort "maybe" (want condense or error)`},
		{"smartCompact", WSMessage{Type: "config", ConfigFields: []string{"smartCompact"}, SmartCompact: "sometimes"},
			`Error: invalid smartCompact "sometimes" (want on or off)`},
		{"contextMode", WSMessage{Type: "config", ConfigFields: []string{"contextMode"}, ContextMode: "deepdive"},
			`Error: invalid contextMode "deepdive" (want default or deep_dive)`},
		{"sessionMaxAgeDays", WSMessage{Type: "config", ConfigFields: []string{"sessionMaxAgeDays"}, SessionMaxAgeDays: -2},
			"Error: sessionMaxAgeDays must be >= -1 (-1 = keep sessions forever)"},
		{"compactKeepRecentMessages", WSMessage{Type: "config", ConfigFields: []string{"compactKeepRecentMessages"}, CompactKeepRecentMessages: -1},
			"Error: compactKeepRecentMessages must be >= 0"},
		{"maxToolResultBytes", WSMessage{Type: "config", ConfigFields: []string{"maxToolResultBytes"}, MaxToolResultBytes: -1},
			"Error: maxToolResultBytes must be >= 0"},
		{"compactReserveTokens", WSMessage{Type: "config", ConfigFields: []string{"compactReserveTokens"}, CompactReserveTokens: -1},
			"Error: compactReserveTokens must be >= 0"},
		{"sessionMaxCount", WSMessage{Type: "config", ConfigFields: []string{"sessionMaxCount"}, SessionMaxCount: -1},
			"Error: sessionMaxCount must be >= 0"},
		{"webApprovalHoldSecs", WSMessage{Type: "config", ConfigFields: []string{"webApprovalHoldSecs"}, WebApprovalHoldSecs: -1},
			"Error: webApprovalHoldSecs must be >= 0"},
		{"webMaxActiveSessions", WSMessage{Type: "config", ConfigFields: []string{"webMaxActiveSessions"}, WebMaxActiveSessions: -1},
			"Error: webMaxActiveSessions must be >= 0"},
		{"webFetch", WSMessage{Type: "config", ConfigFields: []string{"webFetch"}, WebFetch: "sometimes"},
			`Error: invalid webFetch "sometimes" (want on or off)`},
		{"outputSpill", WSMessage{Type: "config", ConfigFields: []string{"outputSpill"}, OutputSpill: "sometimes"},
			`Error: invalid outputSpill "sometimes" (want on or off)`},
		{"statusDisplay", WSMessage{Type: "config", ConfigFields: []string{"statusDisplay"}, StatusDisplay: "sometimes"},
			`Error: invalid statusDisplay "sometimes" (want off, auto, on, or browser)`},
		{"webSearch", WSMessage{Type: "config", ConfigFields: []string{"webSearch"}, WebSearch: "sometimes"},
			`Error: invalid webSearch "sometimes" (want on or off)`},
		{"treesitter", WSMessage{Type: "config", ConfigFields: []string{"treesitter"}, TreeSitter: "sometimes"},
			`Error: invalid treesitter "sometimes" (want on or off)`},
		{"mcp", WSMessage{Type: "config", ConfigFields: []string{"mcp"}, MCP: "sometimes"},
			`Error: invalid mcp "sometimes" (want on or off)`},
		{"webSearchBackend", WSMessage{Type: "config", ConfigFields: []string{"webSearchBackend"}, WebSearchBackend: "openai"},
			`Error: invalid webSearchBackend "openai" (want brave or empty)`},
		{"subagentModel", WSMessage{Type: "config", ConfigFields: []string{"subagentModel"}, SubagentModel: strPtr("   ")},
			"Error: subagentModel must be a model id or empty (inherit)"},
		{"subagentThinkingLevel", WSMessage{Type: "config", ConfigFields: []string{"subagentThinkingLevel"}, SubagentThinkingLevel: strPtr("   ")},
			"Error: subagentThinkingLevel must be a reasoning-effort value or empty (inherit)"},
		{"systemPrompt", WSMessage{Type: "config", ConfigFields: []string{"systemPrompt"}, SystemPrompt: strings.Repeat("x", maxPromptTemplateLen+1)},
			fmt.Sprintf("Error: systemPrompt exceeds %d characters", maxPromptTemplateLen)},
		{"unknown", WSMessage{Type: "config", ConfigFields: []string{"nope"}},
			`Error: unknown config option "nope"`},
		{"duplicate", WSMessage{Type: "config", ConfigFields: []string{"commandSafety", "commandSafety"}},
			`Error: config option "commandSafety" listed twice`},
	}
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	drainHandshake(t, conn)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := conn.WriteJSON(tc.msg); err != nil {
				t.Fatalf("send config: %v", err)
			}
			resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
			if resp.Kind != "settings" || resp.Success {
				t.Fatalf("notice = %+v, want settings error", resp)
			}
			if resp.Content != tc.want {
				t.Fatalf("error = %q, want %q", resp.Content, tc.want)
			}
		})
	}
}
