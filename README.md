# GoGen — AI-Powered Coding Agent

GoGen is a self-hosted coding agent that works on real codebases. Point it at
a project and describe the change you want in plain English: it explores the
layout, searches across files, edits code, runs commands, and checks the
results — from your terminal, a web browser, or a one-shot CLI prompt.

It's built to be handed real work:

- **Runs anywhere** — full-screen TUI, browser UI with multiple chat panes and
  a built-in terminal, or `-p "..."` for a single prompt
- **Any OpenAI-compatible API** — OpenAI, local llama.cpp/Ollama servers, or a
  proxy; register several endpoints and pick a model per session
- **Safe by default** — read-only plan mode, a command guard (blocklist or
  allowlist), sandboxed commands, and explicit approval for destructive
  actions
- **Remembers** — sessions auto-save and resume, history auto-compacts to fit
  the model's context window, and todos/pins survive across turns
- **Knows your project** — project guidelines, `AGENTS.md`/`CLAUDE.md`
  instructions, skills, and auto-detected test/lint commands shape how it
  works in your repo
- **Extensible** — connect MCP servers for extra tools, spawn subagents for
  parallel work, and track tickets on a project kanban board

## Quick start

### Prerequisites

- Go 1.27+
- A C compiler (for CGO / tree-sitter), e.g. `gcc` on Linux
- An OpenAI-compatible API key

### Build and run

```bash
go build -o gogen .
OPENAI_API_KEY=sk-... ./gogen /path/to/project
```

The TUI starts with your project as the working directory. Try prompts like:

```
> What test command does this project use?
> Find the flag parser in main.go and add a --dry-run flag
> Run the tests for the config package
```

