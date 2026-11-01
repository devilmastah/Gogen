const vscode = require("vscode");
const { webviewNonce } = require("./webviewNonce");

class ChatViewProvider {
  constructor(host) {
    this.host = host;
    this.view = null;
    this.unsubscribe = null;
  }

  resolveWebviewView(view) {
    this.view = view;
    view.webview.options = { enableScripts: true };
    view.webview.html = chatHtml(view.webview);
    view.webview.onDidReceiveMessage((msg) => this.onWebview(msg));
    this.unsubscribe = this.host.subscribe((event) => this.post(event));
    this.host.replayPathApproval();
    view.onDidChangeVisibility(() => {
      if (view.visible && !this.host.sidecar) {
        this.host.ensureStarted().catch(() => {});
      }
    });
    view.onDidDispose(() => {
      if (this.unsubscribe) {
        this.unsubscribe();
        this.unsubscribe = null;
      }
      this.view = null;
    });
    this.host.ensureStarted().catch(() => {});
  }

  onWebview(msg) {
    if (!msg) {
      return;
    }
    if (msg.type === "send") {
      const text = String(msg.text || "").trim();
      if (!text) {
        return;
      }
      const queueId = typeof msg.queueId === "string" ? msg.queueId : "";
      this.host.ensureStarted().then(() => {
        try {
          this.host.sendChat(text, queueId);
        } catch (err) {
          this.post({ type: "notice", text: err.message });
          this.post({ type: "restoreInput", text, queueId });
        }
      }).catch((err) => {
        this.post({ type: "notice", text: err.message || String(err) });
        this.post({ type: "restoreInput", text, queueId });
      });
      return;
    }
    if (msg.type === "pickModel") {
      vscode.commands.executeCommand("gogen.pickModel");
      return;
    }
    if (msg.type === "openSettings") {
      vscode.commands.executeCommand("gogen.settings");
      return;
    }
    if (msg.type === "openBoard") {
      vscode.commands.executeCommand("gogen.board");
      return;
    }
    if (msg.type === "openWorkspace") {
      vscode.commands.executeCommand("gogen.workspace");
      return;
    }
    if (msg.type === "setMode") {
      try {
        this.host.setMode(msg.mode);
      } catch (err) {
        this.post({ type: "notice", text: err.message });
      }
      return;
    }
    if (msg.type === "pickThinking") {
      this.host.pickThinking();
      return;
    }
    if (msg.type === "queueSendNow") {
      try {
        this.host.sendQueueNow(msg.id);
      } catch (err) {
        this.post({ type: "notice", text: err.message });
      }
      return;
    }
    if (msg.type === "queueRemove") {
      try {
        this.host.removeQueued(msg.id);
      } catch (err) {
        this.post({ type: "notice", text: err.message });
      }
      return;
    }
    if (msg.type === "cancel") {
      try {
        this.host.cancelTurn();
      } catch (err) {
        this.post({ type: "notice", text: err.message });
      }
      return;
    }
    if (msg.type === "compact") {
      try {
        this.host.compact();
      } catch (err) {
        this.post({ type: "notice", text: err.message });
      }
      return;
    }
    if (msg.type === "setupEndpoint") {
      try {
        this.host.saveProvider("default", String(msg.baseUrl || "").trim(), String(msg.apiKey || ""));
      } catch (err) {
        this.post({ type: "notice", text: err.message });
      }
      return;
    }
    if (msg.type === "setupModel") {
      try {
        this.host.pickModel(msg.provider, msg.id);
      } catch (err) {
        this.post({ type: "notice", text: err.message });
      }
      return;
    }
    if (msg.type === "setupMcp") {
      this.host.enableMcpAndRestart();
      return;
    }
    if (msg.type === "pathAnswer") {
      this.host.answerPath(msg.id, msg.allow === true);
    }
  }

  post(msg) {
    this.view?.webview.postMessage(msg);
  }
}

