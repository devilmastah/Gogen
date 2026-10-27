package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/config"
	"gogen/internal/llm"
)

const (
	// defaultCallTimeout is the default bound for one MCP JSON-RPC call when
	// the caller has not supplied its own deadline and the server entry does
	// not override it (mcp_servers[].tool_timeout_secs). It is deliberately
	// generous: a reverse-engineering MCP server spends minutes inside a
	// native analysis engine (Ghidra/Hopper/IDA startup, binary indexing)
	// before it produces anything, and the previous 30s cap turned those
	// calls into spurious "context deadline exceeded" failures. A caller
	// with its own deadline (a turn the user cancels) still wins.
	defaultCallTimeout = 10 * time.Minute
	// defaultInitTimeout bounds initialize + tools/list during NewManager so
	// a hung MCP stdio server cannot stall the tool registry forever. It is
	// generous because the common way to publish an MCP server is
	// `npx -y <package>`, and a cold npx start downloads the package before
	// it speaks MCP at all; the old 5s bound silently dropped such servers.
	// Initialization runs off the startup path (startMCP), so a long bound
	// costs no startup latency.
	defaultInitTimeout = 60 * time.Second
	// mcpMaxSkippedResponses is how many responses with an ID that was never
	// issued are tolerated before the connection is declared broken.
	mcpMaxSkippedResponses = 100
	// maxToolsListPages bounds tools/list pagination: a server that keeps
	// returning a nextCursor must not make tool discovery loop forever.
	maxToolsListPages = 100
)

// mcpMaxLineBytes bounds one MCP stdio message line read by readLoop. A broken
// or hostile server that streams data with no newline (or one giant frame)
// would otherwise make an unbounded ReadBytes allocate without limit; the
// reader fails the connection instead. Matched to the openai-go decoder / SSE
// filter cap (bufio.MaxScanTokenSize << 9 = 32 MiB), far above any real
// JSON-RPC message. A var so tests can shrink it.
var mcpMaxLineBytes = bufio.MaxScanTokenSize << 9

// toolPrefix is prepended to every LLM-visible MCP tool name.
const toolPrefix = "mcp_"

var sanitizeRE = regexp.MustCompile(`[^a-z0-9_]+`)

// Registry aggregates MCP tools for the agent.
type Registry struct {
	tools map[string]toolEntry
}

type toolEntry struct {
	server string
	tool   string
	schema llm.Tool
	client *Client
}

// Client is a single MCP stdio connection with an async read loop.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	// callTimeout is the default deadline applied to one JSON-RPC call when
	// the caller supplies none (server entry tool_timeout_secs, or
	// defaultCallTimeout). Immutable after startClient.
	callTimeout time.Duration

	mu         sync.Mutex
	writeMu    sync.Mutex
	nextID     atomic.Int64
	pending    map[int64]chan jsonRPCResponse
	closed     chan struct{}
	closeOnce  sync.Once
	readerDone chan struct{}
}

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// cancelledParams is the MCP notifications/cancelled body. requestId is the
// JSON-RPC id of the call the client is abandoning.
type cancelledParams struct {
	RequestID int64  `json:"requestId"`
	Reason    string `json:"reason,omitempty"`
}

// Manager owns MCP server processes.
type Manager struct {
	clients []*Client
	reg     *Registry
}

// ValidServers returns entries that have both a name and a command.
// Incomplete stubs (mcp: on with placeholder objects) are dropped so
// NewManager is never asked to spawn empty exec.Command values, and an empty
// result is nil so callers can test it as "no servers". The validity rule
// itself lives in config.ValidMCPServers, which also answers
// Config.MCPActive (the config-derived predicate behind the model-facing tool
// set).
func ValidServers(servers []config.MCPServerConfig) []config.MCPServerConfig {
	if len(servers) == 0 {
		return nil
	}
	out := config.ValidMCPServers(servers)
	if len(out) == 0 {
		return nil
	}
	return out
}

