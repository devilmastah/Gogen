"use strict";

const crypto = require("crypto");
const http = require("http");
const path = require("path");
const vscode = require("vscode");

// EditorBridge is the loopback listener the stdio MCP server calls. The
// token stays in this process and in that server's environment. It is not
// written to a config file and it is not logged.
class EditorBridge {
  constructor(log, folderPath) {
    this.log = log;
    this.folderPath = folderPath;
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
      server.once("error", reject);
      server.listen(0, "127.0.0.1", () => {
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
    const chunks = [];
    let size = 0;
    req.on("data", (chunk) => {
      size += chunk.length;
      if (size > 65536) {
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on("end", () => {
      let body;
      try {
        body = JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}");
      } catch {
        send(res, 400, { ok: false, text: "bad request" });
        return;
      }
      this.dispatch(body)
        .then((text) => send(res, 200, { ok: true, text }))
        .catch((err) => send(res, 200, { ok: false, text: err && err.message ? err.message : String(err) }));
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
  const file = resolvePath(folder, args.path);
  const uri = vscode.Uri.file(file);
  const stat = await vscode.workspace.fs.stat(uri).catch(() => null);
  if (!stat) {
    throw new Error("file not found: " + file);
  }
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
  const file = resolvePath(folder, args.path);
  const uri = vscode.Uri.file(file);
  const stat = await vscode.workspace.fs.stat(uri).catch(() => null);
  if (!stat) {
    throw new Error("path not found: " + file);
  }
  await vscode.commands.executeCommand("revealInExplorer", uri);
  return "revealed " + file;
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
    ? vscode.Uri.file(resolvePath(folder, args.path))
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
