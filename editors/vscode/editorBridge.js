"use strict";

const crypto = require("crypto");
const fs = require("fs");
const http = require("http");
const path = require("path");
const vscode = require("vscode");

// pathGate is the one outside-path question in flight. The bridge child
// handles one tools/call at a time, so one question is enough. ask answers
// it. signal aborts it when gogen cancels the call.
let pathGate = {
  ask: async () => false,
  signal: null,
};

// EditorBridge is the loopback listener the stdio MCP server calls. The
// token stays in this process and in that server's environment. It is not
// written to a config file and it is not logged.
class EditorBridge {
  constructor(log, folderPath, askOutside) {
    this.log = log;
    this.folderPath = folderPath;
    this.askOutside = askOutside || (async () => false);
    this.token = crypto.randomBytes(32).toString("hex");
    this.server = null;
    this.port = 0;
  }

  start() {
    if (this.server) {
      return Promise.resolve(this.port);
    }
    const server = http.createServer((req, res) => this.onRequest(req, res));
    this.server = server;
    return new Promise((resolve, reject) => {
      let listening = false;
      // once() would handle only the first error. A later one, with no
      // listener, terminates the extension host.
      server.on("error", (err) => {
        const text = err && err.message ? err.message : String(err);
        this.log.appendLine("editor bridge: " + text);
        if (!listening) {
          this.server = null;
          reject(err);
        }
      });
      server.listen(0, "127.0.0.1", () => {
        listening = true;
        this.port = server.address().port;
        resolve(this.port);
      });
    });
  }

  stop() {
    const server = this.server;
    this.server = null;
    if (server) {
      server.close();
    }
  }

  // mcpEnv is the process-local server list for the sidecar. gogen appends
  // it to the user's servers and does not save it.
  mcpEnv(extensionPath) {
    return {
      GOGEN_MCP_EXTRA: JSON.stringify([
        {
          name: "vscode",
          command: process.execPath,
          args: [path.join(extensionPath, "bridge.js")],
          env: {
            ELECTRON_RUN_AS_NODE: "1",
            GOGEN_VSCODE_BRIDGE: "127.0.0.1:" + this.port,
            GOGEN_VSCODE_BRIDGE_TOKEN: this.token,
          },
        },
      ]),
    };
  }

