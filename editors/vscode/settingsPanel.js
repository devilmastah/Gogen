const vscode = require("vscode");
const { webviewNonce } = require("./webviewNonce");

let panel = null;

const TABS = [
  { id: "agent", label: "Agent" },
  { id: "subagents", label: "Subagents" },
  { id: "project", label: "Project" },
  { id: "automations", label: "Automations" },
  { id: "tools", label: "Tools" },
  { id: "embeddings", label: "Embeddings" },
  { id: "context", label: "Context" },
  { id: "security", label: "Security" },
  { id: "sessions", label: "Sessions" },
  { id: "providers", label: "Providers" },
  { id: "mcp", label: "MCP" },
  { id: "server", label: "Server" },
];

const ON_OFF = [["off", "Off"], ["on", "On"]];

// Server-backed settings, in the same order as the web settings modal.
// The sidebar is its own UI. The web page's editor, chat layout, and
// appearance stay out: they are not gogen settings, and VS Code already
// owns its editor. A setting that only this sidebar understands can live
// here. Keep the overlap with the web list as small as the shared gogen
// settings allow.
const FIELDS = [
  { tab: "agent", kind: "feature", key: "board", label: "Project board", options: ON_OFF },
  { tab: "agent", kind: "runtime", field: "boardStartPrompt", label: "Board agent prompt", control: "textarea", rows: 6, reset: true, when: "board", note: "Template for the agent started from a board ticket. Placeholders: {id} {title} {description} {priority} {context}. Empty = built-in default." },
  { tab: "agent", kind: "feature", key: "reviewAgent", label: "Board review agent", options: ON_OFF, note: "When on, a ticket moved into in_review starts a review session, then marks the ticket done or comments and moves it back to in_progress. Max 5 review rounds per ticket." },
  { tab: "agent", kind: "model", field: "reviewAgentModel", thinking: "reviewAgentThinkingLevel", label: "Default review model", inherit: "Cascade (ticket override, else workspace default)", thinkingLabel: "Review agent reasoning effort", thinkingInherit: "Inherit the workspace level", when: "review", note: "Empty model = the ticket's review override, else the workspace default. Empty effort = inherit the workspace level." },
  { tab: "agent", kind: "runtime", field: "boardReviewPrompt", label: "Board review prompt", control: "textarea", rows: 6, reset: true, when: "review", note: "Template for the auto review agent. Placeholders: {id} {title} {description} {priority} {context} {assignee}. Empty = built-in default." },
  { tab: "agent", kind: "runtime", field: "systemPrompt", label: "System prompt", control: "textarea", rows: 6, reset: true, note: "Replaces the built-in system prompt; {working_dir} is substituted. Project rules and plan mode still apply. Empty = built-in default." },
  { tab: "agent", kind: "runtime", field: "agentInstructions", label: "Workspace instructions (AGENTS.md / CLAUDE.md)", options: ON_OFF, note: "When on, AGENTS.md / CLAUDE.md files in the working directory are loaded below the project rules (.gogen/gogen.md stays authoritative). Applies live." },
  { tab: "agent", kind: "runtime", field: "jobNotices", label: "Background job notices", options: ON_OFF, note: "When on, a background shell job that finishes on its own posts a one-line summary into the session and starts a turn on it." },
  { tab: "agent", kind: "runtime", field: "joinQueued", label: "Join queued messages", options: ON_OFF, note: "When on, messages you send while a turn is running leave together as one message when that turn ends. Off = each runs as its own turn." },

  { tab: "subagents", kind: "feature", key: "subagent", label: "Subagents", options: ON_OFF },
  { tab: "subagents", kind: "feature", key: "subagentMaxDepth", label: "Max subagent depth", control: "number", min: 1, max: 10, step: 1 },
  { tab: "subagents", kind: "feature", key: "subagentMaxConcurrent", label: "Max concurrent subagents", control: "number", min: 1, max: 32, step: 1, note: "Per session. Spawning beyond the limit is refused. The terminal UI runs subagents one at a time." },
  { tab: "subagents", kind: "model", field: "subagentModel", thinking: "subagentThinkingLevel", label: "Default subagent model", inherit: "Inherit (parent's model)", thinkingLabel: "Subagent reasoning effort", thinkingInherit: "Inherit the parent session's level", when: "subagent", note: "Empty model = inherit the parent's model. The subagent tool's explicit model argument always wins. Empty effort = inherit the parent's level." },
  { tab: "subagents", kind: "runtime", field: "subagentPrompt", label: "Subagent prompt", control: "textarea", rows: 4, reset: true, note: "Wraps the subagent job; {job} is substituted. Empty = built-in default." },

  { tab: "project", kind: "runtime", field: "testCommand", label: "Test command", placeholder: "make test", note: "Overrides the auto-detected test command (Makefile / go test / cargo …). Empty = auto-detect. Applies to every session on its next turn." },
  { tab: "project", kind: "runtime", field: "lintCommand", label: "Lint command", placeholder: "make vet", note: "Overrides the auto-detected lint command (gofmt/vet, eslint, ruff …). Empty = auto-detect. Applies to every session on its next turn." },

  { tab: "automations", kind: "feature", key: "automations", label: "Automations", options: ON_OFF, note: "When on, GoGen sweeps saved automations every 30s and fires each due one as a headless run. Create them with gogen automation create. They fire only while GoGen is running." },

  { tab: "tools", kind: "runtime", field: "webFetch", label: "Web fetch", options: ON_OFF },
  { tab: "tools", kind: "runtime", field: "webFetchMode", label: "Web fetch mode", options: [["https", "HTTPS only"], ["all", "HTTP + HTTPS"]] },
  { tab: "tools", kind: "runtime", field: "webAllowedDomains", label: "Allowed domains (comma-separated)" },
  { tab: "tools", kind: "runtime", field: "webSearch", label: "Web search", options: ON_OFF },
  { tab: "tools", kind: "runtime", field: "webSearchBackend", label: "Search backend", options: [["", "DuckDuckGo"], ["brave", "Brave"]] },
  { tab: "tools", kind: "runtime", field: "webSearchApiKey", label: "Search API key", control: "password", secret: "webSearchApiKeySet", placeholder: "API key" },
  { tab: "tools", kind: "runtime", field: "treesitter", label: "Tree-sitter", options: ON_OFF },
  { tab: "tools", kind: "runtime", field: "treesitterLangs", label: "Tree-sitter languages (comma-separated)" },
  { tab: "embeddings", kind: "feature", key: "codeIndex", label: "Code index (search by meaning)", options: ON_OFF, note: "Off by default. When on, and an embedding provider is chosen, search_meaning indexes this folder into .gogen/index. Nothing is sent until that tool runs, or you reindex. Leave the provider unset and the tool stays off." },
  { tab: "embeddings", kind: "runtime", field: "codeIndexProvider", label: "Embedding provider", providers: true, modelField: "codeIndexModel", options: [["", "Not set"]], note: "Not set leaves the code index off. Any other choice is a name from the Providers tab." },
  { tab: "embeddings", kind: "runtime", field: "codeIndexModel", label: "Embedding model", models: true, options: [["", "Choose a model"]], note: "Choose a model uses that provider's own model. An empty model is an error when the provider has none. The chat model is not sent on its own." },
  { tab: "embeddings", kind: "runtime", field: "codeIndexChunkLines", label: "Chunk lines", control: "number", min: 10, max: 200, step: 1 },
  { tab: "embeddings", kind: "runtime", field: "codeIndexChunkOverlap", label: "Chunk overlap", control: "number", min: 0, step: 1 },
  { tab: "embeddings", kind: "runtime", field: "codeIndexWatch", label: "Watch for changes", options: ON_OFF, note: "On unless set to off. Once an index exists, a changed file is refreshed after a second of quiet. This does not build the first index. Nothing runs while the code index is off or the embedding provider is not set." },
  { tab: "embeddings", kind: "separator" },
  { tab: "embeddings", kind: "runtime", field: "codeIndexRerank", label: "Rerank results", options: ON_OFF, note: "Off by default. When on, a search sends the closest 24 chunks to the rerank provider below and uses that order. A failure keeps the cosine order." },
  { tab: "embeddings", kind: "runtime", field: "codeIndexRerankProvider", label: "Rerank provider", providers: true, blank: "Embedding provider", modelField: "codeIndexRerankModel", options: [["", "Embedding provider"]], note: "Embedding provider uses the same server as the embedding model. Any other choice is a name from the Providers tab." },
  { tab: "embeddings", kind: "runtime", field: "codeIndexRerankModel", label: "Rerank model", models: true, modelSource: "codeIndexRerankProvider", embedFallback: true, options: [["", "Choose a model"]], note: "With a named provider, Choose a model uses that provider's own model. With Embedding provider, pick a model: the embedding model is not sent." },
  { tab: "embeddings", kind: "button", id: "code-index-reindex", label: "Reindex" },
  { tab: "tools", kind: "feature", key: "skills", label: "Skills (skill tool)", options: ON_OFF, note: "Exposes the skill tool: list/read over .gogen/skills and ~/.config/gogen/skills. Applies live." },
  { tab: "tools", kind: "feature", key: "terminal", label: "Persistent terminal tools", options: ON_OFF, note: "Exposes the agent-facing terminal tool and bash_persistent (a per-session shell whose cwd/env persist across calls)." },
  { tab: "tools", kind: "runtime", field: "statusDisplay", label: "Status display (LAN status screen)", options: [["off", "Off"], ["auto", "Auto (probe at startup)"], ["on", "On"], ["browser", "Browser (web UI fallback)"]], note: "Pushes live turn status to an ESP32 status screen. Auto probes and enables reporting when the screen answers. Browser uses this web UI to post the rows when the server cannot reach the display." },
  { tab: "tools", kind: "runtime", field: "statusDisplayHost", label: "Status display host", when: "status", note: "mDNS name, IP, or base URL. Empty falls back to the firmware default." },
  { tab: "tools", kind: "runtime", field: "statusDisplayToken", label: "Status display token", control: "password", secret: "statusDisplayTokenSet", placeholder: "Token", when: "status", note: "Only needed when the display has a shared secret set." },
  { tab: "tools", kind: "runtime", field: "statusDisplaySource", label: "Status display row name", when: "status", note: "Empty = gogen-<project folder>. Set it when two instances share a project folder." },

  { tab: "context", kind: "runtime", field: "contextLimit", prop: "contextLimitConfig", label: "Context limit (0 = auto)", control: "number", min: 0, step: 1000 },
  { tab: "context", kind: "runtime", field: "compactThreshold", label: "Compact threshold (0–1, 0 = off)", control: "number", min: 0, max: 1, step: 0.05 },
  { tab: "context", kind: "runtime", field: "compactKeepRecentMessages", label: "Keep recent messages on compact (0 = only first user message)", control: "number", min: 0, step: 1 },
  { tab: "context", kind: "runtime", field: "compactReserveTokens", label: "Compact reserve tokens", control: "number", min: 0, step: 100 },
  { tab: "context", kind: "runtime", field: "compactLastResort", label: "Last-resort condensation (message too large for the window)", options: [["condense", "Condense (archive the original)"], ["error", "Error (diagnostic, no condensation)"]] },
  { tab: "context", kind: "runtime", field: "smartCompact", label: "Smart compact (inside a tool loop)", options: [["off", "Off"], ["on", "On"]], note: "Default off. When on, a tool loop that crosses the compact threshold is compacted before the next tool runs, and a thinking stream stops with room left to summarize. A tool call that already finished is run after the summary." },
  { tab: "context", kind: "runtime", field: "contextMode", label: "Context mode", options: [["default", "Default"], ["deep_dive", "Deep dive (long-running tasks)"]], note: "Default keeps the usual prompt and compaction. Deep dive tells the model that history is compacted automatically and to keep notes as todo items, and compaction keeps your latest request verbatim between a summary of what came before it and a summary of the work since. With compact threshold 0 the prompt keeps the default advice." },
  { tab: "context", kind: "runtime", field: "preserveReasoning", label: "Preserve reasoning", options: [["auto", "Auto"], ["on", "On"], ["off", "Off"]] },
  { tab: "context", kind: "runtime", field: "maxToolResultBytes", label: "Max tool result bytes (0 = no cap)", control: "number", min: 0, step: 1024 },
  { tab: "context", kind: "runtime", field: "outputSpill", label: "Spill oversized tool output", options: [["on", "On (save to disk, head/tail preview)"], ["off", "Off"]] },

  { tab: "security", kind: "runtime", field: "commandSafety", label: "Command safety", options: [["blocklist", "Blocklist"], ["allowlist", "Allowlist"], ["off", "Off"]] },
  { tab: "security", kind: "runtime", field: "commandAllowlist", label: "Command allowlist (comma-separated)" },
  { tab: "security", kind: "runtime", field: "deleteApproval", label: "Delete approval", options: [["required", "Required"], ["off", "Off"]] },
  { tab: "security", kind: "runtime", field: "commandApproval", label: "Blocked-command approval", options: [["required", "Required — ask, refuse if unattended"], ["off", "Off — block without asking"]], note: "What a command matching the safety blocklist does. Off keeps the block but never prompts." },
  { tab: "security", kind: "runtime", field: "commandSandbox", label: "Command sandbox", options: [["off", "Off"], ["bwrap", "bwrap"]] },
  { tab: "security", kind: "runtime", field: "commandIdleTimeoutSecs", label: "Command idle timeout (seconds, 0 = default 300)", control: "number", min: 0, step: 1 },
  { tab: "security", kind: "runtime", field: "webApprovalHoldSecs", label: "Approval hold (seconds)", control: "number", min: 0, step: 1 },

  { tab: "sessions", kind: "runtime", field: "sessionMaxCount", label: "Max saved sessions", control: "number", min: 1, step: 1 },
  { tab: "sessions", kind: "runtime", field: "sessionMaxAgeDays", label: "Session retention (days, -1 = keep forever)", control: "number", min: -1, step: 1 },

  { tab: "mcp", kind: "runtime", field: "mcp", label: "MCP (restart)", options: ON_OFF, note: "MCP changes take effect after a restart." },
  // The MCP server LIST is edited in the web settings modal (mcp_save /
  // mcp_delete); this sidebar keeps the toggle + exposure mode only.
  { tab: "mcp", kind: "runtime", field: "mcpToolMode", label: "MCP tool exposure", options: [["nested", "Nested (one `mcp` gateway tool)"], ["inline", "Inline (one tool per server tool)"]], note: "Nested keeps a small fixed tool surface and a stable request prefix; inline inlines every discovered server tool. Applies to new sessions." },

  { tab: "server", kind: "runtime", field: "webAllowedOrigins", label: "Allowed origins (restart)" },
  { tab: "server", kind: "runtime", field: "webAuthToken", label: "Auth token (restart)", control: "password", secret: "webAuthTokenSet", placeholder: "Auth token" },
  { tab: "server", kind: "runtime", field: "webTLSCertFile", label: "TLS cert file (restart)" },
  { tab: "server", kind: "runtime", field: "webTLSKeyFile", label: "TLS key file (restart)" },
  { tab: "server", kind: "runtime", field: "webMaxActiveSessions", label: "Max active sessions (restart)", control: "number", min: 1, step: 1 },
];