function chatHtml(webview) {
  const nonce = webviewNonce();
  const csp = [
    "default-src 'none'",
    `style-src ${webview.cspSource} 'nonce-${nonce}'`,
    `script-src 'nonce-${nonce}'`,
  ].join("; ");
  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="${csp}">
<style nonce="${nonce}">
  :root {
    color-scheme: light dark;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    height: 100vh;
    display: flex;
    flex-direction: column;
    font-family: var(--vscode-font-family);
    font-size: 13px;
    color: var(--vscode-foreground);
    background: var(--vscode-sideBar-background);
  }
  button { font: inherit; cursor: pointer; }
  button:disabled { opacity: 0.45; cursor: default; }
  #top {
    position: relative;
    z-index: 2;
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 10px 10px 2px;
  }
  #brand { flex: 1; min-width: 0; }
  .mark {
    font-size: 11px;
    font-weight: 650;
    letter-spacing: 0.08em;
    text-transform: uppercase;
  }
  #status {
    display: block;
    margin-top: 1px;
    font-size: 11px;
    line-height: 1.3;
    color: var(--vscode-descriptionForeground);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  #status.error { color: var(--vscode-errorForeground); }
  #menu-btn, .icon {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: 4px;
    width: 26px;
    height: 26px;
    padding: 0;
    border: 0;
    border-radius: 7px;
    background: transparent;
    color: var(--vscode-descriptionForeground);
  }
  #menu-btn:hover, .icon:hover:not(:disabled) {
    background: color-mix(in srgb, var(--vscode-foreground) 8%, transparent);
    color: var(--vscode-foreground);
  }
  #think.on { color: var(--vscode-textLink-foreground); }
  #think { width: auto; padding: 0 6px; }
  #menu {
    position: absolute;
    top: 36px;
    right: 8px;
    z-index: 5;
    display: flex;
    flex-direction: column;
    min-width: 180px;
    padding: 4px;
    border-radius: 10px;
    border: 1px solid var(--vscode-menu-border, var(--vscode-widget-border));
    background: var(--vscode-menu-background, var(--vscode-editorWidget-background));
    color: var(--vscode-menu-foreground, var(--vscode-foreground));
    box-shadow: 0 10px 28px color-mix(in srgb, #000 28%, transparent);
  }
  #menu[hidden] { display: none; }
  #menu button {
    display: block;
    width: 100%;
    text-align: left;
    border: 0;
    border-radius: 6px;
    padding: 6px 8px;
    background: transparent;
    color: inherit;
  }
  #menu button:hover { background: var(--vscode-menu-selectionBackground, var(--vscode-list-hoverBackground)); }
  #stage {
    position: relative;
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
  }
  #log {
    flex: 1;
    overflow: auto;
    display: flex;
    flex-direction: column;
    gap: 10px;
    padding: 12px 12px 8px;
  }
  #log:empty {
    align-items: center;
    justify-content: center;
  }
  #log:empty::before {
    content: "Ask about this folder.";
    color: var(--vscode-descriptionForeground);
    font-size: 13px;
  }
  .msg { white-space: pre-wrap; word-break: break-word; line-height: 1.45; }
  .user {
    align-self: flex-end;
    max-width: 88%;
    padding: 8px 10px;
    border-radius: 14px 14px 4px 14px;
    background: color-mix(in srgb, var(--vscode-button-background) 18%, var(--vscode-input-background));
    border: 1px solid color-mix(in srgb, var(--vscode-button-background) 35%, transparent);
  }
  /* Queued sends sit outside the scrolling transcript, so a later token
     cannot push them up into the reply that is still being written. */
  #queue {
    display: flex;
    flex-direction: column;
    align-items: flex-end;
    gap: 6px;
    padding: 0 12px 8px;
  }
  #queue:empty { display: none; padding: 0; }
  .queue-actions {
    display: flex;
    justify-content: flex-end;
    gap: 6px;
    margin-top: 6px;
  }
  .queue-actions button {
    font: inherit;
    font-size: 11px;
    border-radius: 999px;
    border: 1px solid color-mix(in srgb, var(--vscode-foreground) 28%, transparent);
    background: transparent;
    color: var(--vscode-foreground);
    padding: 1px 8px;
    cursor: pointer;
  }
  .assistant {
    align-self: stretch;
    padding: 8px 10px;
    border-radius: 12px 12px 12px 4px;
    background: color-mix(in srgb, var(--vscode-foreground) 7%, transparent);
    line-height: 1.5;
  }
  .thinking {
    align-self: stretch;
    padding: 8px 10px;
    border-radius: 10px;
    border-left: 2px solid color-mix(in srgb, var(--vscode-foreground) 28%, transparent);
    color: var(--vscode-descriptionForeground);
    background: color-mix(in srgb, var(--vscode-foreground) 4%, transparent);
    font-size: 12px;
  }
  .thinking::before {
    content: "Thinking";
    display: block;
    margin-bottom: 3px;
    font-size: 10px;
    letter-spacing: 0.08em;
    text-transform: uppercase;
  }
  .tool {
    align-self: stretch;
    border-radius: 8px;
    border: 1px solid color-mix(in srgb, var(--vscode-foreground) 10%, transparent);
    background: color-mix(in srgb, var(--vscode-foreground) 8%, var(--vscode-editor-background));
    color: var(--vscode-descriptionForeground);
    font-family: var(--vscode-editor-font-family);
    font-size: 11px;
    line-height: 1.35;
  }
  .tool-head { padding: 4px 8px; }
  .tool.failed .tool-head { color: var(--vscode-errorForeground); }
  .tool-body {
    margin: 0;
    padding: 0 8px 6px;
    white-space: pre-wrap;
    word-break: break-word;
    font: inherit;
    max-height: 140px;
    overflow: auto;
  }
  .tool-body:empty { display: none; }
  body.tools-collapsed .tool-body { display: none; }
  #tools {
    align-self: flex-start;
    margin: 0 10px 6px;
    border: 0;
    border-radius: 999px;
    padding: 3px 10px;
    background: color-mix(in srgb, var(--vscode-foreground) 8%, transparent);
    color: var(--vscode-foreground);
    font: inherit;
    font-size: 11px;
  }
  #tools[hidden] { display: none; }
  .notice { color: var(--vscode-descriptionForeground); font-size: 12px; }
  #near {
    display: flex;
    gap: 6px;
    align-items: center;
    margin: 0 10px 6px;
    padding: 6px 8px;
    border-radius: 8px;
    font-size: 12px;
    background: var(--vscode-inputValidation-warningBackground, var(--vscode-editorWarning-background));
  }
  #near[hidden] { display: none; }
  #near span { flex: 1; }
  #near button, #dismiss {
    border: 0;
    border-radius: 6px;
    padding: 2px 8px;
    background: transparent;
    color: var(--vscode-foreground);
  }
  form { margin: 0; padding: 0 10px 10px; }
  #box {
    border: 1px solid var(--vscode-widget-border, var(--vscode-panel-border));
    border-radius: 14px;
    background: var(--vscode-input-background);
  }
  #box:focus-within { border-color: var(--vscode-focusBorder); }
  textarea {
    display: block;
    width: 100%;
    min-height: 52px;
    max-height: 140px;
    margin: 0;
    padding: 10px 12px 2px;
    border: 0;
    resize: none;
    outline: none;
    background: transparent;
    color: inherit;
    font: inherit;
    line-height: 1.4;
  }
  textarea::placeholder { color: var(--vscode-input-placeholderForeground); }
  #row {
    display: flex;
    align-items: center;
    gap: 4px;
    padding: 4px 6px 6px;
  }
  .pill {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    max-width: 42%;
    height: 26px;
    padding: 0 8px;
    border: 0;
    border-radius: 999px;
    background: color-mix(in srgb, var(--vscode-foreground) 8%, transparent);
    color: var(--vscode-foreground);
  }
  .pill:hover:not(:disabled) { background: color-mix(in srgb, var(--vscode-foreground) 14%, transparent); }
  .pill span {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .pill::after {
    content: "";
    flex: 0 0 auto;
    border: 3px solid transparent;
    border-top-color: currentColor;
    transform: translateY(1px);
    opacity: 0.7;
  }
  #mode, #ctx, #index-spin, #think, #cancel, #send { flex-shrink: 0; }
  #model { min-width: 0; }
  #rate {
    flex: 1;
    min-width: 0;
    font-size: 11px;
    color: var(--vscode-descriptionForeground);
    font-variant-numeric: tabular-nums;
  }
  #ctx {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-size: 11px;
    color: var(--vscode-descriptionForeground);
    font-variant-numeric: tabular-nums;
    cursor: default;
  }
  #ring {
    width: 13px;
    height: 13px;
    border-radius: 50%;
    background: conic-gradient(var(--ring, var(--vscode-textLink-foreground)) var(--pct, 0%), color-mix(in srgb, var(--vscode-foreground) 16%, transparent) 0);
  }
  #index-spin {
    width: 13px;
    height: 13px;
    box-sizing: border-box;
    border-radius: 50%;
    border: 2px solid color-mix(in srgb, var(--vscode-foreground) 16%, transparent);
    border-top-color: var(--vscode-textLink-foreground);
    animation: index-spin 0.8s linear infinite;
  }
  #index-spin[hidden] { display: none; }
  @keyframes index-spin { to { transform: rotate(360deg); } }
  #ctx[data-tone="warning"] { color: var(--vscode-editorWarning-foreground); --ring: var(--vscode-editorWarning-foreground); }
  #ctx[data-tone="danger"] { color: var(--vscode-errorForeground); --ring: var(--vscode-errorForeground); }
  #cancel[hidden], #think[hidden] { display: none; }
  #send {
    width: 26px;
    height: 26px;
    padding: 0;
    border: 0;
    border-radius: 50%;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    background: var(--vscode-button-background);
    color: var(--vscode-button-foreground);
  }
  #send:hover:not(:disabled) { background: var(--vscode-button-hoverBackground); }
  #setup {
    position: absolute;
    inset: 0;
    z-index: 4;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 16px;
    background: color-mix(in srgb, var(--vscode-sideBar-background) 92%, transparent);
  }
  #setup[hidden] { display: none; }
  .setup-card {
    width: min(100%, 360px);
    display: flex;
    flex-direction: column;
    gap: 12px;
    padding: 14px;
    border-radius: 16px;
    background: var(--vscode-editor-background);
    border: 1px solid color-mix(in srgb, var(--vscode-foreground) 12%, transparent);
  }
  .setup-steps { display: flex; gap: 6px; }
  .setup-steps span {
    flex: 1;
    padding: 4px 0;
    border-radius: 999px;
    text-align: center;
    font-size: 11px;
    color: var(--vscode-descriptionForeground);
    background: color-mix(in srgb, var(--vscode-foreground) 6%, transparent);
  }
  .setup-steps .done { color: var(--vscode-foreground); }
  .setup-steps .now {
    color: var(--vscode-button-foreground);
    background: var(--vscode-button-background);
  }
  .setup-card h2 { margin: 0; font-size: 14px; font-weight: 650; }
  .setup-card p { margin: 0; font-size: 12px; line-height: 1.4; color: var(--vscode-descriptionForeground); }
  .setup-card label { display: flex; flex-direction: column; gap: 4px; font-size: 11px; color: var(--vscode-descriptionForeground); }
  .setup-card input, .setup-card select {
    width: 100%;
    padding: 6px 8px;
    border-radius: 8px;
    border: 1px solid var(--vscode-input-border, transparent);
    background: var(--vscode-input-background);
    color: var(--vscode-input-foreground);
    font: inherit;
  }
  .setup-card button.primary {
    align-self: flex-start;
    padding: 6px 12px;
    border: 0;
    border-radius: 999px;
    background: var(--vscode-button-background);
    color: var(--vscode-button-foreground);
  }
  .setup-card button.primary:hover:not(:disabled) { background: var(--vscode-button-hoverBackground); }
  #setup-note { min-height: 1.2em; }
  #path-ask {
    position: fixed;
    inset: 0;
    z-index: 30;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 16px;
    background: color-mix(in srgb, var(--vscode-sideBar-background) 35%, transparent);
  }
  #path-ask[hidden] { display: none; }
  .path-card {
    width: 100%;
    max-width: 360px;
    padding: 14px;
    border: 1px solid var(--vscode-widget-border, var(--vscode-panel-border));
    border-radius: 8px;
    background: var(--vscode-editorWidget-background, var(--vscode-sideBar-background));
    box-shadow: 0 8px 24px color-mix(in srgb, black 35%, transparent);
  }
  .path-card h2 { margin: 0 0 8px; font-size: 13px; font-weight: 600; }
  #path-ask-file {
    margin: 0 0 12px;
    white-space: pre-wrap;
    word-break: break-all;
    font-family: var(--vscode-editor-font-family);
    font-size: 12px;
  }
  .path-actions { display: flex; justify-content: flex-end; gap: 8px; }
  .path-actions button {
    padding: 4px 12px;
    border: 1px solid var(--vscode-button-border, transparent);
    border-radius: 4px;
    background: var(--vscode-button-secondaryBackground);
    color: var(--vscode-button-secondaryForeground);
  }
  .path-actions button.primary {
    background: var(--vscode-button-background);
    color: var(--vscode-button-foreground);
  }
  @media (prefers-reduced-motion: reduce) {
    #index-spin { animation: none; }
  }
