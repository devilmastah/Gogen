package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gogen/internal/llm"
)

// Nested MCP exposure (mcp_tool_mode: nested, the default) reaches every
// server tool through three fixed definitions — mcp_list, mcp_describe, and
// mcp_call — instead of inlining one definition per server tool.
//
// Three tools, not one tool with an action enum: smaller and quantized models
// drop a required discriminator and nest the whole call under a parameter
// named arguments, because that word is already the tool-call envelope. The
// tool name carries the operation, and the server tool's own parameters live
// in input.
//
// Why nested is the default:
//   - Prompt caching. A provider caches a byte-exact request prefix, and tool
//     definitions sit at the very FRONT of it (before system prompt and
//     messages). Inlining a catalog makes the prefix grow with the catalog and
//     change whenever the registry attaches, a server reconnects, or an MCP
//     tool shadows a builtin — every such change invalidates the whole
//     conversation, not a tail slice. The three gateway definitions are fixed,
//     so the prefix is identical for every session regardless of how many
//     servers exist.
//   - Context budget. A reverse-engineering server publishes dozens of tools;
//     their names, descriptions and JSON schemas are re-sent on every round.
//     Here they cost three definitions and are only materialized when the model
//     asks (and the answer lands in the message tail, where it invalidates
//     nothing before it).
//
// The mode is decided ONCE at construction (SetMCPTooling), never from the
// live registry: that is what keeps the prefix stable while a slow
// `npx`-launched server is still connecting.

const (
	mcpListTool     = "mcp_list"
	mcpDescribeTool = "mcp_describe"
	mcpCallTool     = "mcp_call"
)

// mcpGatewayToolNames is the nested-mode tool set, in definition order.
func mcpGatewayToolNames() []string {
	return []string{mcpListTool, mcpDescribeTool, mcpCallTool}
}

// mcpListLimit bounds one `list` answer. A bigger catalog is reported as a
// count with a hint to narrow the query rather than dumped whole.
const mcpListLimit = 40

// SetMCPTooling records how MCP tools are exposed for this session, and
// whether any MCP server is configured at all.
//
// configured must come from the SESSION CONFIG (MCP enabled + at least one
// server), NOT from the live registry: the tool set and the system prompt are
// derived from it, so deriving it from a registry that attaches asynchronously
// would rewrite the request prefix mid-session and invalidate the provider's
// prompt cache. nested selects the gateway tool over inlined definitions.
//
// Both values are stable for the session's lifetime; this setter exists for
// construction-time wiring (and tests), not for live toggling.
func (a *Agent) SetMCPTooling(configured, nested bool) {
	a.mcpConfigured.Store(configured)
	a.mcpNested.Store(nested)
	a.noteToolsChanged()
}

// MCPConfigured reports whether the session was constructed with MCP servers
// configured (independent of whether one has connected yet).
func (a *Agent) MCPConfigured() bool {
	return a.mcpConfigured.Load()
}

// MCPNested reports whether MCP tools are exposed through the gateway tool
// rather than inlined one definition per tool.
func (a *Agent) MCPNested() bool {
	return a.mcpNested.Load()
}

// mcpGatewayEnabled reports whether the nested MCP tools are part of this
// session's tool set.
func (a *Agent) mcpGatewayEnabled() bool {
	return a.MCPConfigured() && a.MCPNested()
}

// mcpToolHint returns the system-prompt line that tells the model how to reach
// MCP tools, or "" when the session has no MCP servers configured. The text
// depends only on construction-time state (never on the live registry), so the
// prompt prefix stays byte-stable for the whole session.
func (a *Agent) mcpToolHint() string {
	switch {
	case !a.MCPConfigured():
		return ""
	case a.MCPNested():
		return mcpGatewayHint
	default:
		return mcpInlineHint
	}
}

// mcpGatewayFeatureTools is the nested-mode tool set. Each definition is
// CONSTANT: no tool counts, no server names, no health. Anything volatile
// here would change the request prefix on every turn.
func mcpGatewayFeatureTools() []featureTool {
	return []featureTool{
		{Name: mcpListTool, Definition: mcpListToolDef(), Handler: handleMCPList},
		{Name: mcpDescribeTool, Definition: mcpDescribeToolDef(), Handler: handleMCPDescribe},
		{Name: mcpCallTool, Definition: mcpCallToolDef(), Handler: handleMCPCall},
	}
}

func mcpListToolDef() llm.Tool {
	return llm.Tool{
		Name: mcpListTool,
		Description: "List the tools published by connected MCP servers. " +
			"Returns names and a one-line summary. Optional query is a case-insensitive filter. " +
			"Example: {\"query\":\"decompile\"}. Call this before mcp_describe or mcp_call. " +
			"Disabled in plan mode.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "Optional filter over tool names and descriptions (e.g. \"binary\" or \"decompile\").",
				},
			},
		},
	}
}