const DEFAULT_EFFORTS = ["low", "medium", "high"];

function openSettings(host) {
  if (panel) {
    panel.reveal(vscode.ViewColumn.Active);
    return;
  }
  panel = vscode.window.createWebviewPanel("gogen.settings", "GoGen: Settings", vscode.ViewColumn.Active, {
    enableScripts: true,
    retainContextWhenHidden: true,
  });
  panel.webview.html = settingsHtml(panel.webview);
  const send = (event) => {
    if (!panel || !event) {
      return;
    }
    if (event.type === "config" || event.type === "models" || event.type === "providerTest" || event.type === "mcpTest" || event.type === "notice" || event.type === "codeIndex") {
      panel.webview.postMessage(event);
    }
  };
  const off = host.subscribe(send);
  panel.webview.onDidReceiveMessage((msg) => onMessage(host, msg, send));
  panel.onDidDispose(() => {
    off();
    panel = null;
  });
  host.ensureStarted().catch((err) => send({ type: "notice", kind: "settings", text: err.message }));
}

function onMessage(host, msg, send) {
  if (!msg) {
    return;
  }
  if (msg.type === "ready") {
    const config = host.configView();
    if (config) {
      send(config);
    }
    if (host.catalog.length) {
      send(host.modelsView());
    }
    return;
  }
  try {
    if (msg.type === "feature") {
      host.sendFeature(msg.fields || {});
      return;
    }
    if (msg.type === "reindex") {
      host.reindexCode();
      return;
    }
    if (msg.type === "runtime") {
      host.sendRuntime(msg.field, msg.prop, msg.value);
      return;
    }
    if (msg.type === "pickModel") {
      pickSettingModel(host, msg);
      return;
    }
    if (msg.type === "saveProvider") {
      const name = String(msg.name || "").trim();
      if (!name) {
        host.emit({ type: "notice", kind: "provider", text: "Provider name is required" });
        return;
      }
      host.saveProvider(name, String(msg.baseUrl || "").trim(), String(msg.apiKey || ""), String(msg.model || "").trim());
      return;
    }
    if (msg.type === "deleteProvider") {
      host.deleteProvider(String(msg.name || ""));
      return;
    }
    if (msg.type === "testProvider") {
      host.testProvider({
        name: String(msg.name || "").trim(),
        baseUrl: String(msg.baseUrl || "").trim(),
        apiKey: String(msg.apiKey || ""),
        model: String(msg.model || "").trim(),
      });
      return;
    }
    if (msg.type === "testMcp") {
      host.testMcp({
        name: String(msg.name || "").trim(),
        command: String(msg.command || "").trim(),
        args: Array.isArray(msg.args) ? msg.args : [],
        env: msg.env && typeof msg.env === "object" ? msg.env : {},
      });
    }
  } catch (err) {
    host.emit({ type: "notice", kind: "settings", text: err.message });
  }
}