// NewManager starts configured MCP servers and builds a tool registry.
//
// Servers start IN PARALLEL: each one may spend up to its own init_timeout_secs
// on a cold launcher (`npx -y <package>`), and starting them one after another
// made the total startup time the sum of those budgets — while the callers that
// wait for the handshake (the gateway, the settings test) only allow the
// SLOWEST single budget (MaxInitTimeout), so a third slow server was never
// waited for at all. Registration still happens in the configured order, so the
// tool set and its names stay deterministic.
func NewManager(servers []config.MCPServerConfig) (*Manager, error) {
	reg := &Registry{tools: make(map[string]toolEntry)}
	m := &Manager{reg: reg}
	valid := ValidServers(servers)
	type serverResult struct {
		name   string
		client *Client
		tools  []llm.Tool
	}
	results := make([]serverResult, len(valid))
	var wg sync.WaitGroup
	for i, s := range valid {
		wg.Add(1)
		go func(i int, s config.MCPServerConfig) {
			defer wg.Done()
			c, err := startClient(s)
			if err != nil {
				// Keep starting the remaining servers, but surface the failure:
				// a server that fails to spawn otherwise vanishes silently from
				// the model's toolset with no trace.
				log.Printf("mcp: server %q failed to start: %v", s.Name, err)
				return
			}
			initTimeout := initTimeoutFor(s)
			initCtx, cancel := context.WithTimeout(context.Background(), initTimeout)
			defer cancel()
			if err := c.initialize(initCtx); err != nil {
				_ = c.Close()
				// A slow launcher (cold `npx -y <package>`) is the common cause
				// of a handshake timeout, so name the bound that was hit: without
				// it the server just vanishes from the toolset with no trace.
				log.Printf("mcp: server %q failed to initialize within %s: %v (raise mcp_servers[].init_timeout_secs if the launcher is slow)", s.Name, initTimeout, err)
				return
			}
			tools, err := c.listTools(initCtx)
			if err != nil {
				_ = c.Close()
				log.Printf("mcp: server %q failed to list tools: %v", s.Name, err)
				return
			}
			results[i] = serverResult{name: s.Name, client: c, tools: tools}
		}(i, s)
	}
	wg.Wait()

	// Register in the configured order, never in completion order: the tool set
	// (and the duplicate-name suffixing reg.add applies) must not depend on
	// which server answered first.
	for _, r := range results {
		if r.client == nil {
			continue
		}
		for _, tool := range r.tools {
			reg.add(r.name, tool.Name, tool, r.client)
		}
		m.clients = append(m.clients, r.client)
	}
	return m, nil
}

// MaxInitTimeout is the longest initialize + tools/list budget across the
// configured servers. Callers that wait for the background handshake (the
// gateway, the settings test) use it so a per-server init_timeout_secs
// override beats the package default. The wait is therefore the SLOWEST
// configured server's budget, not the called server's own.
func MaxInitTimeout(servers []config.MCPServerConfig) time.Duration {
	var max time.Duration
	for _, s := range servers {
		if d := initTimeoutFor(s); d > max {
			max = d
		}
	}
	if max <= 0 {
		return defaultInitTimeout
	}
	return max
}

// initTimeoutFor resolves how long one server's initialize + tools/list may
// take: the entry's init_timeout_secs when set (0 = default), else
// defaultInitTimeout.
func initTimeoutFor(s config.MCPServerConfig) time.Duration {
	if s.InitTimeoutSecs > 0 {
		return time.Duration(s.InitTimeoutSecs) * time.Second
	}
	return defaultInitTimeout
}

// callTimeoutFor resolves the default deadline for one tools/call on this
// server: the entry's tool_timeout_secs when set (0 = default), else
// defaultCallTimeout. A non-positive result would mean "no bound", which the
// config layer never produces; negative values fall back to the default so a
// typo cannot remove the safety net.
func callTimeoutFor(s config.MCPServerConfig) time.Duration {
	if s.ToolTimeoutSecs > 0 {
		return time.Duration(s.ToolTimeoutSecs) * time.Second
	}
	return defaultCallTimeout
}

