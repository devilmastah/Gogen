const vscode = require("vscode");
const { webviewNonce } = require("./webviewNonce");

let panel = null;

function openBoard(host) {
  if (panel) {
    panel.reveal(vscode.ViewColumn.Active);
    return;
  }
  panel = vscode.window.createWebviewPanel("gogen.board", "GoGen: Board", vscode.ViewColumn.Active, {
    enableScripts: true,
    retainContextWhenHidden: true,
  });
  panel.webview.html = boardHtml(panel.webview);
  const send = (event) => panel?.webview.postMessage(event);
  const off = host.subscribe(send);
  panel.webview.onDidReceiveMessage((msg) => onMessage(host, msg));
  panel.onDidDispose(() => {
    off();
    panel = null;
  });
  host.ensureStarted().then(() => {
    try {
      host.boardOp({ action: "list" });
    } catch (err) {
      send({ type: "notice", kind: "board", text: err.message });
    }
  }).catch((err) => send({ type: "notice", kind: "board", text: err.message }));
}

function onMessage(host, msg) {
  if (!msg) {
    return;
  }
  try {
    if (msg.type === "enable") {
      host.enableBoard();
      return;
    }
    if (msg.type === "refresh") {
      host.boardOp({ action: "list" });
      return;
    }
    if (msg.type === "add") {
      const title = String(msg.title || "").trim();
      if (!title) {
        return;
      }
      host.boardOp({ action: "add", title, description: String(msg.description || ""), priority: String(msg.priority || "") });
      return;
    }
    if (msg.type === "move") {
      host.boardOp({ action: "move", id: String(msg.id || ""), column: String(msg.column || "") });
      return;
    }
    if (msg.type === "remove") {
      host.boardOp({ action: "remove", id: String(msg.id || "") });
      return;
    }
    if (msg.type === "comment") {
      const text = String(msg.text || "").trim();
      if (!text) {
        return;
      }
      host.boardOp({ action: "comment", id: String(msg.id || ""), text });
      return;
    }
    if (msg.type === "update") {
      host.boardOp({
        action: "update",
        id: String(msg.id || ""),
        title: String(msg.title || ""),
        description: String(msg.description || ""),
        priority: String(msg.priority || ""),
        context: String(msg.context || ""),
      });
      return;
    }
    if (msg.type === "block") {
      const reason = String(msg.reason || "").trim();
      if (!reason) {
        return;
      }
      host.boardOp({ action: "block", id: String(msg.id || ""), reason });
      return;
    }
    if (msg.type === "done") {
      host.boardOp({ action: "done", id: String(msg.id || "") });
      return;
    }
    if (msg.type === "start") {
      host.boardOp({ action: "start", id: String(msg.id || "") });
      return;
    }
    if (msg.type === "openAgent") {
      host.openSession(String(msg.id || ""));
    }
  } catch (err) {
    host.emit({ type: "notice", kind: "board", text: err.message });
  }
}