async function pickSettingModel(host, msg) {
  if (!host.catalog.length) {
    await host.requestModels();
  }
  const inherit = { label: msg.inherit || "Inherit", description: "", id: "" };
  const items = [inherit].concat((host.catalog || []).map((m) => ({
    label: m.id,
    description: m.provider && m.provider !== "default" ? m.provider : "",
    id: (m.provider && m.provider !== "default") ? m.provider + "/" + m.id : m.id,
  })));
  const picked = await vscode.window.showQuickPick(items, {
    placeHolder: msg.label || "Choose a model",
    matchOnDescription: true,
  });
  if (!picked) {
    return;
  }
  host.sendRuntime(msg.field, msg.field, picked.id);
  const level = msg.thinkingLevel || "";
  if (level && level !== "off" && !effortsFor(host, picked.id).includes(level)) {
    host.sendRuntime(msg.thinking, msg.thinking, "");
  }
}

function effortsFor(host, modelId) {
  if (modelId) {
    const match = (host.catalog || []).find((m) => {
      const selector = (m.provider && m.provider !== "default") ? m.provider + "/" + m.id : m.id;
      return m.id === modelId || selector === modelId;
    });
    if (match && Array.isArray(match.reasoningEfforts) && match.reasoningEfforts.length) {
      return match.reasoningEfforts;
    }
  } else if (host.lastConfig && Array.isArray(host.lastConfig.reasoningEfforts) && host.lastConfig.reasoningEfforts.length) {
    return host.lastConfig.reasoningEfforts;
  }
  return DEFAULT_EFFORTS;
}