</style>
</head>
<body>
<div id="path-ask" hidden role="dialog" aria-modal="true" aria-labelledby="path-ask-title">
  <div class="path-card">
    <h2 id="path-ask-title">GoGen wants to use a path outside this workspace</h2>
    <p id="path-ask-file"></p>
    <div class="path-actions">
      <button id="path-deny" type="button">Deny</button>
      <button id="path-allow" type="button" class="primary">Allow</button>
    </div>
  </div>
</div>
<div id="top">
  <div id="brand">
    <div class="mark">gogen</div>
    <div id="status">Starting…</div>
  </div>
  <button id="menu-btn" type="button" aria-label="More" aria-expanded="false" title="Settings, board, folder">
    <svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true"><circle cx="3.5" cy="8" r="1.15" fill="currentColor"/><circle cx="8" cy="8" r="1.15" fill="currentColor"/><circle cx="12.5" cy="8" r="1.15" fill="currentColor"/></svg>
  </button>
  <div id="menu" hidden>
    <button id="settings" type="button">Settings</button>
    <button id="board" type="button">Board</button>
    <button id="workspace" type="button">Folder</button>
  </div>
</div>
<div id="stage">
<div id="log"></div>
<div id="queue"></div>
<button id="tools" type="button" hidden>Hide tools</button>
<div id="near" hidden>
  <span>Context is nearly full.</span>
  <button id="compact" type="button">Compact</button>
  <button id="dismiss" type="button">Dismiss</button>