function boardHtml(webview) {
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
  :root { color-scheme: light dark; }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    height: 100vh;
    display: flex;
    flex-direction: column;
    font-family: var(--vscode-font-family);
    font-size: 13px;
    color: var(--vscode-foreground);
    background: var(--vscode-editor-background);
  }
  button { font: inherit; cursor: pointer; }
  button:disabled { opacity: 0.45; cursor: default; }
  #bar { padding: 12px 14px 4px; }
  #composer {
    border: 1px solid var(--vscode-widget-border, var(--vscode-panel-border));
    border-radius: 14px;
    background: var(--vscode-input-background);
  }
  #composer:focus-within { border-color: var(--vscode-focusBorder); }
  #title, #desc {
    display: block;
    width: 100%;
    border: 0;
    outline: none;
    background: transparent;
    color: inherit;
    font: inherit;
  }
  #title { padding: 10px 12px 2px; }
  #add-row { display: flex; align-items: center; gap: 6px; padding: 4px 6px 6px; }
  #desc { flex: 1; min-width: 0; padding: 6px; }
  #title::placeholder, #desc::placeholder { color: var(--vscode-input-placeholderForeground); }
  select, #detail input, #detail textarea, #detail select {
    font: inherit;
    color: inherit;
    background: color-mix(in srgb, var(--vscode-foreground) 6%, transparent);
    border: 0;
    border-radius: 8px;
  }
  #pri {
    height: 26px;
    padding: 0 8px;
    border-radius: 999px;
    background: color-mix(in srgb, var(--vscode-foreground) 8%, transparent);
  }
  button.primary {
    border: 0;
    border-radius: 999px;
    padding: 4px 12px;
    background: var(--vscode-button-background);
    color: var(--vscode-button-foreground);
  }
  button.primary:hover:not(:disabled) { background: var(--vscode-button-hoverBackground); }
  button.quiet {
    border: 0;
    border-radius: 999px;
    padding: 4px 10px;
    background: color-mix(in srgb, var(--vscode-foreground) 8%, transparent);
    color: var(--vscode-foreground);
  }
  button.quiet:hover { background: color-mix(in srgb, var(--vscode-foreground) 14%, transparent); }
  #enable[hidden] { display: none; }
  #note {
    margin: 8px 14px 0;
    padding: 6px 10px;
    border-radius: 8px;
    font-size: 12px;
    color: var(--vscode-descriptionForeground);
    background: color-mix(in srgb, var(--vscode-foreground) 5%, transparent);
  }
  #note:empty { display: none; }
  #note.error { color: var(--vscode-errorForeground); background: var(--vscode-inputValidation-errorBackground, transparent); }
  #wrap { flex: 1; display: flex; min-height: 0; }
  #board { flex: 1; display: flex; gap: 10px; padding: 12px 14px 14px; overflow: auto; }
  .col {
    flex: 1 0 180px;
    min-width: 180px;
    display: flex;
    flex-direction: column;
    border-radius: 14px;
    background: color-mix(in srgb, var(--vscode-foreground) 4%, transparent);
  }
  .col h3 {
    display: flex;
    align-items: center;
    gap: 8px;
    margin: 0;
    padding: 10px 12px 6px;
    font-size: 11px;
    font-weight: 650;
    letter-spacing: 0.06em;
    text-transform: uppercase;
  }
  .count {
    margin-left: auto;
    letter-spacing: 0;
    font-weight: 500;
    color: var(--vscode-descriptionForeground);
  }
  .cards { flex: 1; min-height: 72px; padding: 4px 8px 10px; display: flex; flex-direction: column; gap: 8px; border-radius: 12px; }
  .cards.over { background: color-mix(in srgb, var(--vscode-focusBorder) 14%, transparent); }
  .card {
    padding: 10px 10px 8px;
    border-radius: 12px;
    background: var(--vscode-editor-background);
    border: 1px solid color-mix(in srgb, var(--vscode-foreground) 8%, transparent);
    cursor: pointer;
  }
  .card:hover { border-color: color-mix(in srgb, var(--vscode-focusBorder) 55%, transparent); }
  .card.open { border-color: var(--vscode-focusBorder); }
  .card .title { font-weight: 600; line-height: 1.35; }
  .card .id { display: flex; align-items: center; gap: 6px; margin-top: 6px; font-size: 11px; color: var(--vscode-descriptionForeground); }
  .pri {
    padding: 1px 6px;
    border-radius: 999px;
    background: color-mix(in srgb, var(--vscode-foreground) 8%, transparent);
    color: var(--vscode-foreground);
  }
  .pri-high, .pri-urgent { color: var(--vscode-errorForeground); background: color-mix(in srgb, var(--vscode-errorForeground) 14%, transparent); }
  .pri-low { color: var(--vscode-descriptionForeground); }
  #detail {
    width: min(380px, 46vw);
    overflow: auto;
    margin: 12px 14px 14px 0;
    padding: 16px;
    border-radius: 14px;
    background: color-mix(in srgb, var(--vscode-foreground) 4%, transparent);
  }
  #detail[hidden] { display: none; }
  #detail h2 { margin: 0; font-size: 16px; font-weight: 650; line-height: 1.35; }
  #detail .meta { display: flex; flex-wrap: wrap; gap: 6px; align-items: center; margin: 8px 0 14px; font-size: 12px; color: var(--vscode-descriptionForeground); }
  #detail h3 { margin: 16px 0 8px; font-size: 11px; font-weight: 650; letter-spacing: 0.06em; text-transform: uppercase; color: var(--vscode-descriptionForeground); }
  #detail p, #detail .body { white-space: pre-wrap; word-break: break-word; margin: 0; line-height: 1.5; }
  #detail .empty { color: var(--vscode-descriptionForeground); }
  .activity { display: flex; flex-direction: column; gap: 6px; }
  .activity div {
    padding: 6px 8px;
    border-radius: 8px;
    background: color-mix(in srgb, var(--vscode-foreground) 4%, transparent);
    font-size: 12px;
    line-height: 1.4;
  }
  .activity .when { color: var(--vscode-descriptionForeground); margin-right: 6px; }
  #detail textarea, #detail input, #detail select { width: 100%; margin-top: 8px; padding: 8px 10px; }
  #detail textarea { min-height: 72px; resize: vertical; }
  #detail .actions { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 12px; }
  #detail .head { display: flex; align-items: flex-start; gap: 8px; }
  #detail .head h2 { flex: 1; }
  #close { flex: 0 0 auto; }
  #comment { margin-top: 14px; }