function settingsHtml(webview) {
  const nonce = webviewNonce();
  const csp = [
    "default-src 'none'",
    `style-src ${webview.cspSource} 'nonce-${nonce}'`,
    `script-src 'nonce-${nonce}'`,
  ].join("; ");
  const data = JSON.stringify({ tabs: TABS, fields: FIELDS }).replace(/</g, "\\u003c");
  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="${csp}">
<style nonce="${nonce}">
  body { margin: 0; height: 100vh; display: flex; flex-direction: column; font-family: var(--vscode-font-family); color: var(--vscode-foreground); background: var(--vscode-editor-background); }
  header { padding: 12px 16px 0; }
  h2 { margin: 0 0 4px; font-size: 16px; font-weight: 600; }
  header p { margin: 0 0 8px; color: var(--vscode-descriptionForeground); font-size: 12px; }
  #banner { margin: 0 16px 8px; padding: 8px 10px; background: var(--vscode-inputValidation-warningBackground); color: var(--vscode-inputValidation-warningForeground, inherit); font-size: 12px; }
  #layout { flex: 1; display: flex; min-height: 0; border-top: 1px solid var(--vscode-panel-border); }
  nav { width: 160px; flex: none; border-right: 1px solid var(--vscode-panel-border); overflow: auto; padding: 8px 0; }
  nav button { display: block; width: 100%; text-align: left; background: transparent; color: inherit; border: 0; padding: 6px 14px; cursor: pointer; }
  nav button.active { background: var(--vscode-list-activeSelectionBackground); color: var(--vscode-list-activeSelectionForeground); }
  #content { flex: 1; overflow: auto; padding: 12px 16px 24px; }
  .panel { display: none; max-width: 640px; }
  .panel.active { display: block; }
  .row { margin: 0 0 14px; }
  .sep { border-top: 1px solid var(--vscode-panel-border); margin: 4px 0 16px; }
  label { display: block; margin-bottom: 4px; font-size: 13px; }
  input, select, textarea { width: 100%; box-sizing: border-box; font: inherit; color: inherit; background: var(--vscode-input-background); border: 1px solid var(--vscode-input-border); padding: 6px 8px; }
  textarea { resize: vertical; }
  .note { margin: 4px 0 0; font-size: 12px; color: var(--vscode-descriptionForeground); }
  .actions { display: flex; gap: 8px; margin-top: 6px; }
  button.act { font: inherit; color: var(--vscode-button-foreground); background: var(--vscode-button-background); border: 0; padding: 4px 10px; cursor: pointer; }
  button.quiet { font: inherit; color: var(--vscode-foreground); background: transparent; border: 1px solid var(--vscode-panel-border); padding: 4px 10px; cursor: pointer; }
  .chips { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 6px; }
  .chips button { font: inherit; color: inherit; background: var(--vscode-input-background); border: 1px solid var(--vscode-panel-border); padding: 3px 8px; cursor: pointer; }
  .chips button.on { background: var(--vscode-button-background); color: var(--vscode-button-foreground); border-color: transparent; }
  .item { padding: 8px; border: 1px solid var(--vscode-panel-border); margin-bottom: 6px; }
  .item .meta { font-size: 12px; color: var(--vscode-descriptionForeground); }
  #providerForm, #mcpForm { display: flex; flex-direction: column; gap: 6px; margin-top: 12px; }
</style>
</head>
<body>
<header>
  <h2>GoGen settings</h2>
  <p>Stored in this folder's gogen config, the same values the web settings write. A blank secret keeps the one already stored.</p>
</header>
<div id="banner" hidden></div>
<div id="layout">
  <nav id="nav"></nav>
  <div id="content"></div>
</div>
<script nonce="${nonce}">
const vscodeApi = acquireVsCodeApi();
const SCHEMA = ${data};
const DEFAULT_EFFORTS = ["low", "medium", "high"];
let cfg = {};
let models = [];
const controls = new Map();

function visible(field) {
  if (field.when === "board") return cfg.board === "on";
  if (field.when === "review") return cfg.board === "on" && cfg.reviewAgent === "on";
  if (field.when === "subagent") return cfg.subagent === "on";
  if (field.when === "status") return cfg.statusDisplay && cfg.statusDisplay !== "off";
  return true;
}

function selectorFor(m) {
  const provider = m.provider || "default";
  return provider !== "default" ? provider + "/" + m.id : m.id;
}

function effortsFor(modelId) {
  if (modelId) {
    const match = models.find((m) => m.id === modelId || selectorFor(m) === modelId);
    if (match && Array.isArray(match.reasoningEfforts) && match.reasoningEfforts.length) return match.reasoningEfforts;
  } else if (Array.isArray(cfg.reasoningEfforts) && cfg.reasoningEfforts.length) {
    return cfg.reasoningEfforts;
  }
  return DEFAULT_EFFORTS;
}

function modelLabel(modelId, inherit) {
  if (!modelId) return inherit;
  const match = models.find((m) => selectorFor(m) === modelId || m.id === modelId);
  if (!match) return modelId;
  return (match.provider && match.provider !== "default" ? match.provider + " · " : "") + match.id;
}

function sendRuntime(field, prop, value) {
  vscodeApi.postMessage({ type: "runtime", field, prop: prop || field, value });
}

function build() {
  const nav = document.getElementById("nav");
  const content = document.getElementById("content");
  for (const tab of SCHEMA.tabs) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.textContent = tab.label;
    btn.dataset.tab = tab.id;
    btn.addEventListener("click", () => showTab(tab.id));
    nav.appendChild(btn);
    const panel = document.createElement("section");
    panel.className = "panel";
    panel.id = "tab-" + tab.id;
    content.appendChild(panel);
  }
  for (const field of SCHEMA.fields) {
    const panel = document.getElementById("tab-" + field.tab);
    if (field.kind === "separator") {
      const rule = document.createElement("div");
      rule.className = "sep";
      rule.setAttribute("role", "separator");
      panel.appendChild(rule);
      continue;
    }
    const row = document.createElement("div");
    row.className = "row";
    if (field.kind === "button") {
      const b = document.createElement("button");
      b.type = "button";
      b.className = "quiet";
      b.id = field.id;
      b.textContent = field.label;
      b.hidden = true;
      b.addEventListener("click", () => vscodeApi.postMessage({ type: "reindex" }));
      row.appendChild(b);
      const progress = document.createElement("p");
      progress.id = "code-index-progress";
      progress.className = "note";
      progress.textContent = "Not indexed";
      row.appendChild(progress);
      controls.set(field.id, { field, row, el: b });
      panel.appendChild(row);
      continue;
    }
    const label = document.createElement("label");
    label.textContent = field.label;
    row.appendChild(label);
    if (field.kind === "model") {
      const pick = document.createElement("button");
      pick.type = "button";
      pick.className = "act";
      pick.textContent = field.inherit;
      pick.addEventListener("click", () => {
        vscodeApi.postMessage({
          type: "pickModel",
          field: field.field,
          thinking: field.thinking,
          thinkingLevel: cfg[field.thinking] || "",
          label: field.label,
          inherit: field.inherit,
        });
      });
      const effortLabel = document.createElement("label");
      effortLabel.textContent = field.thinkingLabel;
      effortLabel.style.marginTop = "8px";
      const chips = document.createElement("div");
      chips.className = "chips";
      row.append(pick, effortLabel, chips);
      controls.set(field.field, { field, row, pick, chips });
    } else {
      let el;
      if (field.options) {
        el = document.createElement("select");
        for (const [value, text] of field.options) {
          const opt = document.createElement("option");
          opt.value = value;
          opt.textContent = text;
          el.appendChild(opt);
        }
      } else if (field.control === "textarea") {
        el = document.createElement("textarea");
        el.rows = field.rows || 4;
      } else if (field.control === "password") {
        el = document.createElement("input");
        el.type = "password";
        el.placeholder = field.placeholder || "";
        el.autocomplete = "new-password";
      } else if (field.control === "number") {
        el = document.createElement("input");
        el.type = "number";
        if (field.min !== undefined) el.min = String(field.min);
        if (field.max !== undefined) el.max = String(field.max);
        if (field.step !== undefined) el.step = String(field.step);
      } else {
        el = document.createElement("input");
        el.type = "text";
      }
      el.addEventListener("change", () => onChange(field, el));
      row.appendChild(el);
      if (field.reset) {
        const reset = document.createElement("button");
        reset.type = "button";
        reset.className = "quiet";
        reset.textContent = "Reset to default";
        reset.addEventListener("click", () => sendRuntime(field.field, field.prop, ""));
        row.appendChild(reset);
      }
      controls.set(field.field || field.key, { field, row, el });
    }
    if (field.note) {
      const note = document.createElement("p");
      note.className = "note";
      note.textContent = field.note;
      row.appendChild(note);
    }
    panel.appendChild(row);
  }
  buildProviders(document.getElementById("tab-providers"));
  buildMcp(document.getElementById("tab-mcp"));
  showTab("agent");
}