</div>
<form id="form">
  <div id="box">
    <textarea id="input" rows="1" placeholder="Message gogen"></textarea>
    <div id="row">
      <button id="mode" class="pill" type="button" disabled><span id="mode-label">Act</span></button>
      <button id="model" class="pill" type="button" disabled title="Choose a model"><span id="model-label">Choose a model</span></button>
      <span id="rate"></span>
      <span id="index-spin" hidden title="Updating the code index"></span>
      <span id="ctx" title="Context usage"><i id="ring"></i><span id="ctx-label">—</span></span>
      <button id="think" class="icon" type="button" disabled hidden title="Thinking level">
        <svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true"><path fill="currentColor" d="M8.1 1.8a5.7 5.7 0 1 0 6.1 7.2A4.5 4.5 0 0 1 8.1 1.8z"/></svg>
        <span id="think-label">Off</span>
      </button>
      <button id="cancel" class="icon" type="button" hidden title="Cancel">
        <svg viewBox="0 0 16 16" width="12" height="12" aria-hidden="true"><rect x="4" y="4" width="8" height="8" rx="1.4" fill="currentColor"/></svg>
      </button>
      <button id="send" type="submit" disabled title="Send" aria-label="Send">
        <svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true"><path fill="currentColor" d="M8 2.8 3.2 7.7h2.7v5.5h2.2V7.7h2.7L8 2.8z"/></svg>
      </button>
    </div>
  </div>
</form>
<div id="setup" hidden>
  <div class="setup-card">
    <div class="setup-steps">
      <span id="setup-mark-endpoint">Endpoint</span>
      <span id="setup-mark-model">Model</span>
      <span id="setup-mark-mcp">MCP</span>
    </div>
    <div id="setup-endpoint">
      <h2>Endpoint</h2>
      <p>An OpenAI-compatible base URL. Leave the key blank to keep one that is already saved.</p>
      <label>Base URL<input id="setup-url" type="url" placeholder="https://api.example.com/v1" autocomplete="off"></label>
      <label>API key<input id="setup-key" type="password" placeholder="API key" autocomplete="off"></label>
      <button id="setup-save-endpoint" class="primary" type="button">Continue</button>
    </div>
    <div id="setup-model" hidden>
      <h2>Model</h2>
      <p>The model for this session. A saved model is selected on its own.</p>
      <select id="setup-models"></select>
      <label id="setup-model-typed">Model id<input id="setup-model-id" placeholder="model id" autocomplete="off"></label>
      <button id="setup-save-model" class="primary" type="button">Continue</button>
    </div>
    <div id="setup-mcp" hidden>
      <h2>MCP</h2>
      <p>MCP lets the agent open files in this editor. Turning it on restarts gogen.</p>
      <button id="setup-mcp-on" class="primary" type="button">Turn on MCP</button>
    </div>
    <p id="setup-note"></p>
  </div>
</div>
</div>
<script nonce="${nonce}">
const vscodeApi = acquireVsCodeApi();
const status = document.getElementById("status");
const log = document.getElementById("log");
const queueEl = document.getElementById("queue");
const input = document.getElementById("input");
const send = document.getElementById("send");
const modelBtn = document.getElementById("model");
const modelLabel = document.getElementById("model-label");
const modeBtn = document.getElementById("mode");
const modeLabel = document.getElementById("mode-label");
const thinkBtn = document.getElementById("think");
const thinkLabel = document.getElementById("think-label");
const cancelBtn = document.getElementById("cancel");
const rateEl = document.getElementById("rate");
const indexSpin = document.getElementById("index-spin");
const ctxEl = document.getElementById("ctx");

function setIndexSpin(phase, line) {
  const updating = phase === "walking" || phase === "embedding" || phase === "writing";
  indexSpin.hidden = !updating;
  indexSpin.title = updating && line ? line : "Updating the code index";
}
const ctxLabel = document.getElementById("ctx-label");
const ring = document.getElementById("ring");
const nearEl = document.getElementById("near");
const menu = document.getElementById("menu");
const menuBtn = document.getElementById("menu-btn");
const toolToggle = document.getElementById("tools");
const savedView = vscodeApi.getState() || {};
let toolsCollapsed = !!savedView.toolsCollapsed;
let live = null;
let connReady = false;
let currentModel = "";
let currentMode = "act";
let contextData = null;
let catalog = [];
let compactDismissed = false;
let turnActive = false;
let serverQueue = [];