</style>
</head>
<body>
<div id="bar">
  <div id="composer">
    <input id="title" type="text" placeholder="New ticket">
    <div id="add-row">
      <input id="desc" type="text" placeholder="Description">
      <select id="pri"><option value="">priority</option><option>low</option><option>normal</option><option>high</option></select>
      <button id="add" class="primary" type="button">Add</button>
      <button id="enable" class="primary" type="button" hidden>Turn board on</button>
    </div>
  </div>
</div>
<div id="note"></div>
<div id="wrap">
  <div id="board"></div>
  <aside id="detail" hidden></aside>
</div>
<script nonce="${nonce}">
const vscodeApi = acquireVsCodeApi();
const board = document.getElementById("board");
const note = document.getElementById("note");
const detail = document.getElementById("detail");
let lastState = null;
let openId = "";
let editing = false;
let blocking = false;
let commentDraft = "";

function label(column) {
  return String(column || "").replaceAll("_", " ");
}

function relTime(iso) {
  if (!iso) return "";
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "";
  const mins = Math.round((Date.now() - then) / 60000);
  if (mins < 1) return "just now";
  if (mins < 60) return mins + "m ago";
  const hours = Math.round(mins / 60);
  if (hours < 24) return hours + "h ago";
  const days = Math.round(hours / 24);
  if (days < 7) return days + "d ago";
  return new Date(iso).toLocaleDateString();
}

function findItem(id) {
  const items = (lastState && lastState.items) || [];
  return items.find((item) => item && item.id === id) || null;
}

function openTicket(id) {
  if (openId === id) {
    openId = "";
    editing = false;
    blocking = false;
  } else {
    openId = id;
    editing = false;
    blocking = false;
    commentDraft = "";
  }
  render(lastState);
}