Prefer a browser? Run with `--web` (http://127.0.0.1:8081). For a single
prompt without any UI, use `-p`. The VS Code plugin starts gogen on a free
port for that window, and that port is not saved as `web_bind`.

```bash
OPENAI_API_KEY=sk-... ./gogen --web
OPENAI_API_KEY=sk-... ./gogen -p "List the top-level packages"
```

Non-loopback binds (e.g. `--host 0.0.0.0`) require a token. Without
`GOGEN_WEB_TOKEN`, one is auto-generated and persisted (`.gogen/web_token`,
0600) so **logins persist across restarts** — an already-paired device keeps
its session. Startup prints a short-lived pairing link, the pairing code in
plain text, and a QR code: the link opens the UI on this machine, the QR
(encoding the LAN address) logs a phone in on scan. The pairing code is
ephemeral by design — it expires after 15 minutes and is replaced at every
server start, so after any restart scan the freshly printed QR; the
long-lived token itself is never printed or logged. Set `GOGEN_WEB_TOKEN`
to use your own token:

```bash
GOGEN_WEB_TOKEN=secret ./gogen --web --host 0.0.0.0
# then open http://host:8081/?token=secret
```

Global mode (use `~/.config/gogen/` instead of project `.gogen/`):

```bash
GOGEN_MODE=global ./gogen
```

Build without tree-sitter (smaller binary, no syntax checks or symbol
extraction):

```bash
CGO_ENABLED=0 go build -o gogen-nocgo .
```

### Flags

| Flag | Description |
|------|-------------|
| `--web` | Run in web mode (listens on `:8081`) |
| `--host <host>` | Listen host for `--web` (e.g. `0.0.0.0`; falls back to `GOGEN_WEB_BIND` or `127.0.0.1:8081`). `GOGEN_WEB_LISTEN`, when set, is the address for this process only: it wins over `--host` and `web_bind`, and a settings save does not write it |
| `--dir <path>` | Set the working directory |
| `--global` | Ignore project `.gogen/`, use `~/.config/gogen/` instead |
| `--url <url>` | Override OpenAI API base URL (e.g., for local LLMs or proxies) |
| `--verbose` | Show full tool output in CLI mode |
| `-p <prompt>` | Run a single prompt and exit (non-interactive) |
| `--save-config` | Write effective config to `.gogen/gogen.conf` and guidelines to `.gogen/gogen.md` |
| `--save-config-secrets` | Include secrets when using `--save-config` (`openai_api_key`, `web_auth_token`, MCP `env`, provider keys) |
| `--save-config-path <file>` | Output path for `--save-config` config file (default `.gogen/gogen.conf`; project mode only — global mode always writes to `~/.config/gogen/config.yaml`) |

The first positional argument is also treated as the working directory (overridden by `--dir`); a second positional argument runs as a single prompt, equivalent to `-p`.

### TUI commands

While in the TUI:

| Command | Description |
|---------|-------------|
| `help` or `/help` | Show available commands |
| `exit`, `quit`, `/exit`, or `/quit` | Quit |
| `dir <path>` | Change working directory |
| `compact` or `/compact` | Manually compact conversation history |
| `/smartcompact` | Smart compact for a long tool loop: `on`, `off`, or bare to toggle. Applies to this run; `/save-config` stores it. Default off |
| `/models` | List available models |
| `/models <name>` | Switch to a different model |
| `/plan` | Enable plan mode (read-only) |
| `/act` | Enable act mode (full tools) |
| `/mode` | Show current mode |
| `/think` | Set thinking/reasoning level (`off`/`low`/`medium`/`high`) or show the current level |
| `/context` | Show context window usage (tokens used, limit, compact threshold) |
| `/new` | Start a fresh session; previous session is saved to disk |
| `/subagents` | Show the nested (subagent) sessions that ran in this session, with their final reports and status (when `subagent: on`); live children also appear as nested rows in the sessions panel |
| `/resume` | List saved sessions (with message count and label) |
| `/resume <id>` | Restore a saved session |
| `/resume latest` | Restore the most recent session other than the current one |
| `/resume del <id>` | Delete a saved session |
| `sessions` | Alias for `/resume` (list sessions) |
| `/fork` | Fork a new session from the last assistant message (`/fork <N>` from message N) |
| `/queue` (or `ctrl+q`) | Show the queued messages for the focused session; `d` removes the selected one, `D` clears the user's queued items |
| `/verbose` | Toggle verbose tool output |
| `/save-config` | Write effective config to `.gogen/gogen.conf` |

Type `/help` for the full list.

While a turn runs the composer stays editable: **Enter** queues the message
(queued items run in order when the turn ends — see the web UI's
**Steering** note; the same queue backs both hosts) and **ctrl+c**
interrupts, which also drops the user's own queued items.

## Supported languages

Tree-sitter is bundled for **26 languages** (syntax checking after edits):

Go, Python, JavaScript, TypeScript, TSX, Rust, Java, Kotlin, C, C++, C#, PHP, Ruby, Scala, SQL, HTML, CSS, JSON, Bash, YAML, TOML, Lua, HCL, Zig, Dockerfile, Make

**Symbol extraction** (`list_definitions`) has dedicated queries for **19** of
these: Go, Python, JavaScript, TypeScript, TSX, Rust, Java, Kotlin, C, C++,
C#, PHP, Ruby, Scala, SQL, Bash, Lua, HCL, Zig. JSON, HTML, CSS, YAML, TOML,
Dockerfile, and Make get syntax checks only.

Tree-sitter requires **CGO** at build time (enabled by default on Linux); set
`CGO_ENABLED=0` to build without it — tree-sitter features are then stubbed
out.

## Configuration

Settings load from **CLI flags**, **environment variables**, and **`.gogen/gogen.conf`** (pure YAML). Precedence: **env > CLI flags > .conf > defaults**.

### Project config (`.gogen/gogen.conf`)

YAML config

```yaml
command_safety: blocklist
command_approval: required   # required = ask before a blocklisted command; off = refuse it outright
openai_model: gpt-4o
mcp: on
mcp_tool_mode: nested  # nested (default) = mcp_list, mcp_describe, mcp_call; inline = one definition per server tool
mcp_servers:
  - name: fetch
    command: npx
    args: ["-y", "@modelcontextprotocol/server-fetch"]
    # init_timeout_secs: 60   # handshake bound (default 60); raise for a cold npx start
    # tool_timeout_secs: 600  # per-call default (default 600); raise for long-running tools
board: on          # project kanban board (board tool + web board tab)
subagent: on       # subagent tool (nested sessions)
subagent_max_depth: 2  # max nesting; 1 (default) = subagents cannot spawn subagents
subagent_max_concurrent: 4  # max subagents running at once per session (web); 4 (default)
subagent_model: gpt-4o-mini    # default subagent model (empty = inherit the parent's model)
subagent_thinking_level: high  # subagent reasoning effort (empty = inherit the parent's level)
review_agent: off  # auto-review: a ticket moved into in_review spawns a review session (web)
review_agent_model: gpt-4o-mini  # default review model (empty = ticket override, else workspace default)
review_agent_thinking_level: high  # review agent reasoning effort (empty = inherit the workspace level)
job_notices: off   # notify the session when a background command finishes
automations: off   # file-based cron scheduler: fire saved prompts as headless runs on a schedule
status_display: auto  # off | auto (probe for the screen at startup) | on
status_display_host: statusscreen.local  # mDNS name, IP, or base URL of the display
status_display_token: ""                 # only if the display has a shared secret set
status_display_source: ""                # display row name; empty = auto (gogen-<project folder>)
skills: off        # skill tool (list/read over .gogen/skills + ~/.config/gogen/skills)
terminal: off      # persistent terminal tool (open/send/read/signal/list/close actions) + bash_persistent
agent_instructions: off  # load AGENTS.md / CLAUDE.md workspace instruction files
web_bind: 0.0.0.0:8080  # web listen address (applies on next start; also GOGEN_WEB_BIND / --host)
# Configurable prompt templates ("" = the built-in default):
board_start_prompt: "You have been assigned board ticket #{id}: {title}..."  # board "Start agent" prompt
board_review_prompt: "You are the review agent for board ticket #{id}..."   # auto review agent prompt
system_prompt: "You are a coding agent in {working_dir}..."                  # replaces the base system prompt
subagent_prompt: "You are a subagent...\n\nJob:\n{job}"                      # wraps subagent jobs
```

Config values follow a strict schema: unknown keys are ignored, `on`/`off`
settings are strings, booleans are `true`/`false`, list settings are
comma-separated strings (e.g. `command_allowlist: "go,git"`,
`treesitter_langs: "go,python"`), integers are YAML integers, and `mcp_servers`
is a YAML list of objects (`name`, `command`, `args`, `env`, and the optional
`init_timeout_secs` / `tool_timeout_secs`). An empty or zero
value means "use the default", with these exceptions where `0` is a real
setting:

| Key | `0` means |
|-----|-----------|
| `compact_threshold` | auto-compaction disabled (never compact automatically; manual `/compact` still works) |
| `compact_keep_recent_messages` | keep nothing recent: only the first user message survives compaction, everything after it is summarized |
| `max_tool_result_bytes` | no truncation cap on tool output |
| `compact_reserve_tokens` | reserve no tokens for new messages after compaction |

Negative values are invalid for these settings and fall back to defaults
(`session_max_age_days` is the exception — a negative value keeps sessions
forever). `compact_threshold` must be in `[0, 1]` when set (including the
explicit `0` that disables auto-compaction); values outside that range fall
back to `0.85`.
Note that disabling auto-compaction means the full conversation is sent to the
model each turn, so it will eventually exceed the model's context window.

The legacy `keep_recent_messages` key (and `GOGEN_KEEP_RECENT_MESSAGES` env
var) is still accepted and mapped onto `compact_keep_recent_messages` with a
deprecation warning, so existing project files keep working after the rename.

`--save-config` writes pure YAML (no `---` front-matter markers); `on`/`off`
values may be emitted quoted, which parses identically. Secrets
(`openai_api_key`, `web_auth_token`, MCP server `env`, provider `api_key`) are
omitted unless `--save-config-secrets` is passed, and options equal to their
built-in default are omitted entirely, so a saved config never bakes in a
default.

Snapshot effective settings:

```bash
./gogen --save-config --dir /path/to/project
```

### Project guidelines (`.gogen/gogen.md` or `GOGEN.md`)

Markdown files for agent instructions and rules:

```markdown
# Project guidelines

- Run `make test` before finishing Go changes. Use `make test-debug` (or `make check`) when touching debug-tagged packages (`internal/agent` view-drift, `internal/profiling`).
- Never modify files in vendor/.
```

Discovery order:
- Config: `.gogen/gogen.conf` → `GOGEN.conf` → front matter in `.gogen/gogen.md`/`GOGEN.md` (fallback)
- Guidelines: `.gogen/gogen.md` → `GOGEN.md` → `.gogen/rules.md` → `.cursor/rules/gogen.md`

Files without config (plain markdown) are treated as guidelines-only. The old combined format (`---` YAML front matter + body) still works as a fallback but `--save-config` now writes separate `.conf` and `.md` files.

### API and workspace

| Variable | Default | Description |
|----------|---------|-------------|
| `OPENAI_API_KEY` | *(required)* | API key for an OpenAI-compatible endpoint |
| `OPENAI_MODEL` | *(empty)* | Model to use; leave empty to use the endpoint's default model or pick one with `/models` |
| `OPENAI_BASE_URL` | *(empty)* | API base URL (e.g. `https://api.openai.com/v1` or a local proxy) |
| `GOGEN_WORKING_DIR` | `.` | Default working directory |

### Multiple OpenAI-compatible providers

Beyond the default profile (`OPENAI_API_KEY` / `OPENAI_BASE_URL` /
`OPENAI_MODEL`), extra OpenAI-compatible endpoints can be registered in
`.gogen/gogen.conf`:

```yaml
openai_providers:
  - name: local
    base_url: http://localhost:11434/v1
    model: llama3.1
    api_key: ""   # endpoints that need none
```

or via the `GOGEN_OPENAI_PROVIDERS` env var (a JSON array of `{name, baseUrl,
apiKey, model}`, overriding the file). Models from all providers are
aggregated into the web model picker and routed to their owning endpoint; an
empty `base_url` means the official OpenAI endpoint. Provider `api_key`
values are never persisted by `--save-config` unless `--save-config-secrets`
is passed.

### Context management

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_CONTEXT_LIMIT` | `0` | Manual token limit override (`0` = resolve from model) |
| `GOGEN_COMPACT_THRESHOLD` | `0.85` | Fraction of context limit that triggers auto-compaction (the UI warns earlier, at 75%) |
| `GOGEN_COMPACT_KEEP_RECENT_MESSAGES` | `12` | Most recent messages kept verbatim when a compaction runs (older middle history is summarized; `0` keeps only the first user message) |
| `GOGEN_MAX_TOOL_RESULT_BYTES` | `262144` | Max bytes for tool output before truncation (matches web_fetch's 256 KB limit) |
| `GOGEN_OUTPUT_SPILL` | `on` | Spill oversized tool output to `.gogen/spill/` (session-scoped; global mode: `~/.local/share/gogen/spill/`) so the inline result is a head/tail preview + locator to the saved file, instead of a lossy head-only truncation (`on`/`off`) |
| `GOGEN_COMPACT_RESERVE_TOKENS` | `4000` | Tokens reserved for new messages after compaction |
| `GOGEN_COMPACT_LAST_RESORT` | `condense` | `condense` or `error`: what happens when a single message cannot fit the context window even after all compaction (e.g. a fresh session whose first message is bigger than the window) |
| `GOGEN_SMART_COMPACT` | `off` | `on` or `off` (`smart_compact` in `.gogen/gogen.conf`). When on, a tool loop that crosses the compact threshold is compacted before the next tool runs, and a thinking stream stops with room left to summarize. A tool call the model has already written runs once, after the summary. Also on the web Context tab and via `/smartcompact` |

When a single message is too large for the window, there is no middle history
to summarize — the message is the head and every request would be refused.
The last-resort condensation handles this: with `compact_last_resort: condense`
(the default) the message is condensed in place via the summarizer, the
original is archived to the session's archive sidecar
(`<session>.archive.jsonl` next to the session file), and the condensation is
announced in-band (TUI system line / web banner: message size vs window,
original archived). With `compact_last_resort: error` the request is not sent
at all: a clear diagnostic ("message is ~N tokens vs M window; shorten it or
start a fresh session (/new)") is returned instead of the raw provider
refusal. The path is strictly last-resort — it only fires when the request is
provably over the window after the forced compaction, and only when condensing
the single largest message would bring it under.

Smart compact stays off unless you turn it on, so the usual path is unchanged:
auto-compaction runs when the latest message is from you. Turn it on
(`smart_compact: on`, `GOGEN_SMART_COMPACT=on`, the Context tab, or
`/smartcompact on`) when a model spends a long run thinking and calling tools
without coming back. Once the conversation crosses the compact threshold, GoGen
summarizes before the next tool runs, then executes the tool call the model
already wrote, once. If the thinking stream itself nears the window, it stops
at the compact reserve plus 4,000 tokens, which leaves room for the summary. A
complete tool call still runs after that summary. A cut in the middle of the
arguments is dropped, and the model continues from the summary and a short
excerpt. `compact_threshold: 0` still means "never compact automatically": it
closes the budget tier and the window-reserve tier alike. The mid-think stop
still uses the window, but nothing is summarized, so an unfinished tool call is
dropped and the turn ends with a note to run `/compact`, raise
`compact_threshold`, or start a new session. A complete tool call still runs.

After each agent turn, GoGen shows context usage in the CLI (dim line) and web UI (sidebar meter). Use `/context` for a detailed breakdown. When the provider returns usage stats (`prompt_tokens` from the API, including streaming with `include_usage`), the display labels this as **last request**; otherwise it falls back to a local token estimate.

### Safety

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_COMMAND_SAFETY` | `blocklist` | `blocklist`, `allowlist`, or `off` |
| `GOGEN_COMMAND_ALLOWLIST` | *(empty)* | Comma-separated allowed command prefixes (allowlist mode) |
| `GOGEN_DELETE_APPROVAL` | `required` | Set to `off` to skip delete confirmation |
| `GOGEN_COMMAND_APPROVAL` | `required` | What a blocklisted command does: `required` asks for approval (and refuses it when nobody can be asked), `off` refuses it without prompting. Never allows a blocked command — that is `GOGEN_COMMAND_SAFETY=off` |

### Command execution

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_COMMAND_SANDBOX` | `off` | Sandbox mode: `off` or `bwrap` (bubblewrap when available) |
| `GOGEN_COMMAND_IDLE_TIMEOUT_SECS` | `300` | Foreground `execute_command` is killed after this many seconds without output (any output resets the window; background jobs are unaffected); also `command_idle_timeout_secs` in `.gogen/gogen.conf` |

The timeout is an **idle (no-output) timeout**, not a wall-clock cap: the
window is reset by any output the command produces, so a long-running command
that keeps printing is never killed. The legacy `command_timeout_secs` key (and
`GOGEN_COMMAND_TIMEOUT_SECS` env var) is still accepted and mapped onto
`command_idle_timeout_secs` with a deprecation warning; the renamed key/env
var win when both are present. Note the semantics change: the old value capped
total runtime, so an existing `command_timeout_secs: 600` now tolerates 600s
of silence rather than 600s total.

### Global mode

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_MODE` | *(empty)* | Set to `global` to use `~/.config/gogen/` instead of project `.gogen/` |

### Tree-sitter

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_TREESITTER` | *(on)* | Set to `off` to disable syntax checks and symbol extraction |
| `GOGEN_TREESITTER_LANGS` | *(all)* | Comma-separated subset, e.g. `go,python,rust` |
| `GOGEN_DEBUG_COMPARE_MESSAGES` | off | (debug builds only) Enable view-fingerprint comparison across turns |

### Project commands

| Config key | Description |
|------------|-------------|
| `test_command` | Override the test command auto-detection (e.g. `"make test"`) |
| `lint_command` | Override the lint command auto-detection (e.g. `"make vet"`) |

These can be set in `.gogen/gogen.conf` only — there is no CLI flag or environment variable equivalent.

### MCP

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_MCP` | off | Set to `on` to enable MCP (also set `mcp: on` in project config) |
| `GOGEN_MCP_TOOL_MODE` | `nested` | `nested` exposes every MCP tool through `mcp_list`, `mcp_describe`, and `mcp_call`; `inline` inlines one definition per server tool (also set `mcp_tool_mode` in project config) |
| `GOGEN_MCP_SERVERS` | *(empty)* | JSON array of `{name, command, args, env, init_timeout_secs, tool_timeout_secs}` (overrides file) |
| `GOGEN_MCP_EXTRA` | *(empty)* | JSON array in the same shape, appended for this process only. A host uses it for its own server (the VS Code editor bridge). It is not saved. `mcp` off does not start it. A name that is already configured is skipped. |

Each `mcp_servers` entry may also set `init_timeout_secs` (handshake bound,
default 60s) and `tool_timeout_secs` (default deadline for one `tools/call`,
default 600s); `0` keeps the default. Both exist for servers whose launcher or
tools are slow: a cold `npx -y <package>` start and a tool that runs a native
analysis engine take minutes, not seconds. A turn you cancel still aborts the
call immediately. Tool catalog pagination (`nextCursor`) is followed, and a
result carrying images, resources, or only `structuredContent` reaches the
model as a described placeholder instead of an empty tool result.

Servers start in the background, and GoGen installs one shared registry
before that handshake. The first agent, web chats opened while a server is
still connecting, and subagents all use that same registry, so a chat created
during startup still sees the tools when the handshake finishes. In nested
mode, `mcp_list`, `mcp_describe`, and `mcp_call` wait for the handshake
instead of failing on the first try. The wait is bounded by that server's
`init_timeout_secs`. The web settings connection test uses the same budget
when `init_timeout_secs` is longer than its 90s floor. Set `tool_timeout_secs`
at least as high as the server's own startup budget (for example a Ghidra
launch configured for 50 minutes) so GoGen does not end the call first.