// pending-sends:begin
// pendingSendsTracker keeps the sends gogen has not answered yet, oldest
// first. gogen answers each send once: a queue_update that lists it, the
// user_acked of the turn it started, or a response. A response that names a
// queue id answers that send. Any other response outside a turn answers the
// oldest send, because gogen handles sends in order. The tracker only decides.
// The view draws.
function pendingSendsTracker() {
  const sends = new Map();
  // user_acked frames still owed to turns a queue_update reported as started.
  let startedAcks = 0;
  function take(id) {
    const text = sends.get(id);
    sends.delete(id);
    return { id, text };
  }
  function oldest() {
    const first = sends.keys().next();
    return first.done ? null : take(first.value);
  }
  return {
    add(id, text) {
      sends.set(id, text);
    },
    list() {
      return Array.from(sends, ([id, text]) => ({ id, text }));
    },
    // queue settles the sends a queue frame lists, queued or started.
    queue(items, left) {
      for (const it of items || []) sends.delete(it.id);
      for (const id of left || []) sends.delete(id);
      if (left && left.length) startedAcks++;
    },
    // acked returns the send whose turn started without being queued, or
    // null when the ack belongs to a turn a queue frame already started.
    acked() {
      if (startedAcks > 0) {
        startedAcks--;
        return null;
      }
      return oldest();
    },
    // replied returns the send a response answers, or null when the
    // response belongs to the running turn. failed means the send did not
    // run: gogen's refusals and errors start with "Error:".
    replied(queueId, reply, turnRunning) {
      let sent = null;
      if (queueId) sent = sends.has(queueId) ? take(queueId) : null;
      else if (!turnRunning && startedAcks === 0) sent = oldest();
      if (sent) sent.failed = String(reply || "").startsWith("Error:");
      return sent;
    },
    drop(id) {
      sends.delete(id);
    },
    turnEnded() {
      startedAcks = 0;
    },
    reset() {
      sends.clear();
      startedAcks = 0;
    },
  };
}
// pending-sends:end

const pending = pendingSendsTracker();

function setStatus(state, text) {
  status.textContent = text;
  status.className = state === "error" || state === "closed" ? "error" : "";
  connReady = state === "ready";
  if (state === "starting") {
    contextData = null;
    catalog = [];
    compactDismissed = false;
    paintContext();
    setTurn(false);
  }
  refreshSend();
}

function formatTokens(n) {
  if (!n || n <= 0) return "—";
  if (n < 1000) return String(n);
  const whole = Math.floor(n / 1000);
  const frac = Math.floor((n % 1000) / 100);
  return frac === 0 ? whole + "k" : whole + "." + frac + "k";
}

function formatCost(usd) {
  if (usd < 0.01) return "$" + usd.toFixed(4);
  if (usd < 1) return "$" + usd.toFixed(3);
  return "$" + usd.toFixed(2);
}

function catalogLimit() {
  const row = catalog.find((m) => m && m.id === currentModel && m.current) || catalog.find((m) => m && m.id === currentModel);
  return (row && row.contextLimit) || 0;
}

function catalogPricing() {
  const row = catalog.find((m) => m && m.id === currentModel && m.current) || catalog.find((m) => m && m.id === currentModel);
  if (!row || (!row.inputPricePer1M && !row.outputPricePer1M)) return null;
  return row;
}

function paintContext() {
  const data = contextData || {};
  const used = data.usedTokens || 0;
  let limit = data.contextLimit || 0;
  if (!limit) limit = catalogLimit();
  let pct = 0;
  if (limit <= 0) {
    ctxLabel.textContent = "—";
    ctxEl.removeAttribute("data-tone");
    ctxEl.title = "Context usage unknown";
  } else if (used <= 0) {
    ctxLabel.textContent = "—/" + formatTokens(limit);
    ctxEl.removeAttribute("data-tone");
    ctxEl.title = "Nothing counted yet. Limit " + limit.toLocaleString() + " tokens.";
  } else {
    pct = Math.min(100, Math.round((used / limit) * 100));
    ctxLabel.textContent = pct + "%";
    if (pct >= 90) ctxEl.setAttribute("data-tone", "danger");
    else if (pct >= 75) ctxEl.setAttribute("data-tone", "warning");
    else ctxEl.removeAttribute("data-tone");
    ctxEl.title = contextTitle(data, limit);
  }
  ring.style.setProperty("--pct", pct + "%");
  nearEl.hidden = compactDismissed || !data.warnNearCompact;
}

function contextTitle(data, limit) {
  const lines = ["Context"];
  lines.push("Used: " + formatTokens(data.usedTokens));
  lines.push("Limit: " + formatTokens(limit));
  if (data.promptTokens) lines.push("Prompt: " + formatTokens(data.promptTokens));
  if (data.completionTokens) lines.push("Completion: " + formatTokens(data.completionTokens));
  if (data.cachedTokens) lines.push("Cached: " + formatTokens(data.cachedTokens));
  if (data.compactAt) lines.push("Compact at: " + formatTokens(data.compactAt));
  if (data.messageCount) lines.push("Messages: " + data.messageCount);
  const prompt = data.totalPromptTokens || 0;
  const completion = data.totalCompletionTokens || 0;
  const cached = data.totalCachedTokens || 0;
  const turns = data.totalTurns || 0;
  if (turns || prompt) {
    lines.push("Session: " + formatTokens(prompt + completion) + " · " + turns + " turns");
    const price = catalogPricing();
    if (price) {
      const billable = Math.max(0, prompt - cached);
      const cost = (billable / 1e6) * (price.inputPricePer1M || 0)
        + (cached / 1e6) * (price.cachedPricePer1M || 0)
        + (completion / 1e6) * (price.outputPricePer1M || 0);
      lines.push("Est. cost: " + formatCost(cost));
    }
  }
  return lines.join("\\n");
}

function setModeLabel(mode) {
  currentMode = (mode || "act").toLowerCase() === "plan" ? "plan" : "act";
  modeLabel.textContent = currentMode === "plan" ? "Plan" : "Act";
  modeBtn.disabled = !connReady;
  modeBtn.title = currentMode === "plan" ? "Plan mode. Click for Act." : "Act mode. Click for Plan.";
}

function setThink(level, efforts, unsupported) {
  if (unsupported) {
    thinkBtn.hidden = true;
    return;
  }
  thinkBtn.hidden = false;
  thinkBtn.disabled = !connReady;
  const value = level || "off";
  const label = value === "off" ? "Off" : value.charAt(0).toUpperCase() + value.slice(1);
  thinkLabel.textContent = label;
  thinkBtn.title = "Thinking: " + label;
  thinkBtn.classList.toggle("on", value !== "off");
}

function setTurn(active) {
  turnActive = !!active;
  cancelBtn.hidden = !turnActive;
  if (!turnActive) {
    rateEl.textContent = "";
    // A turn that failed before its user_acked owes nothing after it ends.
    pending.turnEnded();
  }
}