func mcpDescribeToolDef() llm.Tool {
	return llm.Tool{
		Name: mcpDescribeTool,
		Description: "Show one MCP tool's argument schema. tool is a name from mcp_list, with or without the mcp_ prefix. " +
			"Example: {\"tool\":\"mcp_rea_xrefs\"}. Disabled in plan mode.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool": map[string]any{
					"type":        "string",
					"description": "Tool name from mcp_list (e.g. mcp_rea_inspect or rea_inspect).",
				},
			},
			"required": []string{"tool"},
		},
	}
}

func mcpCallToolDef() llm.Tool {
	return llm.Tool{
		Name: mcpCallTool,
		Description: "Run one MCP tool. tool is a name from mcp_list. input is only that tool's own parameters. " +
			"Example: {\"tool\":\"mcp_rea_search_strings\",\"input\":{\"pattern\":\"demo\",\"case_sensitive\":false}}. " +
			"Do not put tool inside input. Disabled in plan mode.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool": map[string]any{
					"type":        "string",
					"description": "Tool name from mcp_list (e.g. mcp_rea_search_strings or search_strings).",
				},
				"input": map[string]any{
					"type":        "object",
					"description": "That tool's own parameters. A JSON-encoded string is also accepted. Omit when the tool takes none.",
				},
			},
			"required": []string{"tool"},
		},
	}
}

func handleMCPList(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	return a.mcpListTools(ctx, args)
}

func handleMCPDescribe(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	return a.mcpDescribeTool(ctx, args)
}

func handleMCPCall(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	return a.mcpCallTool(ctx, args)
}

// mcpRegistryOrError returns the MCP registry, or a diagnostic that tells the
// model what to do when a configured server has not attached (yet).
//
// A registry installed before the handshake (PendingMCPRegistry) is shared by
// every session. Wait here so the first list/call blocks until that handshake
// finishes or its init timeout elapses, instead of failing instantly and
// leaving a session that copied a nil pointer unable to ever see the tools.
func (a *Agent) mcpRegistryOrError(ctx context.Context) (MCPToolRegistry, error) {
	if a.MCPRegistry == nil {
		return nil, fmt.Errorf("no MCP server is connected yet (the servers are configured but none finished its handshake); retry this call shortly, or report that MCP is unavailable if it keeps failing")
	}
	if w, ok := a.MCPRegistry.(MCPRegistryWaiter); ok {
		if err := w.Wait(ctx); err != nil {
			return nil, err
		}
	}
	return a.MCPRegistry, nil
}

// mcpListTools implements mcp_list: sorted names with a one-line summary,
// filtered by the optional query and bounded by mcpListLimit.
func (a *Agent) mcpListTools(ctx context.Context, args map[string]any) (string, error) {
	reg, err := a.mcpRegistryOrError(ctx)
	if err != nil {
		return "", err
	}
	query, err := stringArgOptional(args, "query")
	if err != nil {
		return "", err
	}
	query = strings.ToLower(strings.TrimSpace(query))

	// Copy before sorting: a registry may hand back its own slice, and the
	// gateway must not reorder another caller's tool list.
	defs := append([]llm.Tool(nil), reg.Definitions()...)
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })

	var matches []llm.Tool
	for _, def := range defs {
		if query != "" && !mcpToolMatches(def, query) {
			continue
		}
		matches = append(matches, def)
	}
	if len(matches) == 0 {
		if len(defs) == 0 {
			return "No MCP tools are registered yet (the configured servers have not finished connecting).", nil
		}
		return fmt.Sprintf("No MCP tool matches %q. %d tools are available; call mcp_list without a query to see them.", query, len(defs)), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d MCP tool(s) available", len(defs))
	if query != "" {
		fmt.Fprintf(&b, ", %d matching %q", len(matches), query)
	}
	b.WriteString(". Call action=describe for a tool's argument schema, then action=call.\n")
	shown := matches
	if len(shown) > mcpListLimit {
		shown = shown[:mcpListLimit]
	}
	for _, def := range shown {
		b.WriteString("\n")
		b.WriteString(def.Name)
		if summary := mcpToolSummary(def.Description); summary != "" {
			b.WriteString(" — ")
			b.WriteString(summary)
		}
	}
	if len(matches) > len(shown) {
		fmt.Fprintf(&b, "\n\n… and %d more; narrow it with a query.", len(matches)-len(shown))
	}
	return b.String(), nil
}

// mcpDescribeTool implements action=describe: the resolved name, its
// description, and the indented JSON input schema.
func (a *Agent) mcpDescribeTool(ctx context.Context, args map[string]any) (string, error) {
	reg, err := a.mcpRegistryOrError(ctx)
	if err != nil {
		return "", err
	}
	want, err := stringArg(args, "tool")
	if err != nil {
		return "", err
	}
	def, name, err := resolveMCPTool(reg, want)
	if err != nil {
		return "", err
	}
	schema := def.Parameters
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	encoded, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode %s schema: %w", name, err)
	}
	var b strings.Builder
	b.WriteString(name)
	b.WriteString("\n")
	if desc := strings.TrimSpace(def.Description); desc != "" {
		b.WriteString(desc)
		b.WriteString("\n")
	}
	b.WriteString("\nArguments (JSON schema):\n")
	b.Write(encoded)
	b.WriteString("\n\nRun it with mcp_call, tool=")
	b.WriteString(name)
	b.WriteString(", input={...}.")
	return b.String(), nil
}