#### Tool exposure: nested (default) or inline

MCP servers publish their own tool catalogs; inlining them adds one definition
per server tool to every request. `mcp_tool_mode` controls that:

- **`nested`** (default) — the model sees three fixed tools and no server
  catalog. `mcp_list` returns names and one-line summaries (optional `query`).
  `mcp_describe` returns one tool's argument schema (`tool`). `mcp_call` runs
  one tool: `tool` is the name, and `input` is only that tool's own parameters
  (`{"tool":"mcp_rea_search_strings","input":{"pattern":"demo"}}`). There is
  no `action` parameter. The tool list stays a fixed size no matter how many
  servers connect, and the names/schemas are only materialized when the model
  asks for them. A call made while the handshake is still running waits for
  it, up to `init_timeout_secs`, and then lists or runs the tools. The three
  tools are act-mode only, matching the plan-mode rule for MCP tools.
- **`inline`** — every discovered `mcp_<server>_<tool>` is added to the tool
  list with its full schema, the historical behavior. Better when a server
  contributes only one or two tools and you would rather not pay the discovery
  step.

Nested mode also keeps the request cacheable. Provider prompt caching matches a
byte-exact request prefix, and tool definitions plus the system prompt lead that
prefix — so adding a definition (or letting one appear when a server finishes
its handshake) invalidates the whole conversation. GoGen therefore decides the
mode from **config**, before the first request, and never from the live
registry: a slow `npx -y <package>` server connecting after turn 1 leaves the
tool list and system prompt byte-identical. Inline mode has no such guarantee —
its definitions are the server's and can only appear once it has answered.