func startClient(s config.MCPServerConfig) (*Client, error) {
	cmd := exec.Command(s.Command, s.Args...)
	cmd.Env = os.Environ()
	for k, v := range s.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	// Discard stderr so MCP noise does not pollute the TUI/CLI.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		// cmd.Start failed, so the process never took ownership of the pipe
		// ends and Cmd.Wait (which normally closes them) is never called.
		// Close both parent ends here so the descriptors are released
		// promptly instead of waiting for the os.File finalizer at GC.
		_ = stdin.Close()
		_ = stdoutPipe.Close()
		return nil, err
	}
	c := &Client{
		cmd:         cmd,
		stdin:       stdin,
		stdout:      stdoutPipe,
		callTimeout: callTimeoutFor(s),
		pending:     make(map[int64]chan jsonRPCResponse),
		closed:      make(chan struct{}),
		readerDone:  make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

func (c *Client) readLoop() {
	defer close(c.readerDone)
	sc := bufio.NewScanner(c.stdout)
	// Bound the size of one JSON-RPC line (see mcpMaxLineBytes): a server that
	// never sends a newline must not make the reader allocate unboundedly. The
	// initial slice is capped at the limit too, so the bound is exact even
	// when a test shrinks mcpMaxLineBytes below the default read size.
	sc.Buffer(make([]byte, 0, min(64*1024, mcpMaxLineBytes)), mcpMaxLineBytes)
	skipped := 0
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var resp jsonRPCResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
			skipped = 0
		}
		c.mu.Unlock()
		if !ok {
			// A response whose ID was never issued is garbage on the wire.
			// A response to a call that already timed out is late but valid
			// (call() removes the pending entry on timeout) and must not
			// count toward the unmatched-response limit — otherwise a slow
			// server replying to many timed-out requests would trip the
			// limit and kill the connection.
			if resp.ID > c.nextID.Load() {
				skipped++
				if skipped > mcpMaxSkippedResponses {
					c.failPending(fmt.Errorf("mcp: too many unmatched responses; server may be broken"))
					return
				}
			}
			continue
		}
		select {
		case ch <- resp:
		default:
		}
	}
	// A clean EOF still means the server is gone: fail every in-flight call
	// promptly with io.EOF instead of leaving it to time out. Scanner errors
	// (e.g. bufio.ErrTooLong from mcpMaxLineBytes) are surfaced verbatim.
	if err := sc.Err(); err != nil {
		c.failPending(err)
		return
	}
	c.failPending(io.EOF)
}

func (c *Client) failPending(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, ch := range c.pending {
		select {
		case ch <- jsonRPCResponse{Error: &jsonRPCError{Message: err.Error()}}:
		default:
		}
		delete(c.pending, id)
	}
}

func (c *Client) initialize(ctx context.Context) error {
	_, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "gogen",
			"version": "1.0.0",
		},
	})
	if err != nil {
		return err
	}
	return c.notify("notifications/initialized", map[string]any{})
}

// mcpTool is one entry of a tools/list response.
type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// listTools fetches the server's full tool set, following nextCursor
// pagination. A large catalog (REA's server publishes tools for many analysis
// families) may be split across pages by the server; stopping at the first page would
// hide every tool after the first batch with no trace. Pagination is bounded
// by maxToolsListPages so a server that always returns a cursor cannot loop
// forever, and a repeated page is deduplicated instead of registered twice.
func (c *Client) listTools(ctx context.Context) ([]llm.Tool, error) {
	var out []llm.Tool
	seen := make(map[string]struct{})
	cursor := ""
	for page := 0; page < maxToolsListPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var result struct {
			Tools      []mcpTool `json:"tools"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, err
		}
		for _, t := range result.Tools {
			if _, dup := seen[t.Name]; dup {
				continue
			}
			seen[t.Name] = struct{}{}
			schema := t.InputSchema
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			out = append(out, llm.Tool{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  schema,
			})
		}
		if result.NextCursor == "" || result.NextCursor == cursor {
			return out, nil
		}
		cursor = result.NextCursor
	}
	// The page budget ran out with a cursor still pending: keep what was
	// discovered (a partial toolset beats none), but say so rather than
	// silently dropping the tail of the catalog.
	log.Printf("mcp: tools/list still paginating after %d pages; keeping the %d tools discovered so far", maxToolsListPages, len(out))
	return out, nil
}

func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	raw, err := c.call(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}
	var result toolCallResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return string(raw), nil
	}
	out := renderToolContent(result.Content)
	if strings.TrimSpace(out) == "" && len(result.StructuredContent) > 0 {
		// Structured-only results (MCP 2025-06-18 servers) would otherwise
		// reach the model as an empty tool result.
		out = renderStructuredContent(result.StructuredContent)
	}
	if result.IsError {
		if out == "" {
			out = "(the tool reported an error but returned no content)"
		}
		return out, fmt.Errorf("mcp tool error")
	}
	if out == "" {
		out = "(the tool completed successfully but returned no content)"
	}
	return out, nil
}

// mcpResource is the embedded-resource payload of a resource content part.
type mcpResource struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType"`
	Text     string `json:"text"`
}

// mcpContentPart is one part of a tools/call result content array. Only the
// fields the renderer needs are decoded; unknown part types are reported by
// type name instead of vanishing.
type mcpContentPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text"`
	MimeType string       `json:"mimeType"`
	Data     string       `json:"data"`
	Resource *mcpResource `json:"resource"`
}