function onChange(field, el) {
  if (field.kind === "feature") {
    if (field.control === "number") {
      let n = parseInt(el.value, 10);
      if (!Number.isFinite(n)) return;
      if (field.min !== undefined) n = Math.max(n, field.min);
      if (field.max !== undefined) n = Math.min(n, field.max);
      el.value = String(n);
      const fields = {};
      fields[field.key] = n;
      vscodeApi.postMessage({ type: "feature", fields });
      return;
    }
    const fields = {};
    fields[field.key] = el.value;
    vscodeApi.postMessage({ type: "feature", fields });
    return;
  }
  if (field.providers) {
    const model = controls.get(field.modelField || "codeIndexModel");
    if (model && model.el && document.activeElement !== model.el) {
      paintModel(model.field, model.el);
    }
    if (field.field === "codeIndexProvider") {
      const rerank = controls.get("codeIndexRerankModel");
      if (rerank && rerank.el && document.activeElement !== rerank.el) paintModel(rerank.field, rerank.el);
    }
  }
  let value = el.value;
  if (el.type === "number") {
    const step = parseFloat(el.step || "1");
    const n = Number.isFinite(step) && step % 1 === 0 ? parseInt(el.value, 10) : parseFloat(el.value);
    if (!Number.isFinite(n)) return;
    value = n;
  }
  sendRuntime(field.field, field.prop, value);
}