### Reverse engineering with REA

[REA](https://github.com/morluto/rea) is an MCP server for reverse engineering:
it inspects native binaries, JavaScript/Electron apps, .NET assemblies, Android
APKs, firmware, and websites, and returns pseudocode, assembly, strings,
metadata and the evidence behind each finding. Deep native analysis uses your
own Hopper, Ghidra or IDA install (static JavaScript/.NET inspection needs no
engine).

GoGen runs REA as a normal stdio MCP server (`npx -y rea-agents mcp` is the
same command REA publishes for manual registration), so nothing is generated by
REA's own setup step — add the entry to `.gogen/gogen.conf`:

```yaml
mcp: on
mcp_servers:
  - name: rea
    command: npx
    args: ["-y", "rea-agents", "mcp"]
    # A cold npx download can outlast the 60s handshake bound.
    init_timeout_secs: 120
    # Provider (Ghidra/Hopper/IDA) startup plus indexing is slow; raise this
    # if a call times out on a large binary.
    tool_timeout_secs: 1800
```

Requirements are REA's: Node.js 22.x (>=22.23.4), 24.x (>=24.22.1), or 26.12.1+,
plus npm. Then ask for the analysis in plain English. REA publishes a tool per
analysis family, so the default `mcp_tool_mode: nested` keeps the model's tool
list at a fixed size: the agent discovers what it needs with `mcp_list` and
runs it with `mcp_call` (the gateway resolves `mcp_rea_<tool>` and `<tool>` alike).
Set `mcp_tool_mode: inline` if you would rather have every REA tool inlined as
its own `mcp_rea_<tool>` definition.

```
> Use rea to work out how this binary computes the sound pan value
> Inspect app.asar with rea and explain how the clipboard bridge works
```

Useful knobs on the REA side (set them in the entry's `env`, which is passed to
the server process):

| Variable | Description |
|----------|-------------|
| `REA_MCP_INPUT_SCHEMA_PROFILE` | `compact` (default) keeps tool schemas small in the request; `full` advertises the complete field guidance |
| `REA_GHIDRA_STARTUP_TIMEOUT_MS` / `REA_HOPPER_STARTUP_TIMEOUT_MS` | Raise the provider's own startup deadline for large binaries (this is separate from `tool_timeout_secs`, which bounds the call from GoGen's side) |

### Agent features

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_BOARD` | off | Set to `on` to enable the project kanban board (board tool + web board tab; live-toggleable from the web settings modal) |
| `GOGEN_SUBAGENT` | off | Set to `on` to enable the subagent tool (nested sessions) |
| `GOGEN_SUBAGENT_MAX_DEPTH` | `1` | Maximum subagent nesting depth (main agent = depth 0); `1` = subagents cannot spawn subagents; values ≤ 0 fall back to the default |
| `GOGEN_SUBAGENT_MAX_CONCURRENT` | `4` | Maximum subagents running concurrently per session (web and TUI); spawning beyond it is refused. Interrupted (idle) subagents do not hold a slot; values ≤ 0 fall back to the default |
| `GOGEN_SUBAGENT_MODEL` | *(empty)* | Default model for spawned subagents (empty = inherit the parent's model; the tool's explicit `model` argument always wins) |
| `GOGEN_SUBAGENT_THINKING_LEVEL` | *(empty)* | Reasoning-effort level for spawned subagents (empty = inherit the parent session's live level; `off` = never send `reasoning_effort`). A level the subagent's final model does not accept is omitted at spawn time |
| `GOGEN_BOARD_START_PROMPT` | *(empty)* | Template for the agent started from a board ticket (`{id}` `{title}` `{description}` `{priority}` `{context}`; empty = built-in default) |
| `GOGEN_REVIEW_AGENT` | off | Set to `on` to enable the board auto-review agent (web mode): a ticket moved into in_review — by an agent's board tool or by the user — automatically starts a headless review session that marks the ticket done or comments findings and moves it back to in_progress. Max 5 review rounds per ticket |
| `GOGEN_REVIEW_AGENT_MODEL` | *(empty)* | Default model for auto review sessions (empty = the ticket's own review model override, else the workspace default model) |
| `GOGEN_REVIEW_AGENT_THINKING_LEVEL` | *(empty)* | Reasoning-effort level for auto review sessions (empty = inherit the workspace level; a ticket's own review level override wins; a level the reviewer's final model does not accept is omitted) |
| `GOGEN_BOARD_REVIEW_PROMPT` | *(empty)* | Template for the auto review agent (`{id}` `{title}` `{description}` `{priority}` `{context}` `{assignee}`; empty = built-in default) |
| `GOGEN_SYSTEM_PROMPT` | *(empty)* | Custom system prompt template (`{working_dir}`; replaces the built-in base prompt; the MCP hint when MCP is enabled, the auto-detected project profile, the project rules, and plan mode are still appended; empty = built-in default) |
| `GOGEN_SUBAGENT_PROMPT` | *(empty)* | Template wrapping subagent jobs (`{job}`; empty = built-in default) |
| `GOGEN_JOB_NOTICES` | off | Set to `on` to notify the session when a background command finishes (injects a summary + runs a turn) |
| `GOGEN_SKILLS` | off | Set to `on` to enable the skill tool (`skill` list/read over `.gogen/skills` + `~/.config/gogen/skills`) |
| `GOGEN_TERMINAL` | off | Set to `on` to enable the persistent-terminal feature tools: `terminal` (actions `open`/`send`/`read`/`signal`/`list`/`close` over session-scoped PTY sessions) and `bash_persistent` (a per-session shell whose cwd/env persist across calls; it bypasses the fresh-process model of `execute_command`). The command guard applies to `bash_persistent`; the PTY `send` action writes raw stdin, so the guard does not apply to it |
| `GOGEN_AGENT_INSTRUCTIONS` | off | Set to `on` to load `AGENTS.md` / `CLAUDE.md` workspace instruction files (below the project guidelines) |
| `GOGEN_STATUS_DISPLAY` | `auto` | Status-screen gate: `off`, `auto` (probe for the screen at startup and enable reporting when it answers), `on` (report without probing), or `browser` (the web UI sends; web mode only) — see [Status display](#status-display-lan-status-screen); live-toggleable from the web settings modal |
| `GOGEN_STATUS_DISPLAY_HOST` | `statusscreen.local` | Display host: mDNS name, IP, or base URL |
| `GOGEN_STATUS_DISPLAY_TOKEN` | *(empty)* | Shared secret for the display's mutating endpoints (sent as `X-Status-Token`) |
| `GOGEN_STATUS_DISPLAY_SOURCE` | *(auto)* | Display row (feed) name; empty derives `gogen-<project folder>` so instances in different projects get their own row |

### Status display (LAN status screen)

With `status_display: auto` (the default), GoGen probes for the display at
startup — a background `GET /info` with a short timeout, so startup never
waits on the network — and enables reporting when the ttgo/status firmware
answers. An absent display costs nothing and stays silent; `on` reports
without probing, `off` disables the feature entirely.

Once active, GoGen pushes live turn status over plain HTTP — no extra
dependency, and updates are best-effort: a slow or unreachable display never
delays a turn, and losing an update costs nothing because every update is a
full snapshot. One row per top-level turn (nested subagent turns are ignored):

- **progress** — the current context-window usage, the same figure as the web
  sidebar's context meter
- **state** — `running` while a turn is in flight, `done` / `error` on the
  outcome, `idle` when the turn was cancelled
- **title** — `Gogen@<project>: <session label>`, drawn on the display's first
  label line (the firmware's two-line mode) and trimmed to the bar width; the
  project tag is how concurrent instances are told apart on the panel
- **message** — the current activity (`thinking...`, the running tool's name,
  `compacting...`, `waiting...`), the turn's elapsed time and the context
  usage (`thinking... 1m42s 368k/1M`), on the second label line. While a turn
  runs the row is refreshed every few seconds so the elapsed time visibly
  ticks — a long model prefill shows a live screen instead of a frozen line;
  a clean finish reads `done 3m12s 368k/1M`

All text sent to the display is printable ASCII: the firmware's built-in
fonts have no glyphs above `0x7E`, so non-ASCII characters (including the
session label's) are replaced with `?` instead of drawing as junk glyphs.

Reporter diagnostics (screen found at startup, send failures and recovery)
are written to the **debug log** when one is configured (`debug_log` /
`GOGEN_DEBUG_LOG`) and are silent otherwise — never to stderr, because a raw
line while the TUI owns the terminal desyncs its inline renderer.

**Browser mode** (`status_display: browser`, web mode only) makes the web UI
a fallback sender for when the GoGen server cannot reach the display's network
but your browser can. The server probes for the display first: when the server
itself can reach it, the server sends directly — the reliable path, no browser
tab needed and a single writer. Only when the server cannot does it build the
rows and relay them over the chat socket, and the web UI performs the
POST/DELETE itself. The display token is pushed to the web UI in this mode
(the browser may be the sender) and travels as a `?token=` query parameter,
because the firmware's CORS preflight only allows the `Content-Type` header.
Every attached tab relays the rows (duplicates are harmless). In the TUI/CLI
there is no browser to fall back to, so the gate probes server-side and
reports directly when the display answers.

With several sessions running at once (web panes / TUI sessions), they share
the row — the most recent activity wins. Separate Gogen **instances** are kept
apart automatically: an empty `status_display_source` derives the row name
from the project folder (`gogen-<folder>`), so instances in different projects
get their own row (the display shows up to 8 rows, 5 visible). Two instances
sharing one project folder need an explicit `status_display_source` each
(e.g. `gogen-tui` / `gogen-web`).

The row clears when the feature is turned off or GoGen exits; otherwise it
expires after its TTL (15 min while running, 5 min for an outcome). The
default host is the firmware's mDNS name, `statusscreen.local`; set
`status_display_host` to an IP when `.local` does not resolve on the machine
running GoGen (for example a `CGO_ENABLED=0` build on a host without a system
mDNS resolver). Set `status_display_token` when the display has a shared
secret configured (`/token?set=…` on the device). In web mode all of these
are live-editable in **Settings → Tools**.

### Session persistence

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_SESSION_PERSIST` | on | Set to `off` to disable session save/resume |
| `GOGEN_SESSION_MAX_COUNT` | `50` | Max saved sessions per working directory |
| `GOGEN_SESSION_MAX_AGE_DAYS` | `30` | Auto-delete sessions older than N days (negative = keep forever) |