type toolCallResult struct {
	Content           []mcpContentPart `json:"content"`
	StructuredContent json.RawMessage  `json:"structuredContent"`
	IsError           bool             `json:"isError"`
}

// renderToolContent flattens a content array into the string a tool result
// carries. Text parts pass through; every other part becomes an explicit
// placeholder carrying its type and size, because silently dropping them
// turned an image- or resource-only result into an empty tool result that
// the model reads as "the tool did nothing" (and then retries). REA returns
// screenshots and binary-analysis artifacts this way.
func renderToolContent(parts []mcpContentPart) string {
	var b strings.Builder
	writePart := func(s string) {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(s)
	}
	for _, part := range parts {
		switch part.Type {
		case "text":
			writePart(part.Text)
		case "image", "audio":
			mime := part.MimeType
			if mime == "" {
				mime = "unknown type"
			}
			writePart(fmt.Sprintf("[%s content: %s, %d bytes — not attached to the conversation]",
				part.Type, mime, base64DecodedLen(part.Data)))
		case "resource":
			switch {
			case part.Resource == nil:
				writePart("[resource content]")
			case part.Resource.Text != "":
				writePart(fmt.Sprintf("[resource %s]\n%s", part.Resource.URI, part.Resource.Text))
			default:
				writePart(fmt.Sprintf("[resource %s (%s) — content not inlined]", part.Resource.URI, part.Resource.MimeType))
			}
		default:
			writePart(fmt.Sprintf("[%s content: %d bytes]", part.Type, base64DecodedLen(part.Data)))
		}
	}
	return b.String()
}

// renderStructuredContent renders a structured-only result as indented JSON.
func renderStructuredContent(raw json.RawMessage) string {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		return string(raw)
	}
	return pretty.String()
}

// base64DecodedLen returns the decoded byte length of a base64 payload
// without allocating the decoded bytes; it only feeds the size in a
// placeholder, so a non-base64 or padded value just needs to be close.
func base64DecodedLen(data string) int {
	if data == "" {
		return 0
	}
	n := len(data) * 3 / 4
	if strings.HasSuffix(data, "==") {
		n -= 2
	} else if strings.HasSuffix(data, "=") {
		n--
	}
	if n < 0 {
		return 0
	}
	return n
}