function showTab(id) {
  for (const btn of document.querySelectorAll("#nav button")) {
    btn.classList.toggle("active", btn.dataset.tab === id);
  }
  for (const panel of document.querySelectorAll(".panel")) {
    panel.classList.toggle("active", panel.id === "tab-" + id);
  }
}

function fillSelect(select, options, current) {
  select.replaceChildren();
  for (const [value, text] of options) {
    const opt = document.createElement("option");
    opt.value = value;
    opt.textContent = text;
    select.appendChild(opt);
  }
  select.value = current;
}

// fillCodeIndexProviders lists the configured provider names. The blank
// choice leaves the code index off. A saved name that is no longer in the
// list stays visible so the push does not silently switch the selection.
function fillCodeIndexProviders(select, providers, current, blankLabel) {
  const names = [];
  const seen = new Set();
  for (const p of providers || []) {
    const name = p && p.name;
    if (!name || seen.has(name)) continue;
    seen.add(name);
    const url = p.baseUrl ? " — " + p.baseUrl : "";
    names.push([name, name + url]);
  }
  if (current && !seen.has(current)) names.push([current, current]);
  fillSelect(select, [["", blankLabel || "Not set"]].concat(names), current);
}

// fillCodeIndexModels lists the models that belong to the chosen provider.
// A named provider's blank choice means that provider's own model. An empty
// provider lists no models, because the code index is off.
function fillCodeIndexModels(select, providers, catalog, providerName, chatProvider, current, blankOverride) {
  if (!providerName) {
    const blank = blankOverride || "Choose a model";
    const options = [["", blank]];
    if (current) options.push([current, current]);
    fillSelect(select, options, current || "");
    return;
  }
  const owner = providerName;
  const provider = (providers || []).find((p) => p && p.name === providerName);
  const saved = provider && provider.model ? provider.model : "";
  let blank = saved ? "Provider's model (" + saved + ")" : "Choose a model";
  if (blankOverride) blank = blankOverride;
  const ids = [];
  const seen = new Set();
  for (const m of catalog || []) {
    if (!m || !m.id) continue;
    if ((m.provider || "default") !== owner) continue;
    if (seen.has(m.id)) continue;
    seen.add(m.id);
    ids.push(m.id);
  }
  const options = [["", blank]].concat(ids.map((id) => [id, id]));
  if (current && !seen.has(current)) options.push([current, current]);
  fillSelect(select, options, current);
}

function fillRerankModels(select, providers, catalog, rerankProvider, embedProvider, chatProvider, current) {
  if (rerankProvider) {
    fillCodeIndexModels(select, providers, catalog, rerankProvider, chatProvider, current);
    return;
  }
  fillCodeIndexModels(select, providers, catalog, embedProvider || "", chatProvider, current, "Choose a model");
}

function selectedValue(fieldName) {
  const entry = controls.get(fieldName);
  if (entry && entry.el) return entry.el.value || "";
  return typeof cfg[fieldName] === "string" ? cfg[fieldName] : "";
}