`GOGEN_SESSION_MAX_COUNT` and `GOGEN_SESSION_MAX_AGE_DAYS` can also be set in `.gogen/gogen.conf` (`session_max_count`, `session_max_age_days`); env vars take precedence.

### Sessions and debug

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_CLI_VERBOSE` | off | Verbose tool output in CLI |
| `GOGEN_DEBUG_LOG` | *(empty)* | Path to JSON debug log |
| `GOGEN_DEBUG_SESSION` | *(empty)* | Session id in debug logs |

### Web server

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_WEB_BIND` | `127.0.0.1:8081` | Listen address for `--web` (e.g. `0.0.0.0:8080` to accept remote connections); also `web_bind` in `.gogen/gogen.conf` (the settings modal's web-bind field persists it for the next start) |
| `GOGEN_WEB_LISTEN` | *(unset)* | Listen address for this process only. When set, it wins over `--host` and `web_bind`, and a settings save does not write it. The VS Code sidebar uses it for the window's free port |
| `GOGEN_WEB_TOKEN` | *(empty)* | Required for non-loopback binds; auto-generated and persisted (`.gogen/web_token`) when `--web --host` is used without one — startup prints a short-lived pairing link + QR; also `web_auth_token` in `.gogen/gogen.conf` |
| `GOGEN_WEB_ALLOWED_ORIGINS` | *(empty)* | Comma-separated host allowlist for WebSocket CORS; empty uses localhost defaults; also `web_allowed_origins` in `.gogen/gogen.conf` |
| `GOGEN_WEB_TLS_CERT` | *(empty)* | Path to PEM certificate file for TLS; also `web_tls_cert_file` in `.gogen/gogen.conf` |
| `GOGEN_WEB_TLS_KEY` | *(empty)* | Path to PEM key file for TLS; also `web_tls_key_file` in `.gogen/gogen.conf` |
| `GOGEN_WEB_MAX_ACTIVE_SESSIONS` | `8` | Cap on concurrently active sessions (panes); also `web_max_active_sessions` in `.gogen/gogen.conf` |
| `GOGEN_WEB_APPROVAL_HOLD_SECS` | `0` | Keep pending delete approvals alive for N seconds after the last client detaches (a reconnecting client is re-notified and can answer); `0` auto-denies immediately on detach. Also `web_approval_hold_secs` in `.gogen/gogen.conf` |

Non-loopback binds without TLS log a warning: the auth token is sent in plain
text. Set `GOGEN_WEB_TLS_CERT` / `GOGEN_WEB_TLS_KEY` (or the file keys) to
enable TLS.

### Web UI

The web UI supports **multiple chat panes** on one page. Each pane is an
independent session with its own history, mode, thinking level, model, and
in-flight turn — changing the model in one pane never affects another, and a
pane's model is remembered when the session is resumed later. Because the
panes share one workspace, the working directory is a single server-wide
setting: it can only be changed in global mode (`gogen --global`); in project
mode it is fixed to the project directory and the input is hidden.

- The sidebar is ONE list of sessions ordered by each session's last output
  (most recent first). Whether a session is focused, open, or responding
  shows as the row's status dot/highlight — never as a different position:
  focusing or activating a session does not reorder the list, only new
  output does. **New** opens a fresh pane alongside the current one (which
  keeps streaming); clicking an open pane focuses it, and **✕** closes it —
  any
  in-flight turn is cancelled and the session's live runtime is released, but
  it stays saved and can be resumed later. Deleting a saved session (✕, with
  confirmation) removes it permanently. Typed `/new`, `/resume <id>`, and
  `/fork N` replace the *current* pane's session — a `/resume` of a session
  already open as a pane focuses that pane instead.
- **Background panes keep running**: a turn in a non-focused pane continues
  server-side (amber "responding" indicator in the sidebar) and notifies once
  when it starts. Focus the pane to see it.
- **Disconnect ≠ cancel**: closing the tab or losing the connection does not
  stop the current turn — it completes server-side and is saved. When you
  reconnect, open panes re-attach automatically and show "Resuming…" while a
  still-running turn finishes. The **Cancel** button (or `Esc` while
  streaming) is the only way to stop a turn, and it works even from a fresh
  connection.
- **Steering**: the composer stays editable while a turn runs and typing a
  message QUEUES it (FIFO, one item per turn) instead of interrupting — it is
  delivered as the next user turn when the in-flight one ends, on the session
  it was typed into. The queued bubble shows a **queued** chip whose ✕ removes
  that one item; the input bar counts them ("· N queued") and **Cancel**
  clears the user's own queued items (machine notices keep draining — they
  are one-way deliveries and a run of them drains as one batched turn, so a
  burst of job/child completions costs one request). Command
  words (`/new`, `help`, `plan`, …) and image-only messages: commands keep
  taking the turn lock, images queue like chat. At most 20 user messages may
  wait per session — beyond that the send is refused with a visible error
  rather than dropped.