func (c *Client) notify(method string, params any) error {
	select {
	case <-c.closed:
		return fmt.Errorf("mcp client closed")
	default:
	}
	req := jsonRPCRequest{JSONRPC: "2.0", Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

// withCallTimeout bounds an MCP request with the client's default call
// timeout only when the caller has not imposed its own deadline. A
// caller-provided deadline is honored as-is — even when it is longer than
// the default — so a legitimately long-running tools/call is not reported as
// failed early. initialize/listTools always pass their own init deadline, so
// they stay bounded. The returned CancelFunc is never nil; callers may defer
// it.
func (c *Client) withCallTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	if c.callTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.callTimeout)
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ctx, cancel := c.withCallTimeout(ctx)
	defer cancel()

	select {
	case <-c.closed:
		return nil, fmt.Errorf("mcp client closed")
	default:
	}

	id := c.nextID.Add(1)
	ch := make(chan jsonRPCResponse, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	idCopy := id
	req := jsonRPCRequest{JSONRPC: "2.0", ID: &idCopy, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	c.writeMu.Lock()
	_, err = c.stdin.Write(append(data, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		// The server is still inside the call. Tell it to stop. The VS Code
		// bridge closes its path-approval dialog when this arrives. A server
		// that ignores the notification is unchanged.
		_ = c.notify("notifications/cancelled", cancelledParams{
			RequestID: id,
			Reason:    "cancelled",
		})
		return nil, ctx.Err()
	case <-c.closed:
		return nil, fmt.Errorf("mcp client closed")
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("mcp rpc error: %s", resp.Error.Message)
		}
		return resp.Result, nil
	}
}

// Close shuts down the MCP client process.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		if c.stdout != nil {
			_ = c.stdout.Close()
		}
		if c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
			_, _ = c.cmd.Process.Wait()
		}
		// Wait for the reader to exit before touching pending under mu, so we
		// never contend with readLoop.failPending on the same mutex.
		<-c.readerDone
		c.failPending(fmt.Errorf("mcp client closed"))
	})
	return nil
}

// Close shuts down all MCP clients.
func (m *Manager) Close() error {
	for _, c := range m.clients {
		_ = c.Close()
	}
	return nil
}

// Registry returns the MCP tool registry.
func (m *Manager) Registry() *Registry {
	return m.reg
}

// ExternalToolName builds the LLM-visible MCP tool name.
func ExternalToolName(server, tool string) string {
	return toolPrefix + sanitize(server) + "_" + sanitize(tool)
}

// add registers one MCP tool under its LLM-visible external name. Two
// different (server, tool) pairs can sanitize to the same name — server
// "my-server" + tool "list_files" vs server "my" + tool "server_list_files",
// or "get-url" vs "get_url" on one server — and a bare map write would
// silently overwrite the earlier entry, making that tool vanish from the
// model's toolset with no trace. The FIRST registration wins (the same
// duplicate-ID precedence the provider profiles use) and the loser is logged
// so the config mistake is diagnosable.
func (r *Registry) add(server, tool string, schema llm.Tool, client *Client) {
	extName := ExternalToolName(server, tool)
	if prev, ok := r.tools[extName]; ok {
		log.Printf("mcp: duplicate tool name %q (server %q, tool %q) collides with server %q, tool %q; keeping the first registration — rename one of them",
			extName, server, tool, prev.server, prev.tool)
		return
	}
	r.tools[extName] = toolEntry{
		server: server,
		tool:   tool,
		schema: schema,
		client: client,
	}
}

func sanitize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = sanitizeRE.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "x"
	}
	return s
}

// Definitions implements agent.MCPToolRegistry.
func (r *Registry) Definitions() []llm.Tool {
	if r == nil || len(r.tools) == 0 {
		return nil
	}
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]llm.Tool, 0, len(r.tools))
	for _, name := range names {
		t := r.tools[name].schema
		t.Name = name
		out = append(out, t)
	}
	return out
}

// ToolNames implements agent.MCPToolRegistry.
func (r *Registry) ToolNames() map[string]struct{} {
	if r == nil {
		return nil
	}
	out := make(map[string]struct{}, len(r.tools))
	for name := range r.tools {
		out[name] = struct{}{}
	}
	return out
}

// HasTool reports whether name is a registered MCP tool (agent.MCPToolRegistry).
func (r *Registry) HasTool(name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.tools[name]
	return ok
}

// CallTool implements agent.MCPToolRegistry.
func (r *Registry) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	if r == nil {
		return "", fmt.Errorf("mcp registry not configured")
	}
	entry, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown mcp tool: %s", name)
	}
	return entry.client.CallTool(ctx, entry.tool, args)
}