function setRate(tokensPerSec) {
  const n = Number(tokensPerSec) || 0;
  rateEl.textContent = turnActive && n >= 1 ? Math.round(n) + " tok/s" : "";
}

function refreshSend() {
  send.disabled = !connReady || !currentModel;
  modelBtn.disabled = !connReady;
  modeBtn.disabled = !connReady;
  thinkBtn.disabled = !connReady || thinkBtn.hidden;
}

function setModel(label, model) {
  currentModel = model || "";
  const text = label || "Choose a model";
  modelLabel.textContent = text;
  modelBtn.title = text;
  refreshSend();
}

function fitInput() {
  input.style.height = "0px";
  input.style.height = Math.min(Math.max(input.scrollHeight, 52), 140) + "px";
}

function add(className, text) {
  const el = document.createElement("div");
  el.className = "msg " + className;
  el.textContent = text;
  log.appendChild(el);
  log.scrollTop = log.scrollHeight;
  return el;
}

function clipTool(text) {
  const body = String(text || "");
  return body.length > 2000 ? body.slice(0, 2000) + "…" : body;
}

function addTool(name) {
  const el = document.createElement("div");
  el.className = "msg tool";
  const head = document.createElement("div");
  head.className = "tool-head";
  head.textContent = name || "tool";
  const body = document.createElement("pre");
  body.className = "tool-body";
  el.append(head, body);
  log.appendChild(el);
  log.scrollTop = log.scrollHeight;
  refreshToolToggle();
  return el;
}

function setToolResult(el, text, success) {
  const head = el.querySelector(".tool-head");
  const body = el.querySelector(".tool-body");
  if (!success) {
    el.classList.add("failed");
    if (head && !head.textContent.endsWith(" failed")) head.textContent += " failed";
  }
  if (body) body.textContent = clipTool(text);
  log.scrollTop = log.scrollHeight;
}

function refreshToolToggle() {
  const count = log.querySelectorAll(".tool").length;
  toolToggle.hidden = count === 0;
  toolToggle.textContent = toolsCollapsed ? "Show tools" : "Hide tools";
  document.body.classList.toggle("tools-collapsed", toolsCollapsed);
}

function renderHistory(entries) {
  // A full history frame follows clear_chat. Replacing the log is the clear.
  // Unanswered sends go with the old transcript. The queue rows stay:
  // session_state, which carries the queue, arrives before this frame.
  log.replaceChildren();
  live = null;
  pending.reset();
  paintQueue();
  const cards = new Map();
  for (const entry of entries || []) {
    if (entry.role === "user") {
      if (entry.content) add("user", entry.content);
      continue;
    }
    if (entry.role === "assistant") {
      if (entry.reasoning) add("thinking", entry.reasoning);
      if (entry.content) add("assistant", entry.content);
      for (const call of entry.toolCalls || []) {
        const card = addTool(call.name || "tool");
        if (call.id) cards.set(call.id, card);
      }
      continue;
    }
    if (entry.role === "tool") {
      const card = (entry.toolCallId && cards.get(entry.toolCallId)) || addTool("tool");
      setToolResult(card, entry.content || "", true);
    }
  }
  refreshToolToggle();
}

function ensureLive() {
  if (!live) {
    live = { thinking: null, answer: null, tool: null };
  }
  return live;
}

let setupProviders = [];
let setupModel = "";
let setupMcp = "";
let setupKnown = false;
let setupPending = "";

function setupStep() {
  if (!setupKnown) {
    return "";
  }
  const endpoint = setupProviders.some((p) => p && p.baseUrl);
  if (!endpoint) {
    return "endpoint";
  }
  if (!setupModel) {
    return "model";
  }
  if (String(setupMcp || "").toLowerCase() !== "on") {
    return "mcp";
  }
  return "";
}

function fillSetupModels() {
  const sel = document.getElementById("setup-models");
  const typed = document.getElementById("setup-model-typed");
  const has = catalog.length > 0;
  sel.hidden = !has;
  typed.hidden = has;
  if (!has) {
    return;
  }
  const current = sel.value;
  sel.replaceChildren();
  for (const model of catalog) {
    const provider = model.provider || "default";
    const opt = document.createElement("option");
    opt.value = provider + "\t" + model.id;
    opt.textContent = provider !== "default" ? provider + " · " + model.id : model.id;
    sel.appendChild(opt);
  }
  if (current && Array.from(sel.options).some((opt) => opt.value === current)) {
    sel.value = current;
  }
}

function paintSetup() {
  const step = setupStep();
  document.getElementById("setup").hidden = !step;
  const order = { endpoint: 0, model: 1, mcp: 2 };
  for (const name of ["endpoint", "model", "mcp"]) {
    const mark = document.getElementById("setup-mark-" + name);
    const panel = document.getElementById("setup-" + name);
    mark.className = step && order[name] < order[step] ? "done" : name === step ? "now" : "";
    panel.hidden = name !== step;
  }
  if (step === "model") {
    fillSetupModels();
  }
  if (step === "mcp") {
    const button = document.getElementById("setup-mcp-on");
    button.disabled = false;
    button.textContent = "Turn on MCP";
  }
}

function setMenu(open) {
  menu.hidden = !open;
  menuBtn.setAttribute("aria-expanded", open ? "true" : "false");
}

const pathAsk = document.getElementById("path-ask");
let pathAskID = null;

function showPathAsk(msg) {
  pathAskID = msg.id;
  document.getElementById("path-ask-file").textContent = msg.path || "";
  pathAsk.hidden = false;
  document.getElementById("path-deny").focus();
}

function hidePathAsk(id) {
  if (id !== undefined && id !== null && id !== pathAskID) {
    return;
  }
  pathAsk.hidden = true;
  pathAskID = null;
}

function answerPath(allow) {
  const id = pathAskID;
  if (id === null) {
    return;
  }
  hidePathAsk(id);
  vscodeApi.postMessage({ type: "pathAnswer", id, allow });
}