- **Idle sessions release themselves**: a session whose last tab closes while
  no turn is running is dropped from memory (it stays saved). The violet
  "resume to continue" indicator only appears for sessions that are genuinely
  live — open in another tab, or with a turn still running server-side.
- A built-in **Terminal** tab provides an interactive user shell (one per
  connection, respawnable after it exits) plus per-command terminal tabs that
  stream `execute_command` output live, with running/exit status dots and a
  restart button. The editor runs on its own socket, so saving a file is never
  blocked by a streaming turn.
- The settings modal is organized into sidebar sections (Editor, Chat, Global,
  Agent, Security, Context, Tools, Sessions, Server, MCP, Providers); opening
  it auto-selects the section matching the screen you are on, and manual
  choices are remembered per browser.

#### Board and subagents

- The settings modal's **Agent** group toggles the **Project board** and
  **Subagents** features live, no restart. The toggles are server-backed:
  they persist to `.gogen/gogen.conf` and every tab stays in sync via the
  config push.
- With `board: on`, a **Board** tab appears next to Chat/Editor: a kanban
  view (backlog, ready, in_progress, in_review, blocked, done) with
  drag-and-drop moves, column widths you can drag to resize (persisted per
  browser), a "New card" form (title, acceptance criteria, priority), and
  inline card detail with an activity log. Clicking a card expands inline
  actions: **Edit** (turns the title/description/priority/context into
  fields in place, with Save/Cancel), **Comment**, **Block** (with a
  required reason), and **Done**. The **Context** field is free-text
  background for the started agent, merged into the prompt's `{context}`
  placeholder ahead of the activity log. Blocked cards show their latest
  reason, done cards are struck through, and a card under review shows a
  **review** badge. Agent board-tool mutations re-render the board live; the
  tab is hidden while the feature is off. Each card has a **▶ Start** button
  that opens a small popover with a
  per-ticket model picker, a model-aware **reasoning effort** picker
  (**Inherit** = the active pane's live level, **Off**, or the model's
  accepted values), and an editable prompt — then claims the ticket and
  starts a dedicated agent session seeded with it; the button becomes
  **Open agent** and switches to the chat tab with the session attached.
  The started session runs headless until then — it survives closing the
  tab, and its delete approvals pop the approval modal right on the board
  (if the initiating tab is closed, approvals are denied until the session
  is reopened). The choices are stored on the ticket so the popover
  pre-fills on the next start; the start prompt template is editable in
  the settings modal (`board_start_prompt`). The popover's collapsible
  **Review agent** section sets per-ticket review options (model +
  reasoning effort) used by the auto-review agent on this ticket.
- With `review_agent: on` (web mode; the **Board review agent** toggle in
  the settings Agent group), a ticket moved into **in_review** — by an
  agent's board tool or by drag-drop — automatically starts a headless
  review session (`review #N: title` in the sidebar). It verifies the
  work (diff, tests, ticket criteria) and marks the ticket done, or
  comments findings and moves it back to in_progress. One reviewer runs
  per ticket at a time; auto-review stops after 5 review rounds per
  ticket (commented on the card). The reviewer's default model and
  reasoning effort are set in the settings modal
  (`review_agent_model`, `review_agent_thinking_level`), its prompt via
  `board_review_prompt`; a ticket's own **Review agent** popover section
  overrides them.
- With `subagent: on`, subagents appear as **nested rows** under their parent
  session in the sidebar. A colored dot marks a live child — amber
  **responding** while running, red **failed** on error, green **done** on
  success — whether it is open as a pane here or live in another tab; a
  settled child that is neither open nor live shows a muted text status
  instead of a dot. Clicking a
  nested row opens the child as a normal pane (live transcript, Cancel, ✕),
  which is the escape hatch for a subagent stuck in a loop. The Agent tab's
  **Default subagent model** picker and the **System prompt** / **Subagent
  prompt** textareas persist to `.gogen/gogen.conf` and apply to both web and
  TUI subagents (`subagent_model`, `system_prompt`, `subagent_prompt`). The
  prompt fields are pre-populated with the effective templates and a "Reset to
  default" button restores the built-in; a value equal to the built-in default
  is treated as unset, so the default text is never baked into the config
  file.
- **Both frontends run the same subagent lifecycle** (shared
  `internal/subagents`): `run_in_background` and the control tools
  (`subagent_fork`, `list_agents`, `get_agent_result`, `wait_agent`,
  `send_message`, `interrupt_agent`) exist in the web UI and the TUI alike.
  A background child keeps running after the spawning tool call returns, is
  listed and steerable from a later turn, and is cancelled with its parent
  session (and on quit). The TUI hosts a child as a live pane in the same
  session list: its row nests under the parent and opens into a normal
  transcript (streaming, cancel, join mid-turn).
- **A child has one logical return path** (like the other agent harnesses):
  the run's final message — the `subagent` tool result for a foreground child,
  or a compact terminal event for a background one — plus one exact reply per
  `send_message`. Background parents use `wait_agent` to block for the main run
  and `get_agent_result` to pull its authoritative bounded report; the report
  remains available from the persisted child after the live retention window.
  Children never push interim progress into the parent conversation as turns;
  the parent checks `list_agents` (which lists each child's latest progress
  line) and the user watches the child pane.
- **Max concurrent subagents** (`subagent_max_concurrent`, default `4`): caps
  how many subagents may run at the same time for one session, in both the web
  UI and the TUI. The Agent tab's numeric input live-toggles it like the depth
  field; spawning beyond the cap is refused with an error the agent can act on
  (`interrupt_agent` a running child or wait for one to finish). An
  interrupted (idle) child no longer holds a slot; a separate internal guard
  bounds non-finished children so a spawn+interrupt loop cannot accumulate
  runtimes.