function renderDetail(item) {
  detail.replaceChildren();
  if (!item) {
    detail.hidden = true;
    return;
  }
  detail.hidden = false;
  const head = document.createElement("div");
  head.className = "head";
  const title = document.createElement("h2");
  title.textContent = item.title || "(untitled)";
  const close = document.createElement("button");
  close.id = "close";
  close.type = "button";
  close.className = "quiet";
  close.textContent = "Close";
  close.addEventListener("click", () => openTicket(item.id));
  head.append(title, close);
  const meta = document.createElement("div");
  meta.className = "meta";
  meta.append(chip("#" + item.id), chip(label(item.status)));
  if (item.priority) {
    const pri = document.createElement("span");
    pri.className = "pri pri-" + item.priority;
    pri.textContent = item.priority;
    meta.appendChild(pri);
  }
  if (item.assignee) meta.appendChild(chip(item.assignee));
  detail.append(head, meta);

  if (editing) {
    detail.appendChild(editForm(item));
  } else {
    const desc = document.createElement("p");
    desc.className = "body" + (item.description ? "" : " empty");
    desc.textContent = item.description || "No description";
    detail.appendChild(desc);
    if (item.context) {
      const h = document.createElement("h3");
      h.textContent = "Context";
      const body = document.createElement("p");
      body.className = "body";
      body.textContent = item.context;
      detail.append(h, body);
    }
  }

  const activity = item.activity || [];
  if (activity.length) {
    const h = document.createElement("h3");
    h.textContent = "Activity";
    const list = document.createElement("div");
    list.className = "activity";
    for (const act of activity.slice(-12)) {
      const row = document.createElement("div");
      const when = document.createElement("span");
      when.className = "when";
      when.textContent = relTime(act.at);
      when.title = act.at ? new Date(act.at).toLocaleString() : "";
      row.append(when, document.createTextNode((act.by || "?") + ": " + (act.text || "")));
      list.appendChild(row);
    }
    detail.append(h, list);
  }

  const comment = document.createElement("textarea");
  comment.id = "comment";
  comment.placeholder = "Add a comment";
  comment.value = commentDraft;
  comment.addEventListener("input", () => { commentDraft = comment.value; });
  const commentBtn = document.createElement("button");
  commentBtn.type = "button";
  commentBtn.className = "primary";
  commentBtn.textContent = "Comment";
  commentBtn.addEventListener("click", () => {
    const text = comment.value.trim();
    if (!text) {
      comment.focus();
      return;
    }
    commentDraft = "";
    vscodeApi.postMessage({ type: "comment", id: item.id, text });
  });
  const actions = document.createElement("div");
  actions.className = "actions";
  actions.appendChild(actionButton(editing ? "Cancel edit" : "Edit", () => {
    editing = !editing;
    blocking = false;
    render(lastState);
  }));
  if (!editing) {
    actions.appendChild(actionButton(blocking ? "Cancel block" : "Block", () => {
      blocking = !blocking;
      render(lastState);
    }));
    if (item.status !== "done") {
      actions.appendChild(actionButton("Done", () => vscodeApi.postMessage({ type: "done", id: item.id })));
    }
    if (item.agentSession) {
      actions.appendChild(actionButton("Open in chat", () => vscodeApi.postMessage({ type: "openAgent", id: item.agentSession }), "primary"));
    } else if (item.status !== "done" && !item.assignee) {
      actions.appendChild(actionButton("Start", () => vscodeApi.postMessage({ type: "start", id: item.id }), "primary"));
    }
    actions.appendChild(actionButton("Remove", () => vscodeApi.postMessage({ type: "remove", id: item.id })));
  }
  detail.append(comment, commentBtn, actions);
  if (blocking && !editing) {
    detail.appendChild(blockForm(item));
  }
}

function chip(text) {
  const el = document.createElement("span");
  el.className = "pri";
  el.textContent = text;
  return el;
}

function actionButton(text, onClick, kind) {
  const btn = document.createElement("button");
  btn.type = "button";
  btn.textContent = text;
  btn.className = kind === "primary" ? "primary" : "quiet";
  btn.addEventListener("click", onClick);
  return btn;
}

function editForm(item) {
  const box = document.createElement("div");
  const title = document.createElement("input");
  title.type = "text";
  title.value = item.title || "";
  const desc = document.createElement("textarea");
  desc.placeholder = "Description";
  desc.value = item.description || "";
  const context = document.createElement("textarea");
  context.placeholder = "Context for the agent";
  context.value = item.context || "";
  const pri = document.createElement("select");
  for (const value of ["", "low", "medium", "high", "urgent"]) {
    const opt = document.createElement("option");
    opt.value = value;
    opt.textContent = value || "priority";
    pri.appendChild(opt);
  }
  pri.value = item.priority || "";
  const save = actionButton("Save", () => {
    const next = title.value.trim();
    if (!next) {
      title.focus();
      return;
    }
    editing = false;
    vscodeApi.postMessage({
      type: "update",
      id: item.id,
      title: next,
      description: desc.value.trim(),
      context: context.value.trim(),
      priority: pri.value,
    });
  }, "primary");
  box.append(title, desc, context, pri, save);
  return box;
}

