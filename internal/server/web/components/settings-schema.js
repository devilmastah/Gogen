// Declarative schema for the GoGen web settings modal.
//
// SETTINGS_TABS is the sidebar order (top to bottom); the renderer
// (components/settings-render.js) builds one nav button + one
// `.settings-group[role=tabpanel]` per tab, in this order. SETTINGS_SCHEMA
// is the flat list of settings; each entry names its tab, so grouping and
// order are pure data — regrouping is an edit here, not an HTML shuffle.
//
// Entry shape (simple controls; composites carry only `custom`):
//   id          element id (also the RUNTIME_CONTROLS key)
//   tab         SETTINGS_TABS id
//   type        select | number | text | password | textarea | color
//   label       the <label> text (also the default aria-label)
//   ariaLabel   override the aria-label when it differs from the label
//   options     [[value, text], ...] for `select`
//   min/max/step  for `number`
//   rows        for `textarea`
//   placeholder for text/password
//   value       initial value for `color`
//   button      {id, label} trailing secondary button (e.g. accent reset)
//   note        <p class="settings-note"> text under the control
//   resetToDefault  renders a "Reset to default" button (+ `resetId`)
//   rowId       the .setting-row id when feature gates target the row
//   storage     'server' | 'local'
//   channel     'runtime' | 'feature' (server only) | 'local'
//   field       config field name (server runtime controls)
//   prop        WSMessage prop override when it differs from `field`
//   featureKey  feature-config key (server feature toggles)
//   localKey    localStorage key (local controls)
//   default     initial/fallback value (local controls)
//   custom      id of a composite block moved in from #settings-composites
//
// The server-backed runtime controls are wired generically (settings.js
// derives RUNTIME_CONTROLS from this list); the feature toggles and local
// preferences keep their bespoke wiring in settings.js but their markup is
// generated here, so a new control no longer duplicates its id/label in
// index.html and settings.js.

// NOTE: `var` (not `const`) so the jsdom harness (scripts/web-harness.js),
// which evals each module as an indirect global script and strips imports,
// sees these as globals — top-level `const`/`let` bindings are eval-scoped
// and invisible to later modules. In real ESM this is module-scoped either
// way, and exported normally.
export var SETTINGS_TABS = [
    { id: 'editor', label: 'Editor' },
    { id: 'chat', label: 'Chat' },
    { id: 'appearance', label: 'Appearance' },
    { id: 'agent', label: 'Agent' },
    { id: 'subagents', label: 'Subagents' },
    { id: 'project', label: 'Project' },
    { id: 'automations', label: 'Automations' },
    { id: 'tools', label: 'Tools' },
    { id: 'context', label: 'Context' },
    { id: 'security', label: 'Security' },
    { id: 'sessions', label: 'Sessions' },
    { id: 'providers', label: 'Providers' },
    { id: 'mcp', label: 'MCP' },
    { id: 'server', label: 'Server' },
];