window.addEventListener("message", (event) => {
  const msg = event.data || {};
  if (msg.type === "pathApproval") {
    if (msg.hide) {
      hidePathAsk(msg.id);
    } else {
      showPathAsk(msg);
    }
    return;
  }
  if (msg.type === "restoreInput") {
    // The send never reached gogen: the drawn copy goes, the draft comes back.
    if (msg.queueId) unsend(msg.queueId);
    restoreDraft(msg.text);
    paintQueue();
    return;
  }
  if (msg.type === "status") {
    setStatus(msg.state, msg.text);
    return;
  }
  if (msg.type === "workspace") {
    const folderBtn = document.getElementById("workspace");
    folderBtn.textContent = msg.label || "Folder";
    folderBtn.title = msg.folder || "";
    return;
  }
  if (msg.type === "config" || msg.type === "models") {
    setModel(msg.modelLabel, msg.model);
    if (msg.type === "models" && Array.isArray(msg.models)) catalog = msg.models;
    if (msg.type === "config") {
      if (typeof msg.codeIndexPhase === "string") {
        setIndexSpin(msg.codeIndexPhase, msg.codeIndexLine || "");
      }
      setupKnown = true;
      if (Array.isArray(msg.providers)) setupProviders = msg.providers;
      setupModel = msg.model || "";
      setupMcp = msg.mcp || "";
      if (msg.mode) setModeLabel(msg.mode);
      if (msg.mode || msg.model || msg.thinkingLevel || msg.reasoningEfforts || msg.reasoningEffortsUnsupported) {
        setThink(msg.thinkingLevel || "off", msg.reasoningEfforts, !!msg.reasoningEffortsUnsupported);
      }
      const note = document.getElementById("setup-note");
      if (setupPending && setupStep() === setupPending) {
        note.textContent = setupPending === "endpoint"
          ? "The endpoint was not saved. Check the URL."
          : "The model was not saved.";
      } else if (setupPending) {
        setupPending = "";
        note.textContent = "";
      }
    }
    paintContext();
    paintSetup();
    return;
  }
  if (msg.type === "context") {
    if (!msg.warnNearCompact) compactDismissed = false;
    contextData = msg;
    paintContext();
    return;
  }
  if (msg.type === "stats") {
    setRate(msg.tokensPerSec);
    return;
  }
  if (msg.type === "turn") {
    setTurn(msg.active);
    setRate(msg.tokensPerSec);
    return;
  }
  if (msg.type === "history") {
    renderHistory(msg.entries);
    return;
  }
  if (msg.type === "queue") {
    applyQueue(msg.items, msg.left, msg.text);
    return;
  }
  if (msg.type === "userAcked") {
    takePendingSend();
    return;
  }
  if (msg.type === "thinking") {
    setTurn(true);
    const box = ensureLive();
    // A tool call closed the previous bubble. This stretch is a new one,
    // placed under that call. Switching to an answer does the same.
    if (!box.thinking) {
      box.answer = null;
      box.thinking = add("thinking", "");
    }
    box.thinking.textContent += msg.text;
    log.scrollTop = log.scrollHeight;
    return;
  }
  if (msg.type === "token") {
    setTurn(true);
    const box = ensureLive();
    if (!box.answer) {
      box.thinking = null;
      box.answer = add("assistant", "");
    }
    box.answer.textContent += msg.text;
    log.scrollTop = log.scrollHeight;
    return;
  }
  if (msg.type === "codeIndex") {
    setIndexSpin(msg.phase, msg.line);
    if (live && live.tool) {
      const head = live.tool.querySelector(".tool-head");
      const body = live.tool.querySelector(".tool-body");
      if (head && body && head.textContent === "search_meaning" && msg.line) {
        body.textContent = msg.line;
      }
    }
    return;
  }
  if (msg.type === "tool") {
    setTurn(true);
    const box = ensureLive();
    box.thinking = null;
    box.answer = null;
    box.tool = addTool(msg.name);
    return;
  }
  if (msg.type === "toolResult") {
    const box = ensureLive();
    const target = box.tool || addTool(msg.name);
    setToolResult(target, msg.text, msg.success !== false);
    box.tool = null;
    return;
  }
  if (msg.type === "notice") {
    if (msg.reply) settleReply(msg.queueId, msg.text);
    if (msg.text) add("notice", msg.text);
    // A refused Send now: repainting gives the button back.
    if (msg.kind === "queue") paintQueue();
    return;
  }
  if (msg.type === "turnEnd") {
    live = null;
  }
});