- **Subagent reasoning effort** (`subagent_thinking_level`): the Agent tab's
  **Subagent reasoning effort** picker sits next to the model picker and is
  model-aware — its options are the selected subagent model's accepted
  values (the active session's model values while the model is "Inherit").
  The leading **Inherit** option (the default, empty value) makes spawned
  subagents run with the parent session's live thinking level; **Off** never
  sends `reasoning_effort`. A level the subagent's final model does not
  accept is omitted at spawn time (the tool's explicit `model` argument can
  override the configured model, so validity is resolved against the child's
  model, never at save time). `subagent_fork` always inherits the parent's
  level.

### Web tools (web_fetch / web_search)

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_WEB_FETCH` | on | Enable `web_fetch` tool (`on`/`off`) |
| `GOGEN_WEB_SEARCH` | on | Enable `web_search` tool (`on`/`off`) |
| `GOGEN_WEB_FETCH_MODE` | `https` | `https` (default) or `all` (allow HTTP) |
| `GOGEN_WEB_SEARCH_BACKEND` | *(empty)* | `brave` for Brave Search API; empty uses DuckDuckGo |
| `GOGEN_WEB_SEARCH_API_KEY` | *(empty)* | API key for Brave Web Search |
| `GOGEN_WEB_ALLOWED_DOMAINS` | *(empty)* | Comma-separated domain suffix allowlist for fetch |

### API compatibility

| Variable | Default | Description |
|----------|---------|-------------|
| `GOGEN_PRESERVE_REASONING` | `auto` | Controls `preserve_reasoning` for llama.cpp endpoints: `auto`, `on`, or `off` |
| `GOGEN_STREAM_IDLE_TIMEOUT` | `30m` | Streaming responses are killed after this long without receiving any bytes (Go duration, e.g. `45m`). The window also covers waiting for the **first** token: long prompt processing (llama.cpp re-processes the whole prompt each round) and queued requests on a busy backend send no bytes until generation starts. `0` or `off` waits indefinitely |
| `GOGEN_STREAM_RETRY_BACKOFF` | `1s` | Delay before the stream recovery ladder re-requests after a failed stream attempt; doubles per stage (`1s` before the muted streaming retry, `2s` before the non-streaming fallback). An immediate re-request hammers an endpoint that is often busy — the very condition that broke the stream. `0` or `off` retries immediately |
| `GOGEN_STREAM_STALL` | `10s` | How long a streaming round may see no SSE chunk before the UI switches from "thinking" to a "still waiting on model" label (long prefill or a stalled upstream; informational only — it never interrupts the stream). `0` or `off` disables the signal |
| `GOGEN_STREAM_DRAIN_GRACE` | `5s` | How long to wait for the promised usage chunk (`stream_options.include_usage`) AFTER the round's finish_reason before treating the round as complete; an endpoint that finishes without usage and holds the connection open would otherwise park the turn until the read idle timeout. `0` or `off` waits for the read idle timeout |
| `GOGEN_REASONING_ECHO` | `on` | Whether historical assistant reasoning is re-sent as `reasoning_content` on every request. `off` drops it: reasoning can be a third or more of a long session's payload and chat completions is stateless, so the whole of it is re-uploaded on every round — the bytes shrink and stay byte-stable (nothing is sent, so nothing shifts). The model then no longer sees its own earlier thinking, which is what several providers do by stripping replayed reasoning anyway |
| `GOGEN_IMAGE_MAX_EDGE` | `1568` | Long-edge pixel budget for attached images (`read_image` and pasted screenshots). Anything larger is downscaled and re-encoded as JPEG at ingest, because every subsequent request re-uploads the image: a 3.5 MB screenshot is ~4.7 MB of base64 **per round**, while vision models resize to roughly this size internally and never see the extra pixels. PNG/JPEG only — WebP/BMP/GIF pass through untouched. `0` or `off` attaches images byte-for-byte |

### Example
```bash
export OPENAI_API_KEY=sk-...
export OPENAI_MODEL=gpt-4o
export OPENAI_BASE_URL=https://api.openai.com/v1
export GOGEN_WORKING_DIR=/path/to/your/project
export GOGEN_COMMAND_SAFETY=blocklist
export GOGEN_DELETE_APPROVAL=required
export GOGEN_WEB_TOKEN=my-secret-token
export GOGEN_WEB_SEARCH_BACKEND=brave
export GOGEN_WEB_SEARCH_API_KEY=BSA...
export GOGEN_COMMAND_SANDBOX=bwrap
export GOGEN_PRESERVE_REASONING=on

./gogen
```

## Automations

Automations are saved **prompt + working dir + schedule** records that a
running GoGen fires as headless runs at the scheduled time — the "fleet runs
itself" loop on top of the board and subagents.

```bash
# Create: pass exactly one schedule selector.
gogen automation create "Nightly triage" \
  --prompt "Triage new GitHub issues and update the board" \
  --dir /path/to/repo --daily --time 22:00 --timezone America/New_York

gogen automation create "Hourly build check" \
  --prompt "Run the build and report failures" --hourly --minute 15

gogen automation create "Monday standup notes" \
  --prompt "Summarize last week's board activity" --weekly 1

# Inspect / manage
gogen automation ls [--json]
gogen automation get <id> [--json]
gogen automation runs <id> [--json]     # run history (status + session id)
gogen automation update <id> [--title T] [--prompt P] [--dir PATH]
    [--at|--daily|--hourly|--weekdays|--weekly DAYS] [--time HH:MM]
    [--minute M] [--timezone IANA] [--enable|--disable] [--json]
gogen automation enable <id> | disable <id>
gogen automation delete <id> --yes
```

`update` is a PATCH: pass at least one field, omitted fields keep their
stored values. A schedule selector (or `--time`/`--minute`/`--timezone`)
re-times the next run; editing only the prompt/direction leaves an imminent
run where it was.

How it works:

- **Storage**: records live in the global config dir as JSON —
  `~/.config/gogen/automations.json` plus a bounded run-history file
  (`automation_runs.json`, newest-first, 25 runs per automation). Every
  write is atomic and takes an advisory file lock (`automations.lock`) on the
  store directory, re-reading the file before applying a change — so the CLI
  and a running host, or two hosts, never overwrite each other's records. A
  corrupted store file surfaces as an error message (never a panic, and never
  overwritten).
- **Schedules**: `once` (RFC 3339 `--at`, or `2006-01-02 15:04` interpreted
  in `--timezone`), `hourly` (`--minute`), `daily` / `weekdays` /
  `weekly <0=Sun…6=Sat>` at `--time`, all evaluated in an IANA `--timezone`
  (empty = the machine's local zone). Wall-clock times stay pinned across
  DST shifts: a nonexistent time in a spring-forward gap fires just after
  the gap (02:30 → 03:30), the fall-back duplicate takes the earlier
  occurrence.
- **Firing**: while the `automations` feature flag (below) is on, a running
  TUI or web host sweeps the store every 30 s. Each due automation starts a
  detached `gogen -p <prompt> --dir <working_dir>` subprocess — the same
  headless path as a manual run, in its own process, so a failing run is
  recorded as a failed run and never crashes the host. Every run gets a
  history entry (`running` → `completed`/`failed`) linking the fired
  session's id; a run capped at 60 minutes is killed and recorded as
  failed.
- **Catch-up policy**: a due automation fires if it is less than one hour
  overdue (e.g. GoGen opened shortly after the scheduled time); anything
  older — missed while no host was running — is recorded as a `missed` run
  and the schedule advances without firing. `once` schedules disarm
  themselves after their fire (or miss); runs left `running` by a host that
  is gone are marked `interrupted` at startup (each run row records the host
  and pid that started it, so a run another live host is executing is left
  alone).
- **The flag**: `automations: on` (config file, `GOGEN_AUTOMATIONS`, or the
  web Settings → Agent toggle) gates firing only — the CLI manages records
  whether or not the flag is on. Automations fire only while a GoGen host
  is running. Two hosts with the flag on (e.g. TUI and web server) are safe:
  the claim advances `next_run_at` under the store's cross-process lock, so
  each occurrence fires exactly once between them.
- **Web tab**: with the flag on, an **Automations** tab appears in the web
  UI next to Board — the saved jobs with their schedule, next run and last
  status, per-job Edit / Pause / Enable / Delete, an expandable run history
  (status + session id per run), and a create form (title, prompt, working
  dir, schedule selectors, timezone). The CLI and the tab edit the same
  store, so both stay in sync.

## Architecture

```
main.go
└── internal/
    ├── agent/       — Core agent logic, tool execution, safety guards
    ├── automation/  — File-based cron scheduler (saved prompts on a schedule)
    ├── projectfile/ — .gogen/gogen.conf and .md file loading/merging/writing
    ├── mcp/         — MCP stdio client and tool registry
    ├── session/     — Conversation persistence (JSON snapshots on disk)
    ├── skills/      — Skill discovery and loading (project + user dirs)
    ├── spill/       — Oversized tool output persisted to .gogen/spill (session-scoped, 0700/0600; the global data dir in global mode) so caps preview instead of losing data
    ├── tui/         — Interactive terminal interface (Bubble Tea)
    ├── config/      — Environment-based configuration
    ├── contextmgr/  — Token-aware context window management and auto-compaction
    ├── llm/         — OpenAI API integration, model-aware token limits
    ├── server/      — WebSocket-based web server
    ├── treesitter/  — Source code parsing, symbol extraction, syntax checking
    ├── ioutil/      — Atomic file writes and helpers
    ├── randhex/     — Random-hex ID generation (sessions, background jobs, approvals)
    ├── streamutil/  — Token batching for streaming
    ├── modelinfo/   — models.dev context limit resolver
    ├── debuglog/    — Structured debug logging
    └── profiling/   — CPU/memory profiling (debug builds)
```

### Agent Tools

The agent has access to the following tools:

| Tool | Description |
|------|-------------|
| `repo_overview` | Summarize repo layout: top-level dirs, file counts, root files |
| `list_files` | List directory contents (optional `recursive`, `tracked_only`) |
| `glob` | Find files by glob pattern |
| `read_file` | Read a single file (optional `offset`/`limit` ranges, regex `search`) |
| `read_files` | Read multiple files at once |
| `list_definitions` | List functions/types with line numbers (tree-sitter AST, text fallback) |
| `write_file` | Create a new file (refuses existing paths) |
| `execute_command` | Run a shell command (with safety guardrails) |
| `replace_in_file` | Replace a literal string in a file (optional `replace_all`) |
| `delete` | Delete a file or empty directory (requires approval; non-empty directories refused) |
| `patch_file` | Apply surgical unified diff(s) (preferred; `dry_run`, `fuzzy`) |
| `show_diff` | Show git diff (working tree or path) |
| `search_code` | Regex/literal search across the codebase (optional `context_lines`) |
| `find_symbol` | Locate a symbol: `kind=def` (definition) or `refs` (references) |
| `git` | Git history/status/diff: `action=log`, `status`, `show` (read-only; plan mode) |
| `git_commit` | Commit (requires staged files) |
| `git_stage` | Stage files (empty = stage all) |
| `web_search` | Web search (DuckDuckGo Lite; no API key needed) |
| `web_fetch` | Fetch a web page as Markdown (optional `selector`/`query` extraction) |
| `download_file` | Download a raw file into the workspace (binary-safe, SSRF-protected) |
| `find_file` | Find files by name (case-insensitive substring) |
| `rename_symbol` | Rename a symbol across files (AST or text fallback) |
| `call_graph` | Call relationships / impact analysis for a symbol (`direction=impact`) |
| `todo` | Manage todo items: add/list/done/remove/clear |
| `board` | Project kanban board (when `board: on`): list/show/add/update/claim/move/block/comment/done/remove (update edits title/description/priority/context; remove deletes a card entirely). Available in plan mode too — the board is the coordination exception |
| `subagent` | Spawn a nested agent session for a job (when `subagent: on`); the bounded final report returns as the foreground tool result. Set `run_in_background=true` to return a child id immediately and receive a compact terminal event when it finishes. Subagents cannot spawn subagents by default (`subagent_max_depth`) |
| `subagent_fork` | Fork a child session seeded with a deep copy of this session's history and run one turn on it (when `subagent: on`) |
| `list_agents` | List live nested (subagent) sessions of this session: id, label, status, depth, and each child's latest progress line (when `subagent: on`) |
| `get_agent_result` | Get a background subagent's current status or authoritative final result by id; the persisted result remains available after the live child is released (when `subagent: on`) |
| `wait_agent` | Wait up to `timeout_ms` (default 60000, max 600000) for a background subagent's main run to finish; timeout/cancellation leaves the child running (when `subagent: on`) |
| `send_message` | Queue a message for a background subagent of this session — running, interrupted, or finished-but-retained. It runs after any current child turn; its exact reply is injected into this session when that turn finishes (when `subagent: on`) |
| `interrupt_agent` | Cancel the current main or follow-up turn of a live background subagent; an interrupted main run remains continuable (when `subagent: on`) |
| `session_rename` | Rename the current session |
| `context_pin_last` | Pin the last user message to survive compaction |
| `read_image` | Attach an image to the session context for vision-capable models (png/jpeg/gif/webp up to 3.5 MB; optional `detail=auto|low|high`) |
| `background_job` | Manage a background job (`execute_command background=true`): `action=status` (output tail), `action=wait` (block until it exits; `timeout_ms` default 60s, max 10m — on timeout the job keeps running), `action=cancel`, `action=input` (write to the job's stdin), or `action=list` (every live job in this session) |

With `mcp_tool_mode: nested` (the default) the model also gets `mcp_list`
(names + one-line summaries, optional `query`), `mcp_describe` (one tool's
argument schema), and `mcp_call` (run one tool; `input` is that tool's own
parameters). Those three reach every connected server. If the handshake is
still running, they wait up to that server's `init_timeout_secs`.
With `mcp_tool_mode: inline`, server tools are instead listed individually.

Additional tools arrive at runtime from connected MCP servers as `mcp_<server>_<tool>` (inline mode). The system prompt carries the `Additional mcp_* tools may be available.` line only while such a registry is attached — with MCP off (the default) the session never advertises tools it does not have.

## Safety

- **Command Guard** — Shell commands are filtered through a safety layer. In `blocklist` mode, dangerous patterns (`sudo`, `rm -rf /`, `curl | bash`) are not hard-blocked: the agent asks for explicit user approval before running them, and refuses them outright when nobody can be asked (unattended or headless turns). Set `GOGEN_COMMAND_APPROVAL=off` (or the Security settings entry) to keep the blocklist but skip the prompt — such commands are then refused, never prompted for. In `allowlist` mode, only explicitly listed commands may run. Set `GOGEN_COMMAND_SAFETY=off` to disable the guard entirely.
- **Delete Approval** — File deletion requires explicit user confirmation before proceeding (unless `GOGEN_DELETE_APPROVAL=off`).
- **Patch-First Edits** — The agent prefers `patch_file` (unified diffs) over full file rewrites to minimize accidental data loss.
- **Syntax Checking** — After edits, syntax errors are detected via tree-sitter for supported languages.

## Development

The Makefile covers the common tasks; `make check` runs the full local suite
sequentially (it does not auto-update dependencies — use `make update` for
that):

```bash
make check        # fmt, tidy, tests (incl. -race), vet, staticcheck, vuln,
                  # no-CGO build, web UI lint + tests
make test         # go test -race ./...
make test-debug   # debug-tagged packages (view-drift, profiling)
make vet          # go vet ./...
make staticcheck  # go tool staticcheck ./...
make build-nocgo  # verify the documented no-tree-sitter build compiles
make lint-web     # lint the hand-maintained web UI JS (skipped without node)
make test-web     # jsdom regression tests for the web UI (skipped without node)
```

`make outdated` fails when any direct dependency or declared tool has a newer
version; `make update` upgrades them and tidies `go.mod`.

## License

[GNU AGPL-3.0](LICENSE)