export var SETTINGS_SCHEMA = [
    // ── Editor (local) ─────────────────────────────────────────────────
    { id: 'editor-minimap', tab: 'editor', type: 'select', label: 'Minimap', storage: 'local', channel: 'local', localKey: 'gogen_editor_minimap', default: 'off', options: [['off', 'Off'], ['on', 'On']] },
    { id: 'editor-wordwrap', tab: 'editor', type: 'select', label: 'Word wrap', storage: 'local', channel: 'local', localKey: 'gogen_editor_wordwrap', default: 'on', options: [['on', 'On'], ['off', 'Off']] },
    { id: 'editor-sticky', tab: 'editor', type: 'select', label: 'Sticky scroll', storage: 'local', channel: 'local', localKey: 'gogen_editor_sticky', default: 'on', options: [['on', 'On'], ['off', 'Off']] },
    { id: 'editor-fontsize', tab: 'editor', type: 'number', label: 'Font size', storage: 'local', channel: 'local', localKey: 'gogen_editor_fontsize', default: '13', min: 8, max: 32, step: 1 },

    // ── Chat (local) ───────────────────────────────────────────────────
    { id: 'file-click-behavior', tab: 'chat', type: 'select', label: 'Click file in chat', storage: 'local', channel: 'local', localKey: 'gogen_file_click_behavior', default: 'open', options: [['open', 'Open (background)'], ['open-switch', 'Open & switch to editor']] },
    { id: 'chat-diff-viewer', tab: 'chat', type: 'select', label: 'Chat diff viewer', storage: 'local', channel: 'local', localKey: 'gogen_chat_diff_viewer', default: 'tokenizer', options: [['tokenizer', 'Static (line numbers)'], ['monaco', 'Monaco (full editor)']] },
    { id: 'show-reply-model', tab: 'chat', type: 'select', label: 'Show reply model', storage: 'local', channel: 'local', localKey: 'gogen_show_reply_model', default: 'off', options: [['off', 'Off'], ['on', 'On']] },

    // ── Appearance (local; was the "Global" tab) ───────────────────────
    { id: 'theme-select', tab: 'appearance', type: 'select', label: 'Theme', storage: 'local', channel: 'local', localKey: 'gogen-theme', default: 'auto', options: [['auto', 'Auto (system)'], ['dark', 'Dark'], ['light', 'Light']] },
    { id: 'chat-width-select', tab: 'appearance', type: 'select', label: 'Chat width', storage: 'local', channel: 'local', localKey: 'gogen_chat_width', default: 'full', options: [['full', 'Full width'], ['comfortable', 'Comfortable (centered)']], note: 'Comfortable caps each message to a centered column for easier reading on wide screens.' },
    { id: 'accent-color-input', tab: 'appearance', type: 'color', label: 'Accent color', storage: 'local', channel: 'local', localKey: 'gogen-accent-color', default: '', value: '#7aa2f7', button: { id: 'accent-reset-btn', label: 'Default' } },
    { id: 'notifications-select', tab: 'appearance', type: 'select', label: 'Desktop notifications', storage: 'local', channel: 'local', localKey: 'gogen_notifications', default: 'off', options: [['off', 'Off'], ['background', 'When tab is hidden'], ['always', 'Always']] },

    // ── Agent (server feature toggles + board/review + prompt templates) ─
    { id: 'board-enabled-select', tab: 'agent', type: 'select', label: 'Project board', storage: 'server', channel: 'feature', featureKey: 'board', options: [['off', 'Off'], ['on', 'On']] },
    { id: 'board-start-prompt-input', tab: 'agent', type: 'textarea', label: 'Board agent prompt', storage: 'server', channel: 'runtime', field: 'boardStartPrompt', rows: 6, rowId: 'board-prompt-row', resetToDefault: true, resetId: 'board-prompt-reset-btn', note: 'Template for the agent started from a board ticket. Placeholders: {id} {title} {description} {priority} {context}. Empty = built-in default.' },
    { id: 'review-agent-enabled-select', tab: 'agent', type: 'select', label: 'Board review agent', storage: 'server', channel: 'feature', featureKey: 'reviewAgent', options: [['off', 'Off'], ['on', 'On']], note: 'When on, a ticket moved into in_review automatically starts a review session that verifies the work, then marks the ticket done or comments findings and moves it back to in_progress. Max 5 review rounds per ticket. Web mode only.' },
    { custom: 'review-agent-model-row', tab: 'agent' },
    { custom: 'review-agent-thinking-row', tab: 'agent' },
    { id: 'board-review-prompt-input', tab: 'agent', type: 'textarea', label: 'Board review prompt', storage: 'server', channel: 'runtime', field: 'boardReviewPrompt', rows: 6, rowId: 'board-review-prompt-row', resetToDefault: true, resetId: 'board-review-prompt-reset-btn', note: 'Template for the auto review agent started when a ticket enters in_review. Placeholders: {id} {title} {description} {priority} {context} {assignee}. Empty = built-in default.' },
    { id: 'system-prompt-input', tab: 'agent', type: 'textarea', label: 'System prompt', storage: 'server', channel: 'runtime', field: 'systemPrompt', rows: 6, resetToDefault: true, resetId: 'system-prompt-reset-btn', note: 'Replaces the built-in system prompt; {working_dir} is substituted. Project rules and plan mode still apply. Empty = built-in default.' },
    { id: 'agent-instructions-select', tab: 'agent', type: 'select', label: 'Workspace instructions (AGENTS.md / CLAUDE.md)', storage: 'server', channel: 'runtime', field: 'agentInstructions', options: [['off', 'Off'], ['on', 'On']], note: 'When on, AGENTS.md / CLAUDE.md files in the working directory are loaded and appended BELOW the project rules (.gogen/gogen.md stays authoritative), with byte caps. Applies live to every session.' },
    { id: 'job-notices-select', tab: 'agent', type: 'select', label: 'Background job notices', storage: 'server', channel: 'runtime', field: 'jobNotices', options: [['off', 'Off'], ['on', 'On']], note: 'When on, a background shell job that finishes on its own posts a one-line summary into the session and starts a turn on it (off = a finished job stays silent). Applies live to every session.' },
    { id: 'join-queued-select', tab: 'agent', type: 'select', label: 'Join queued messages', storage: 'server', channel: 'runtime', field: 'joinQueued', options: [['off', 'Off'], ['on', 'On']], note: 'When on, messages you send while a turn is running leave together as one message when that turn ends (off = each queued message runs as its own turn). Applies live to every session.' },

    // ── Subagents (split out of Agent) ─────────────────────────────────
    { id: 'subagent-enabled-select', tab: 'subagents', type: 'select', label: 'Subagents', storage: 'server', channel: 'feature', featureKey: 'subagent', options: [['off', 'Off'], ['on', 'On']] },
    { id: 'subagent-depth-input', tab: 'subagents', type: 'number', label: 'Max subagent depth', storage: 'server', channel: 'feature', featureKey: 'subagentMaxDepth', min: 1, max: 10, step: 1 },
    { id: 'subagent-limit-input', tab: 'subagents', type: 'number', label: 'Max concurrent subagents', storage: 'server', channel: 'feature', featureKey: 'subagentMaxConcurrent', min: 1, max: 32, step: 1, note: 'Per session: how many subagents may run at the same time. Spawning beyond the limit is refused (the agent can interrupt or wait). Web mode only — the TUI runs subagents one at a time.' },
    { custom: 'row-subagent-model-picker', tab: 'subagents' },
    { custom: 'row-subagent-thinking-picker', tab: 'subagents' },
    { id: 'subagent-prompt-input', tab: 'subagents', type: 'textarea', label: 'Subagent prompt', storage: 'server', channel: 'runtime', field: 'subagentPrompt', rows: 4, resetToDefault: true, resetId: 'subagent-prompt-reset-btn', note: 'Wraps the subagent job; {job} is substituted. Empty = built-in default.' },

    // ── Project (server runtime; the workspace profile) ────────────────
    { id: 'test-command-input', tab: 'project', type: 'text', label: 'Test command', storage: 'server', channel: 'runtime', field: 'testCommand', placeholder: 'make test', note: 'Overrides the auto-detected test command (Makefile / go test / cargo …). Empty = auto-detect. Applies to every session on its next turn.' },
    { id: 'lint-command-input', tab: 'project', type: 'text', label: 'Lint command', storage: 'server', channel: 'runtime', field: 'lintCommand', placeholder: 'make vet', note: 'Overrides the auto-detected lint command (gofmt/vet, eslint, ruff …). Empty = auto-detect. Applies to every session on its next turn.' },

    // ── Automations (server feature toggle) ────────────────────────────
    { id: 'automations-enabled-select', tab: 'automations', type: 'select', label: 'Automations', storage: 'server', channel: 'feature', featureKey: 'automations', options: [['off', 'Off'], ['on', 'On']], note: 'When on, GoGen sweeps the saved automations every 30 s and fires each due one as a headless run in its working directory (runs recorded with their session id). Create automations with `gogen automation create <title> --prompt ... --daily|--hourly|--weekly ...`; list them with `gogen automation ls`. Automations fire only while GoGen is running.' },

    // ── Tools (server runtime) ─────────────────────────────────────────
    { id: 'web-fetch-select', tab: 'tools', type: 'select', label: 'Web fetch', storage: 'server', channel: 'runtime', field: 'webFetch', options: [['on', 'On'], ['off', 'Off']] },
    { id: 'web-fetch-mode-select', tab: 'tools', type: 'select', label: 'Web fetch mode', storage: 'server', channel: 'runtime', field: 'webFetchMode', options: [['https', 'HTTPS only'], ['all', 'HTTP + HTTPS']] },
    { id: 'web-allowed-domains-input', tab: 'tools', type: 'text', label: 'Allowed domains (comma-separated)', storage: 'server', channel: 'runtime', field: 'webAllowedDomains' },
    { id: 'web-search-select', tab: 'tools', type: 'select', label: 'Web search', storage: 'server', channel: 'runtime', field: 'webSearch', options: [['on', 'On'], ['off', 'Off']] },
    { id: 'web-search-backend-select', tab: 'tools', type: 'select', label: 'Search backend', storage: 'server', channel: 'runtime', field: 'webSearchBackend', options: [['', 'DuckDuckGo'], ['brave', 'Brave']] },
    { id: 'web-search-api-key-input', tab: 'tools', type: 'password', label: 'Search API key', ariaLabel: 'Web search API key', storage: 'server', channel: 'runtime', field: 'webSearchApiKey', placeholder: 'API key' },
    { id: 'treesitter-select', tab: 'tools', type: 'select', label: 'Tree-sitter', storage: 'server', channel: 'runtime', field: 'treesitter', options: [['on', 'On'], ['off', 'Off']] },
    { id: 'treesitter-langs-input', tab: 'tools', type: 'text', label: 'Tree-sitter languages (comma-separated)', storage: 'server', channel: 'runtime', field: 'treesitterLangs' },
    { id: 'skills-enabled-select', tab: 'tools', type: 'select', label: 'Skills (skill tool)', storage: 'server', channel: 'feature', featureKey: 'skills', options: [['off', 'Off'], ['on', 'On']], note: 'Exposes the skill tool: list/read over .gogen/skills and ~/.config/gogen/skills. Applies live to every session.' },
    { id: 'terminal-enabled-select', tab: 'tools', type: 'select', label: 'Persistent terminal tools', storage: 'server', channel: 'feature', featureKey: 'terminal', options: [['off', 'Off'], ['on', 'On']], note: 'Exposes the agent-facing terminal tool (open/send/read/signal/list/close) and bash_persistent: a per-session shell whose cwd/env persist across calls. Separate from this web UI\'s own terminal tab.' },

    // ── Status display (server runtime; a LAN ttgo/status screen) ──────
    { id: 'status-display-select', tab: 'tools', type: 'select', label: 'Status display (LAN status screen)', storage: 'server', channel: 'runtime', field: 'statusDisplay', options: [['off', 'Off'], ['auto', 'Auto (probe at startup)'], ['on', 'On'], ['browser', 'Browser (web UI fallback)']], note: 'Pushes live turn status to an ESP32 status screen running the ttgo/status firmware: one row per top-level turn, progress = context usage, state = running / done / error / idle. Auto probes for the screen (GET /info) in the background and enables reporting when it answers. Browser probes too, and only when the server cannot reach the display does this web UI POST the rows instead — use it when your browser can reach the display and the server cannot (web mode only). Updates are best-effort and never affect a turn.' },
    { id: 'status-display-host-input', tab: 'tools', type: 'text', label: 'Status display host', ariaLabel: 'Status display host', storage: 'server', channel: 'runtime', field: 'statusDisplayHost', rowId: 'status-display-host-row', note: 'mDNS name, IP, or base URL of the display. The default assumes the firmware advertises statusscreen.local; use the IP if .local does not resolve.' },
    { id: 'status-display-token-input', tab: 'tools', type: 'password', label: 'Status display token', ariaLabel: 'Status display token', storage: 'server', channel: 'runtime', field: 'statusDisplayToken', rowId: 'status-display-token-row', placeholder: 'Token', note: 'Only needed when the display has a shared secret set (GET /token?set=… on the device).' },
    { id: 'status-display-source-input', tab: 'tools', type: 'text', label: 'Status display row name', ariaLabel: 'Status display row name', storage: 'server', channel: 'runtime', field: 'statusDisplaySource', rowId: 'status-display-source-row', note: 'Empty = auto: gogen-<project folder>, so instances in different projects get their own row. Set it explicitly when two instances share a project folder (e.g. gogen-tui / gogen-web).' },

    // ── Context (server runtime) ───────────────────────────────────────
    { id: 'context-limit-input', tab: 'context', type: 'number', label: 'Context limit (0 = auto)', storage: 'server', channel: 'runtime', field: 'contextLimit', prop: 'contextLimitConfig', min: 0, step: 1000 },
    { id: 'compact-threshold-input', tab: 'context', type: 'number', label: 'Compact threshold (0–1, 0 = off)', storage: 'server', channel: 'runtime', field: 'compactThreshold', min: 0, max: 1, step: 0.05 },
    { id: 'compact-keep-input', tab: 'context', type: 'number', label: 'Keep recent messages on compact (0 = only first user message)', storage: 'server', channel: 'runtime', field: 'compactKeepRecentMessages', min: 0, step: 1 },
    { id: 'compact-reserve-input', tab: 'context', type: 'number', label: 'Compact reserve tokens', storage: 'server', channel: 'runtime', field: 'compactReserveTokens', min: 0, step: 100 },
    { id: 'compact-last-resort-select', tab: 'context', type: 'select', label: 'Last-resort condensation (message too large for the window)', storage: 'server', channel: 'runtime', field: 'compactLastResort', options: [['condense', 'Condense (archive the original)'], ['error', 'Error (diagnostic, no condensation)']] },
    { id: 'smart-compact-select', tab: 'context', type: 'select', label: 'Smart compact (inside a tool loop)', storage: 'server', channel: 'runtime', field: 'smartCompact', options: [['off', 'Off'], ['on', 'On']], note: 'Default off. When on, a tool loop that crosses the compact threshold is compacted before the next tool runs, and a thinking stream stops with room left to summarize. A tool call that already finished is run after the summary.' },
    { id: 'context-mode-select', tab: 'context', type: 'select', label: 'Context mode', storage: 'server', channel: 'runtime', field: 'contextMode', options: [['default', 'Default'], ['deep_dive', 'Deep dive (long-running tasks)']], note: 'Default keeps the usual prompt and compaction. Deep dive tells the model that history is compacted automatically and to keep notes as todo items, and compaction keeps your latest request verbatim between a summary of what came before it and a summary of the work since. With compact threshold 0 the prompt keeps the default advice.' },
    { id: 'preserve-reasoning-select', tab: 'context', type: 'select', label: 'Preserve reasoning', storage: 'server', channel: 'runtime', field: 'preserveReasoning', options: [['auto', 'Auto'], ['on', 'On'], ['off', 'Off']] },
    { id: 'max-tool-bytes-input', tab: 'context', type: 'number', label: 'Max tool result bytes (0 = no cap)', storage: 'server', channel: 'runtime', field: 'maxToolResultBytes', min: 0, step: 1024 },
    { id: 'output-spill-select', tab: 'context', type: 'select', label: 'Spill oversized tool output (off = legacy head-only truncation)', storage: 'server', channel: 'runtime', field: 'outputSpill', options: [['on', 'On (save to disk, head/tail preview)'], ['off', 'Off']] },

    // ── Security (server runtime; absorbed Approval hold) ──────────────
    { id: 'command-safety-select', tab: 'security', type: 'select', label: 'Command safety', storage: 'server', channel: 'runtime', field: 'commandSafety', options: [['blocklist', 'Blocklist'], ['allowlist', 'Allowlist'], ['off', 'Off']] },
    { id: 'command-allowlist-input', tab: 'security', type: 'text', label: 'Command allowlist (comma-separated)', storage: 'server', channel: 'runtime', field: 'commandAllowlist' },
    { id: 'delete-approval-select', tab: 'security', type: 'select', label: 'Delete approval', storage: 'server', channel: 'runtime', field: 'deleteApproval', options: [['required', 'Required'], ['off', 'Off']] },
    { id: 'command-approval-select', tab: 'security', type: 'select', label: 'Blocked-command approval', storage: 'server', channel: 'runtime', field: 'commandApproval', options: [['required', 'Required — ask, refuse if unattended'], ['off', 'Off — block without asking']], note: 'What a command matching the safety blocklist does. Off keeps the block but never prompts; dangerous commands are still refused (command_safety off is what allows them).' },
    { id: 'command-sandbox-select', tab: 'security', type: 'select', label: 'Command sandbox', storage: 'server', channel: 'runtime', field: 'commandSandbox', options: [['off', 'Off'], ['bwrap', 'bwrap']] },
    { id: 'command-idle-timeout-input', tab: 'security', type: 'number', label: 'Command idle timeout (seconds, 0 = default 300)', storage: 'server', channel: 'runtime', field: 'commandIdleTimeoutSecs', min: 0, step: 1 },
    { id: 'approval-hold-input', tab: 'security', type: 'number', label: 'Approval hold (seconds)', storage: 'server', channel: 'runtime', field: 'webApprovalHoldSecs', min: 0, step: 1 },

    // ── Sessions (server runtime) ──────────────────────────────────────
    { id: 'session-max-count-input', tab: 'sessions', type: 'number', label: 'Max saved sessions', storage: 'server', channel: 'runtime', field: 'sessionMaxCount', min: 1, step: 1 },
    { id: 'session-max-age-input', tab: 'sessions', type: 'number', label: 'Session retention (days)', ariaLabel: 'Session retention days (-1 = keep forever)', storage: 'server', channel: 'runtime', field: 'sessionMaxAgeDays', min: -1, step: 1 },

    // ── Providers (composite) ──────────────────────────────────────────
    { custom: 'provider-block', tab: 'providers' },

    // ── MCP (simple toggle + composite list/form) ──────────────────────
    { id: 'mcp-select', tab: 'mcp', type: 'select', label: 'MCP (restart)', storage: 'server', channel: 'runtime', field: 'mcp', options: [['off', 'Off'], ['on', 'On']], note: 'MCP changes take effect after a restart.' },
    { id: 'mcp-tool-mode-select', tab: 'mcp', type: 'select', label: 'MCP tool exposure', storage: 'server', channel: 'runtime', field: 'mcpToolMode', options: [['nested', 'Nested (one `mcp` gateway tool)'], ['inline', 'Inline (one tool per server tool)']], note: 'Nested keeps a small fixed tool surface and a stable request prefix; inline inlines every discovered server tool into the request. Applies to new sessions (the tool set is fixed for the life of a session).' },
    { custom: 'mcp-block', tab: 'mcp' },

    // ── Server (restart-staged, server runtime) ────────────────────────
    { custom: 'restart-banner', tab: 'server' },
    { id: 'web-bind-input', tab: 'server', type: 'text', label: 'Web bind (restart)', ariaLabel: 'Web bind address', storage: 'server', channel: 'runtime', field: 'webBind' },
    { id: 'web-allowed-origins-input', tab: 'server', type: 'text', label: 'Allowed origins (restart)', storage: 'server', channel: 'runtime', field: 'webAllowedOrigins' },
    { id: 'web-auth-token-input', tab: 'server', type: 'password', label: 'Auth token (restart)', ariaLabel: 'Auth token', storage: 'server', channel: 'runtime', field: 'webAuthToken', placeholder: 'Auth token' },
    { id: 'web-tls-cert-input', tab: 'server', type: 'text', label: 'TLS cert file (restart)', ariaLabel: 'TLS cert file', storage: 'server', channel: 'runtime', field: 'webTLSCertFile' },
    { id: 'web-tls-key-input', tab: 'server', type: 'text', label: 'TLS key file (restart)', ariaLabel: 'TLS key file', storage: 'server', channel: 'runtime', field: 'webTLSKeyFile' },
    { id: 'web-max-active-input', tab: 'server', type: 'number', label: 'Max active sessions (restart)', storage: 'server', channel: 'runtime', field: 'webMaxActiveSessions', min: 1, step: 1 },
];