function paintModel(field, el, current) {
  if (current === undefined) current = el.value;
  const chat = cfg.modelProvider || "default";
  if (field.embedFallback) {
    fillRerankModels(el, cfg.providers, models, selectedValue("codeIndexRerankProvider"), selectedValue("codeIndexProvider"), chat, current);
    return;
  }
  fillCodeIndexModels(el, cfg.providers, models, selectedValue(field.modelSource || "codeIndexProvider"), chat, current);
}

function applyConfig() {
  const banner = document.getElementById("banner");
  const pending = Array.isArray(cfg.restartRequired) ? cfg.restartRequired : [];
  if (pending.length) {
    banner.hidden = false;
    banner.textContent = "Restart gogen for these to take effect: " + pending.join(", ");
  } else if (!banner.dataset.note) {
    banner.hidden = true;
  }
  for (const entry of controls.values()) {
    entry.row.hidden = !visible(entry.field);
    if (entry.field.kind === "model") {
      const modelId = cfg[entry.field.field] || "";
      entry.pick.textContent = modelLabel(modelId, entry.field.inherit);
      renderChips(entry, modelId, cfg[entry.field.thinking] || "");
      continue;
    }
    const el = entry.el;
    if (!el || document.activeElement === el || el.type === "password") {
      if (el && el.type === "password" && entry.field.secret) {
        el.placeholder = cfg[entry.field.secret] ? "Saved (blank keeps stored)" : (entry.field.placeholder || "");
      }
      continue;
    }
    const prop = entry.field.prop || entry.field.field || entry.field.key;
    if (entry.field.providers && el.tagName === "SELECT") {
      fillCodeIndexProviders(el, cfg.providers, typeof cfg[prop] === "string" ? cfg[prop] : "", entry.field.blank);
      continue;
    }
    if (entry.field.models && el.tagName === "SELECT") {
      paintModel(entry.field, el, typeof cfg[prop] === "string" ? cfg[prop] : "");
      continue;
    }
    const value = cfg[prop];
    if (value === undefined || value === null) continue;
    if (String(el.value) !== String(value)) el.value = String(value);
  }
  renderProviders();
  renderMcp();
  const reindex = document.getElementById("code-index-reindex");
  if (reindex) reindex.hidden = cfg.codeIndex !== "on";
  const codeLine = document.getElementById("code-index-progress");
  if (codeLine && cfg.codeIndexLine) codeLine.textContent = cfg.codeIndexLine;
  const path = document.getElementById("configPath");
  if (path) path.textContent = cfg.configFilePath || "this folder's gogen config";
}

function renderChips(entry, modelId, level) {
  entry.chips.replaceChildren();
  const add = (value, label) => {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.textContent = label;
    btn.className = value === level ? "on" : "";
    btn.addEventListener("click", () => {
      if (value !== level) sendRuntime(entry.field.thinking, entry.field.thinking, value);
    });
    entry.chips.appendChild(btn);
  };
  add("", "Inherit");
  add("off", "Off");
  for (const value of effortsFor(modelId)) {
    if (value === "off") continue;
    add(value, value.charAt(0).toUpperCase() + value.slice(1));
  }
}

function textInput(id, placeholder, type, autocomplete) {
  const input = document.createElement("input");
  input.id = id;
  input.type = type || "text";
  input.placeholder = placeholder;
  if (autocomplete) input.setAttribute("autocomplete", autocomplete);
  return input;
}

function buildProviders(panel) {
  const warn = document.createElement("p");
  warn.className = "note";
  const where = document.createElement("span");
  where.id = "configPath";
  warn.append("API keys are stored in ", where, ". A blank key keeps the stored one. The default profile cannot be deleted.");
  const list = document.createElement("div");
  list.id = "providerList";
  const form = document.createElement("div");
  form.id = "providerForm";
  form.append(
    textInput("pName", "Name"),
    textInput("pUrl", "Base URL (https://…/v1)"),
    textInput("pKey", "API key (blank keeps stored)", "password", "new-password"),
    textInput("pModel", "Default model (optional)"),
  );
  const actions = document.createElement("div");
  actions.className = "actions";
  const test = document.createElement("button");
  test.type = "button";
  test.className = "quiet";
  test.textContent = "Test";
  test.addEventListener("click", () => {
    setNote("providerNote", "Testing…");
    vscodeApi.postMessage({ type: "testProvider", name: val("pName"), baseUrl: val("pUrl"), apiKey: val("pKey"), model: val("pModel") });
  });
  const save = document.createElement("button");
  save.type = "button";
  save.className = "act";
  save.textContent = "Add / Save";
  save.addEventListener("click", () => {
    vscodeApi.postMessage({ type: "saveProvider", name: val("pName"), baseUrl: val("pUrl"), apiKey: val("pKey"), model: val("pModel") });
    document.getElementById("pKey").value = "";
  });
  actions.append(test, save);
  const note = document.createElement("p");
  note.id = "providerNote";
  note.className = "note";
  panel.append(warn, list, form, actions, note);
}