function blockForm(item) {
  const box = document.createElement("div");
  const reason = document.createElement("textarea");
  reason.placeholder = "Why is this blocked?";
  const go = actionButton("Confirm block", () => {
    const text = reason.value.trim();
    if (!text) {
      reason.focus();
      return;
    }
    blocking = false;
    vscodeApi.postMessage({ type: "block", id: item.id, reason: text });
  }, "primary");
  box.append(reason, go);
  return box;
}

function render(state) {
  lastState = state;
  const columns = (state && state.columns) || [];
  const items = (state && state.items) || [];
  if (openId && !findItem(openId)) {
    openId = "";
    editing = false;
    blocking = false;
  }
  board.replaceChildren();
  for (const column of columns) {
    const col = document.createElement("section");
    col.className = "col";
    const h = document.createElement("h3");
    const name = document.createElement("span");
    name.textContent = label(column);
    const count = document.createElement("span");
    count.className = "count";
    count.textContent = String(items.filter((entry) => entry && entry.status === column).length);
    h.append(name, count);
    const cards = document.createElement("div");
    cards.className = "cards";
    cards.dataset.column = column;
    cards.addEventListener("dragover", (event) => {
      event.preventDefault();
      cards.classList.add("over");
    });
    cards.addEventListener("dragleave", () => cards.classList.remove("over"));
    cards.addEventListener("drop", (event) => {
      event.preventDefault();
      cards.classList.remove("over");
      const id = event.dataTransfer.getData("text/plain");
      if (id) vscodeApi.postMessage({ type: "move", id, column });
    });
    for (const item of items) {
      if (item.status !== column) continue;
      const card = document.createElement("article");
      card.className = "card" + (item.id === openId ? " open" : "");
      card.draggable = true;
      let dragged = false;
      card.addEventListener("dragstart", (event) => {
        dragged = true;
        event.dataTransfer.setData("text/plain", item.id);
      });
      card.addEventListener("dragend", () => {
        setTimeout(() => { dragged = false; }, 0);
      });
      card.addEventListener("click", () => {
        if (dragged) return;
        openTicket(item.id);
      });
      const title = document.createElement("div");
      title.className = "title";
      title.textContent = item.title || "(untitled)";
      const meta = document.createElement("div");
      meta.className = "id";
      meta.append(document.createTextNode("#" + item.id));
      if (item.priority) {
        const pri = document.createElement("span");
        pri.className = "pri pri-" + item.priority;
        pri.textContent = item.priority;
        meta.appendChild(pri);
      }
      card.append(title, meta);
      cards.appendChild(card);
    }
    col.append(h, cards);
    board.appendChild(col);
  }
  renderDetail(openId ? findItem(openId) : null);
}

window.addEventListener("message", (event) => {
  const msg = event.data || {};
  if (msg.type === "config" && msg.board === "off") {
    note.textContent = "The board is off for this folder.";
    note.className = "error";
    document.getElementById("enable").hidden = false;
    return;
  }
  if (msg.type === "config" && msg.board === "on") {
    const wasOff = note.textContent.indexOf("off") >= 0;
    document.getElementById("enable").hidden = true;
    if (wasOff) {
      note.textContent = "";
      note.className = "";
      vscodeApi.postMessage({ type: "refresh" });
    }
    return;
  }
  if (msg.type === "board") {
    note.textContent = "";
    note.className = "";
    document.getElementById("enable").hidden = true;
    render(msg.state);
    return;
  }
  if (msg.type === "notice" && msg.kind === "board") {
    note.textContent = msg.text;
    note.className = msg.text.indexOf("disabled") >= 0 ? "error" : "";
  }
});

document.getElementById("enable").addEventListener("click", () => {
  vscodeApi.postMessage({ type: "enable" });
});
document.getElementById("add").addEventListener("click", () => {
  const title = document.getElementById("title");
  vscodeApi.postMessage({
    type: "add",
    title: title.value,
    description: document.getElementById("desc").value,
    priority: document.getElementById("pri").value,
  });
  title.value = "";
  document.getElementById("desc").value = "";
});
</script>
</body>
</html>`;
}

module.exports = { openBoard };