// mcpCallTool implements action=call: resolve the name, decode the arguments
// (object or JSON string), and dispatch to the server. A tool error keeps the
// server's text so the model can act on it.
func (a *Agent) mcpCallTool(ctx context.Context, args map[string]any) (string, error) {
	reg, err := a.mcpRegistryOrError(ctx)
	if err != nil {
		return "", err
	}
	want, err := stringArg(args, "tool")
	if err != nil {
		return "", err
	}
	_, name, err := resolveMCPTool(reg, want)
	if err != nil {
		return "", err
	}
	callArgs, err := mcpCallInput(args)
	if err != nil {
		return "", err
	}
	out, err := reg.CallTool(ctx, name, callArgs)
	if err != nil {
		return out, fmt.Errorf("mcp tool %s: %w", name, err)
	}
	return out, nil
}

// mcpCallInput reads the server tool's parameters from input. arguments is
// accepted as the same field when input is absent: that is the payload
// object, not a wrapper around tool or any other gateway key.
func mcpCallInput(args map[string]any) (map[string]any, error) {
	if _, ok := args["input"]; ok {
		return mcpArguments(args["input"])
	}
	if _, ok := args["arguments"]; ok {
		return mcpArguments(args["arguments"])
	}
	return map[string]any{}, nil
}

// mcpArguments normalizes one payload value. Models emit either an object
// or a JSON-encoded string; both are accepted. Absent/null means "no
// parameters".
func mcpArguments(raw any) (map[string]any, error) {
	switch v := raw.(type) {
	case nil:
		return map[string]any{}, nil
	case map[string]any:
		return v, nil
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return map[string]any{}, nil
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
			return nil, fmt.Errorf("input must be a JSON object: %w", err)
		}
		if out == nil {
			return map[string]any{}, nil
		}
		return out, nil
	default:
		return nil, fmt.Errorf("input must be a JSON object, got %T", raw)
	}
}

// resolveMCPTool maps a model-supplied name onto a registered tool. Accepts the
// exact registry name, the name without its mcp_ prefix, or a unique suffix
// (the prefix already encodes the server, so requiring it is gratuitous
// friction). The registry's definition list is small enough that this stays a
// linear scan.
func resolveMCPTool(reg MCPToolRegistry, want string) (llm.Tool, string, error) {
	want = strings.TrimSpace(want)
	if want == "" {
		return llm.Tool{}, "", fmt.Errorf("missing required argument %q", "tool")
	}
	defs := reg.Definitions()
	byName := make(map[string]llm.Tool, len(defs))
	for _, def := range defs {
		byName[def.Name] = def
	}
	if def, ok := byName[want]; ok {
		return def, want, nil
	}
	if def, ok := byName["mcp_"+strings.ToLower(want)]; ok {
		return def, "mcp_" + strings.ToLower(want), nil
	}
	suffix := "_" + strings.ToLower(want)
	var matchName string
	matches := 0
	for name := range byName {
		if strings.HasSuffix(name, suffix) {
			matchName = name
			matches++
		}
	}
	if matches == 1 {
		return byName[matchName], matchName, nil
	}
	if matches > 1 {
		return llm.Tool{}, "", fmt.Errorf("tool %q is ambiguous (%d MCP tools end with it); pass the full name from mcp_list", want, matches)
	}
	return llm.Tool{}, "", fmt.Errorf("unknown MCP tool %q; call mcp_list to see the available tools", want)
}

// mcpToolMatches reports whether a tool matches a lowercased query over its
// name, description, and argument names.
func mcpToolMatches(def llm.Tool, query string) bool {
	if strings.Contains(strings.ToLower(def.Name), query) ||
		strings.Contains(strings.ToLower(def.Description), query) {
		return true
	}
	if props, ok := def.Parameters["properties"].(map[string]any); ok {
		for name := range props {
			if strings.Contains(strings.ToLower(name), query) {
				return true
			}
		}
	}
	return false
}

// mcpToolSummary compresses a tool description to its first line, bounded.
// Server descriptions can be paragraphs; list output is an index, not a manual.
func mcpToolSummary(description string) string {
	desc := strings.TrimSpace(description)
	if i := strings.IndexByte(desc, '\n'); i >= 0 {
		desc = desc[:i]
	}
	desc = strings.TrimSpace(desc)
	const maxRunes = 120
	if runes := []rune(desc); len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}
	return desc
}