function renderProviders() {
  const list = document.getElementById("providerList");
  if (!list) return;
  list.replaceChildren();
  for (const p of cfg.providers || []) {
    const row = document.createElement("div");
    row.className = "item";
    const title = document.createElement("div");
    title.textContent = p.name || "default";
    const meta = document.createElement("div");
    meta.className = "meta";
    meta.textContent = [p.baseUrl, p.model, p.apiKeySet ? "key saved" : ""].filter(Boolean).join(" · ");
    const actions = document.createElement("div");
    actions.className = "actions";
    const edit = document.createElement("button");
    edit.type = "button";
    edit.className = "quiet";
    edit.textContent = "Edit";
    edit.addEventListener("click", () => {
      document.getElementById("pName").value = p.name || "";
      document.getElementById("pUrl").value = p.baseUrl || "";
      document.getElementById("pModel").value = p.model || "";
      document.getElementById("pKey").value = "";
      document.getElementById("pKey").placeholder = p.apiKeySet ? "API key (blank keeps stored)" : "API key";
    });
    const test = document.createElement("button");
    test.type = "button";
    test.className = "quiet";
    test.textContent = "Test";
    test.addEventListener("click", () => {
      setNote("providerNote", "Testing " + p.name + "…");
      vscodeApi.postMessage({ type: "testProvider", name: p.name, baseUrl: p.baseUrl || "", model: p.model || "" });
    });
    actions.append(edit, test);
    if (p.deletable) {
      const del = document.createElement("button");
      del.type = "button";
      del.className = "quiet";
      del.textContent = "Delete";
      del.addEventListener("click", () => {
        if (!confirm('Delete provider "' + p.name + '"?')) return;
        vscodeApi.postMessage({ type: "deleteProvider", name: p.name });
      });
      actions.appendChild(del);
    }
    row.append(title, meta, actions);
    list.appendChild(row);
  }
}

function buildMcp(panel) {
  const list = document.createElement("div");
  list.id = "mcpList";
  const form = document.createElement("div");
  form.id = "mcpForm";
  form.append(
    textInput("mName", "Name (for a saved server)"),
    textInput("mCmd", "Command (e.g. npx)"),
    textInput("mArgs", "Args (space-separated)"),
    textInput("mEnv", "Env KEY=VALUE (space-separated)"),
  );
  const actions = document.createElement("div");
  actions.className = "actions";
  const test = document.createElement("button");
  test.type = "button";
  test.className = "quiet";
  test.textContent = "Test";
  test.addEventListener("click", () => {
    setNote("mcpNote", "Testing…");
    const raw = val("mArgs").trim();
    const env = {};
    for (const part of val("mEnv").trim().split(/\\s+/)) {
      const eq = part.indexOf("=");
      if (eq > 0) env[part.slice(0, eq)] = part.slice(eq + 1);
    }
    vscodeApi.postMessage({
      type: "testMcp",
      name: val("mName"),
      command: val("mCmd"),
      args: raw ? raw.split(/\\s+/) : [],
      env,
    });
  });
  actions.appendChild(test);
  const note = document.createElement("p");
  note.id = "mcpNote";
  note.className = "note";
  const help = document.createElement("p");
  help.className = "note";
  help.textContent = "Test checks a saved server, or the command typed above. Servers are added in the config file; this does not register one.";
  panel.append(list, help, form, actions, note);
}

function renderMcp() {
  const list = document.getElementById("mcpList");
  if (!list) return;
  list.replaceChildren();
  const servers = cfg.mcpServers || [];
  if (!servers.length) {
    const empty = document.createElement("p");
    empty.className = "note";
    empty.textContent = "No MCP servers configured.";
    list.appendChild(empty);
    return;
  }
  for (const s of servers) {
    const row = document.createElement("div");
    row.className = "item";
    const title = document.createElement("div");
    title.textContent = s.name || "";
    const meta = document.createElement("div");
    meta.className = "meta";
    meta.textContent = (s.command || "") + (s.args && s.args.length ? " " + s.args.join(" ") : "") + (s.envSet ? " · env saved" : "") + (s.host ? " · this process" : "");
    const actions = document.createElement("div");
    actions.className = "actions";
    if (!s.host) {
      const test = document.createElement("button");
      test.type = "button";
      test.className = "quiet";
      test.textContent = "Test";
      test.addEventListener("click", () => {
        setNote("mcpNote", "Testing " + s.name + "…");
        vscodeApi.postMessage({ type: "testMcp", name: s.name });
      });
      actions.appendChild(test);
    }
    row.append(title, meta, actions);
    list.appendChild(row);
  }
}

function val(id) {
  return (document.getElementById(id) || {}).value || "";
}

function setNote(id, text) {
  const el = document.getElementById(id);
  if (el) el.textContent = text;
}

function testSummary(result, noun) {
  if (!result) return "";
  if (result.ok) {
    const n = (result.models || result.tools || []).length;
    return "OK" + (result.latencyMs ? " in " + result.latencyMs + " ms" : "") + (n ? ", " + n + " " + noun : "");
  }
  return result.error || "failed";
}

window.addEventListener("message", (event) => {
  const msg = event.data || {};
  if (msg.type === "config") {
    cfg = msg;
    applyConfig();
    return;
  }
  if (msg.type === "codeIndex") {
    setNote("code-index-progress", msg.line || "");
    return;
  }
  if (msg.type === "models") {
    models = msg.models || [];
    applyConfig();
    return;
  }
  if (msg.type === "providerTest") {
    setNote("providerNote", testSummary(msg.result, "models"));
    return;
  }
  if (msg.type === "mcpTest") {
    setNote("mcpNote", testSummary(msg.result, "tools"));
    return;
  }
  if (msg.type === "notice" && (msg.kind === "provider" || msg.kind === "settings" || msg.kind === "mcp")) {
    const banner = document.getElementById("banner");
    banner.hidden = false;
    banner.dataset.note = "1";
    banner.textContent = msg.text;
  }
});

build();
vscodeApi.postMessage({ type: "ready" });
</script>
</body>
</html>`;
}

module.exports = { openSettings };