toolToggle.addEventListener("click", () => {
  toolsCollapsed = !toolsCollapsed;
  vscodeApi.setState(Object.assign({}, vscodeApi.getState(), { toolsCollapsed }));
  refreshToolToggle();
});
refreshToolToggle();
modelBtn.addEventListener("click", () => vscodeApi.postMessage({ type: "pickModel" }));
modeBtn.addEventListener("click", () => vscodeApi.postMessage({ type: "setMode", mode: currentMode === "plan" ? "act" : "plan" }));
thinkBtn.addEventListener("click", () => vscodeApi.postMessage({ type: "pickThinking" }));
cancelBtn.addEventListener("click", () => vscodeApi.postMessage({ type: "cancel" }));
document.getElementById("compact").addEventListener("click", () => vscodeApi.postMessage({ type: "compact" }));
document.getElementById("dismiss").addEventListener("click", () => { compactDismissed = true; nearEl.hidden = true; });
document.getElementById("settings").addEventListener("click", () => vscodeApi.postMessage({ type: "openSettings" }));
document.getElementById("board").addEventListener("click", () => vscodeApi.postMessage({ type: "openBoard" }));
document.getElementById("workspace").addEventListener("click", () => vscodeApi.postMessage({ type: "openWorkspace" }));
document.getElementById("setup-save-endpoint").addEventListener("click", () => {
  const baseUrl = document.getElementById("setup-url").value.trim();
  const note = document.getElementById("setup-note");
  if (!baseUrl) {
    note.textContent = "A base URL is required.";
    return;
  }
  setupPending = "endpoint";
  note.textContent = "Saving…";
  vscodeApi.postMessage({ type: "setupEndpoint", baseUrl, apiKey: document.getElementById("setup-key").value });
});
document.getElementById("setup-save-model").addEventListener("click", () => {
  const note = document.getElementById("setup-note");
  if (catalog.length) {
    const value = document.getElementById("setup-models").value || "";
    const split = value.indexOf("\t");
    if (split < 0) {
      note.textContent = "Choose a model.";
      return;
    }
    setupPending = "model";
    note.textContent = "Saving…";
    vscodeApi.postMessage({ type: "setupModel", provider: value.slice(0, split), id: value.slice(split + 1) });
    return;
  }
  const id = document.getElementById("setup-model-id").value.trim();
  if (!id) {
    note.textContent = "A model id is required.";
    return;
  }
  setupPending = "model";
  note.textContent = "Saving…";
  vscodeApi.postMessage({ type: "setupModel", provider: "default", id });
});
document.getElementById("setup-mcp-on").addEventListener("click", () => {
  const button = document.getElementById("setup-mcp-on");
  button.disabled = true;
  button.textContent = "Restarting…";
  document.getElementById("setup-note").textContent = "Turning MCP on and restarting gogen.";
  vscodeApi.postMessage({ type: "setupMcp" });
});
menuBtn.addEventListener("click", (event) => {
  event.stopPropagation();
  setMenu(menu.hidden);
});
document.addEventListener("click", () => setMenu(false));
document.getElementById("form").addEventListener("submit", (event) => {
  event.preventDefault();
  const text = input.value.trim();
  if (!text || send.disabled) return;
  const id = newQueueId();
  pending.add(id, text);
  if (turnActive) {
    paintQueue();
  } else {
    // The server may still be in the turn when this view thinks it is idle.
    // The bubble is tagged so a queue frame can lift it back out of the
    // transcript instead of leaving a copy that scrolls away.
    live = null;
    const el = add("user", text);
    el.dataset.qid = id;
  }
  vscodeApi.postMessage({ type: "send", text, queueId: id });
  input.value = "";
  fitInput();
});

// newQueueId need not match the web client's ids. Both only have to pass the
// server's queue-id rule, which queueId.js and its test pin.
function newQueueId() {
  const bytes = new Uint8Array(8);
  crypto.getRandomValues(bytes);
  let hex = "";
  for (const b of bytes) hex += b.toString(16).padStart(2, "0");
  return "q-" + hex;
}

// paintQueue pins the server's queue, then the sends gogen has not answered
// yet. A pending send drawn in the transcript (sent while idle) is not pinned.
function paintQueue() {
  queueEl.replaceChildren();
  const shown = serverQueue.slice();
  for (const it of pending.list()) {
    if (!logBubble(it.id)) shown.push(it);
  }
  shown.forEach((it, i) => {
    const row = document.createElement("div");
    row.className = "msg user queued";
    const body = document.createElement("div");
    body.textContent = it.text;
    const actions = document.createElement("div");
    actions.className = "queue-actions";
    if (i === shown.length - 1) {
      const now = document.createElement("button");
      now.type = "button";
      now.textContent = "Send now";
      now.addEventListener("click", () => {
        now.disabled = true;
        now.textContent = "Sending…";
        vscodeApi.postMessage({ type: "queueSendNow", id: it.id });
      });
      actions.append(now);
    }
    const remove = document.createElement("button");
    remove.type = "button";
    remove.textContent = "Remove";
    remove.addEventListener("click", () => {
      vscodeApi.postMessage({ type: "queueRemove", id: it.id });
    });
    actions.append(remove);
    row.append(body, actions);
    queueEl.append(row);
  });
}

function logBubble(id) {
  return log.querySelector('.user[data-qid="' + CSS.escape(id) + '"]');
}

// unsend forgets a send and removes its early copy from the transcript.
function unsend(id) {
  pending.drop(id);
  const premature = logBubble(id);
  if (premature) premature.remove();
}

// restoreDraft puts the text of a send that did not run back into the
// composer, ahead of anything typed since.
function restoreDraft(text) {
  if (!text) return;
  input.value = input.value ? text + "\\n\\n" + input.value : text;
  fitInput();
}

// drawSent shows a send that ran, unless it is already in the transcript.
function drawSent(id, text) {
  if (logBubble(id)) return;
  live = null;
  const el = add("user", text);
  el.dataset.qid = id;
}

// applyQueue takes the server's queue. left lists the ids whose turn just
// started, and text is the one message that turn runs. The queue rows show
// queued sends, and the started turn is drawn once, so the early copies go.
function applyQueue(items, left, text) {
  const next = [];
  for (const it of items || []) {
    if (it && typeof it.id === "string" && typeof it.text === "string") next.push(it);
  }
  pending.queue(next, left);
  for (const it of next) unsend(it.id);
  if (left && left.length) {
    left.forEach(unsend);
    live = null;
    add("user", text);
  }
  serverQueue = next;
  paintQueue();
}

// takePendingSend handles user_acked.
function takePendingSend() {
  const sent = pending.acked();
  if (sent) drawSent(sent.id, sent.text);
  paintQueue();
}

// settleReply handles a response. A send that did not run goes back into the
// composer. Any other answered send was a command gogen handled without a
// turn.
function settleReply(queueId, text) {
  const sent = pending.replied(queueId, text, turnActive);
  if (!sent) return;
  if (sent.failed) {
    unsend(sent.id);
    restoreDraft(sent.text);
  } else {
    drawSent(sent.id, sent.text);
  }
  paintQueue();
}
input.addEventListener("input", fitInput);
input.addEventListener("keydown", (event) => {
  if (event.key === "Enter" && !event.shiftKey) {
    event.preventDefault();
    document.getElementById("form").requestSubmit();
  }
});
document.getElementById("path-allow").addEventListener("click", () => answerPath(true));
document.getElementById("path-deny").addEventListener("click", () => answerPath(false));
pathAsk.addEventListener("keydown", (event) => {
  if (event.key === "Escape") {
    event.preventDefault();
    answerPath(false);
  }
});
</script>
</body>
</html>`;
}

module.exports = { ChatViewProvider };