  onRequest(req, res) {
    if (req.method !== "POST" || req.url !== "/call") {
      send(res, 404, { ok: false, text: "not found" });
      return;
    }
    if (!authorized(req.headers.authorization, this.token)) {
      send(res, 401, { ok: false, text: "unauthorized" });
      return;
    }
    const maxBody = 65536;
    const declared = Number(req.headers["content-length"]);
    if (Number.isFinite(declared) && declared > maxBody) {
      send(res, 413, { ok: false, text: "request is too large" });
      req.resume();
      return;
    }
    const chunks = [];
    let size = 0;
    let tooBig = false;
    const ac = new AbortController();
    // 'close' also fires after a normal response. Abort only when the client
    // went away before we answered, which is gogen cancelling the tool call.
    res.on("close", () => {
      if (!res.writableEnded) {
        ac.abort();
      }
    });
    req.on("data", (chunk) => {
      size += chunk.length;
      if (size > maxBody) {
        if (!tooBig) {
          tooBig = true;
          send(res, 413, { ok: false, text: "request is too large" });
        }
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on("end", () => {
      if (tooBig) {
        return;
      }
      let body;
      try {
        body = JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}");
      } catch {
        send(res, 400, { ok: false, text: "bad request" });
        return;
      }
      pathGate.ask = this.askOutside;
      pathGate.signal = ac.signal;
      this.dispatch(body)
        .then((text) => send(res, 200, { ok: true, text }))
        .catch((err) => send(res, 200, { ok: false, text: err && err.message ? err.message : String(err) }))
        .finally(() => {
          pathGate.signal = null;
        });
    });
  }

  async dispatch(body) {
    const name = body && body.name;
    const args = (body && body.arguments) || {};
    const folder = this.folderPath() || "";
    switch (name) {
      case "open_file":
        return openFile(folder, args);
      case "reveal_file":
        return revealFile(folder, args);
      case "editor_state":
        return editorState(folder, args);
      default:
        throw new Error("unknown tool " + name);
    }
  }
}

function authorized(header, token) {
  const want = Buffer.from("Bearer " + token);
  const got = Buffer.from(String(header || ""));
  if (got.length !== want.length) {
    return false;
  }
  return crypto.timingSafeEqual(got, want);
}

function send(res, status, payload) {
  const body = Buffer.from(JSON.stringify(payload));
  res.writeHead(status, {
    "content-type": "application/json",
    "content-length": body.length,
  });
  res.end(body);
}

// allowPath resolves a model-supplied path and keeps it inside the folders
// the user has open. A path outside those folders asks once for this call.
// Deny leaves it closed. The question ends when gogen cancels the tool call.
// The active editor is not passed through here.
async function allowPath(folder, raw) {
  const file = resolvePath(folder, raw);
  if (insideWorkspace(folder, file)) {
    return file;
  }
  if (pathGate.signal && pathGate.signal.aborted) {
    throw new Error("the path approval ended");
  }
  const allow = await pathGate.ask(file, pathGate.signal);
  if (!allow) {
    throw new Error("path is outside the workspace");
  }
  return file;
}

function insideWorkspace(folder, file) {
  const target = realPath(file);
  return workspaceRoots(folder).some((root) => {
    const base = realPath(root);
    return target === base || target.startsWith(base + path.sep);
  });
}

function workspaceRoots(folder) {
  const roots = (vscode.workspace.workspaceFolders || []).map((item) => item.uri.fsPath);
  if (folder) {
    roots.push(folder);
  }
  return roots;
}

function realPath(file) {
  const resolved = path.resolve(file);
  try {
    return pathKey(fs.realpathSync(resolved));
  } catch {
    // A missing file still has to be judged by the parent that does exist,
    // so a symlink out of the workspace is not accepted.
    try {
      return pathKey(path.join(fs.realpathSync(path.dirname(resolved)), path.basename(resolved)));
    } catch {
      return pathKey(resolved);
    }
  }
}

function pathKey(file) {
  const normal = path.normalize(file);
  return process.platform === "win32" ? normal.toLowerCase() : normal;
}

function resolvePath(folder, raw) {
  const text = String(raw || "").trim();
  if (!text) {
    throw new Error("path is required");
  }
  if (path.isAbsolute(text)) {
    return path.normalize(text);
  }
  if (!folder) {
    throw new Error("open a folder before using a relative path");
  }
  return path.resolve(folder, text);
}

async function openFile(folder, args) {
  const { file, uri, stat } = await existingPath(folder, args.path, "file not found");
  if (stat.type & vscode.FileType.Directory) {
    await vscode.commands.executeCommand("revealInExplorer", uri);
    return "that path is a directory; revealed it in the explorer";
  }
  const doc = await vscode.workspace.openTextDocument(uri);
  const opts = { preview: false };
  if (args.line) {
    const line = clamp(Number(args.line) - 1, 0, Math.max(0, doc.lineCount - 1));
    const column = clamp((Number(args.column) || 1) - 1, 0, 100000);
    opts.selection = new vscode.Range(line, column, line, column);
  }
  await vscode.window.showTextDocument(doc, opts);
  return "opened " + file;
}

async function revealFile(folder, args) {
  const { file, uri } = await existingPath(folder, args.path, "path not found");
  await vscode.commands.executeCommand("revealInExplorer", uri);
  return "revealed " + file;
}

// existingPath resolves a model-supplied path, applies the workspace check,
// and returns the file only when it exists.
async function existingPath(folder, raw, missing) {
  const file = await allowPath(folder, raw);
  const uri = vscode.Uri.file(file);
  const stat = await vscode.workspace.fs.stat(uri).catch(() => null);
  if (!stat) {
    throw new Error(missing + ": " + file);
  }
  return { file, uri, stat };
}

async function editorState(folder, args) {
  const lines = [];
  const editor = vscode.window.activeTextEditor;
  if (editor) {
    const pos = editor.selection.active;
    lines.push("active: " + editor.document.uri.fsPath);
    lines.push("cursor: L" + (pos.line + 1) + " C" + (pos.character + 1));
    const selected = editor.document.getText(editor.selection);
    if (selected) {
      lines.push("selection: " + clip(selected, 400));
    }
  } else {
    lines.push("active: none");
  }
  const target = args.path
    ? vscode.Uri.file(await allowPath(folder, args.path))
    : editor && editor.document.uri;
  if (target) {
    const diags = vscode.languages.getDiagnostics(target).slice(0, 20);
    if (diags.length === 0) {
      lines.push("diagnostics: none");
    } else {
      lines.push("diagnostics:");
      for (const d of diags) {
        lines.push("- " + severity(d.severity) + " L" + (d.range.start.line + 1) + ": " + clip(d.message, 200));
      }
    }
  }
  return lines.join("\n");
}

function clamp(n, min, max) {
  if (!Number.isFinite(n)) {
    return min;
  }
  return Math.min(max, Math.max(min, Math.trunc(n)));
}

function clip(text, max) {
  const flat = String(text).replace(/\s+/g, " ").trim();
  return flat.length > max ? flat.slice(0, max) + "…" : flat;
}

function severity(value) {
  switch (value) {
    case vscode.DiagnosticSeverity.Error:
      return "error";
    case vscode.DiagnosticSeverity.Warning:
      return "warning";
    case vscode.DiagnosticSeverity.Information:
      return "info";
    default:
      return "hint";
  }
}

module.exports = { EditorBridge };
