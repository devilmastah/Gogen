"use strict";

// Checks the HTTP layer of editors/vscode/editorBridge.js: the token guard,
// the request and body guards, the three editor tools, and the one question
// a path outside the workspace asks. The VS Code API is stubbed, because
// this file runs outside the editor.
// Run: node scripts/test_vscode_editor_bridge.js

const Module = require("node:module");
const fs = require("node:fs");
const http = require("node:http");
const os = require("node:os");
const path = require("node:path");

const MAX_BODY = 65536;

let failed = 0;

function check(name, got, want) {
  const left = JSON.stringify(got);
  const right = JSON.stringify(want);
  if (left !== right) {
    console.error(name + ": got " + left + ", want " + right);
    failed++;
  }
}

function ok(name, condition, detail) {
  if (!condition) {
    console.error(name + ": " + detail);
    failed++;
  }
}

const root = fs.mkdtempSync(path.join(os.tmpdir(), "gogen-editor-bridge-"));
const ws = path.join(root, "ws");
const outside = path.join(root, "outside");
fs.mkdirSync(ws);
fs.mkdirSync(outside);
fs.writeFileSync(path.join(ws, "inside.txt"), "in");
fs.writeFileSync(path.join(outside, "secret.txt"), "out");

// The editor is stubbed just far enough for the three tools to run.
const shown = [];
const vscodeStub = {
  Uri: { file: (p) => ({ fsPath: p }) },
  FileType: { File: 1, Directory: 2 },
  Range: class Range {
    constructor(startLine, startChar, endLine, endChar) {
      Object.assign(this, { startLine, startChar, endLine, endChar });
    }
  },
  DiagnosticSeverity: { Error: 0, Warning: 1, Information: 2, Hint: 3 },
  workspace: {
    workspaceFolders: [{ uri: { fsPath: ws } }],
    fs: {
      stat: async (uri) => {
        let info;
        try {
          info = fs.statSync(uri.fsPath);
        } catch {
          throw new Error("ENOENT");
        }
        return { type: info.isDirectory() ? 2 : 1 };
      },
    },
    openTextDocument: async (uri) => ({ lineCount: 5, uri }),
  },
  window: {
    activeTextEditor: undefined,
    showTextDocument: async (doc) => {
      shown.push(doc.uri.fsPath);
    },
  },
  commands: { executeCommand: async () => {} },
  languages: { getDiagnostics: () => [] },
};

const load = Module._load;
Module._load = function (request, ...rest) {
  if (request === "vscode") {
    return vscodeStub;
  }
  return load.call(this, request, ...rest);
};
const { EditorBridge } = require(path.join(__dirname, "..", "editors", "vscode", "editorBridge"));

function request(port, opts, body) {
  return new Promise((resolve) => {
    const payload = body === undefined ? "" : body;
    const headers = Object.assign({}, opts.headers);
    if (payload && headers["content-length"] === undefined) {
      headers["content-length"] = Buffer.byteLength(payload);
    }
    // agent: false keeps every check on its own socket. A refused request
    // leaves its body unread, and the keep-alive global agent would reuse
    // that socket for the next check.
    const req = http.request(
      { host: "127.0.0.1", port, path: opts.path || "/call", method: opts.method || "POST", headers, agent: false },
      (res) => {
        const chunks = [];
        res.on("data", (chunk) => chunks.push(chunk));
        res.on("end", () => {
          const raw = Buffer.concat(chunks).toString("utf8");
          let json = null;
          try {
            json = JSON.parse(raw);
          } catch {
            json = null;
          }
          resolve({ status: res.statusCode, json, raw });
        });
      },
    );
    req.on("error", (err) => resolve({ error: err }));
    req.end(payload);
  });
}

async function main() {
  const logged = [];
  const log = { appendLine: (line) => logged.push(line) };
  const asked = [];
  let allowNext = false;
  const bridge = new EditorBridge(log, () => ws, async (file) => {
    asked.push(file);
    return allowNext;
  });
  const port = await bridge.start();
  const auth = { authorization: "Bearer " + bridge.token };
  const call = (body, headers) =>
    request(port, { headers: headers === undefined ? auth : headers }, typeof body === "string" ? body : JSON.stringify(body));

  try {
    // --- the request guard ---------------------------------------------
    check("GET is refused", (await request(port, { method: "GET" })).status, 404);
    check("another path is refused", (await request(port, { path: "/other", headers: auth }, "{}")).status, 404);
    check("no token", (await call({ name: "editor_state" }, {})).status, 401);
    check(
      "wrong token of the same length",
      (await call({ name: "editor_state" }, { authorization: "Bearer " + "0".repeat(bridge.token.length) })).status,
      401,
    );
    check("bad json", (await call("{")).status, 400);

    // --- the body guard -------------------------------------------------
    check(
      "declared body over 64KB",
      (await request(port, { headers: Object.assign({}, auth, { "content-length": MAX_BODY + 1 }) }, "{}")).status,
      413,
    );
    const streamed = await new Promise((resolve) => {
      const req = http.request({ host: "127.0.0.1", port, path: "/call", method: "POST", headers: auth, agent: false }, (res) => {
        res.resume();
        resolve({ status: res.statusCode });
      });
      req.on("error", (err) => resolve({ error: err }));
      req.write(Buffer.alloc(MAX_BODY + 1024, 0x61));
      req.end();
    });
    ok(
      "streamed body over 64KB is refused",
      streamed.status === 413 || streamed.error !== undefined,
      "got " + JSON.stringify(streamed),
    );

    // --- the tools ------------------------------------------------------
    const unknown = await call({ name: "nope", arguments: {} });
    check("unknown tool is a failed call, not a 500", [unknown.status, unknown.json.ok], [200, false]);
    check("unknown tool text", unknown.json.text, "unknown tool nope");

    const openInside = await call({ name: "open_file", arguments: { path: "inside.txt", line: 2, column: 3 } });
    check("open inside", [openInside.status, openInside.json.ok, openInside.json.text], [200, true, "opened " + path.join(ws, "inside.txt")]);
    check("open inside asks nothing", asked, []);
    check("open inside reached the editor", shown, [path.join(ws, "inside.txt")]);

    const missing = await call({ name: "open_file", arguments: { path: "gone.txt" } });
    check("missing file", [missing.json.ok, missing.json.text], [false, "file not found: " + path.join(ws, "gone.txt")]);

    const reveal = await call({ name: "reveal_file", arguments: { path: "inside.txt" } });
    check("reveal inside", [reveal.json.ok, reveal.json.text], [true, "revealed " + path.join(ws, "inside.txt")]);

    const state = await call({ name: "editor_state", arguments: {} });
    check("editor state with no editor", [state.json.ok, state.json.text], [true, "active: none"]);

    // --- the path question ----------------------------------------------
    allowNext = true;
    const allowed = await call({ name: "open_file", arguments: { path: path.join(outside, "secret.txt") } });
    check("outside path, allowed", [allowed.json.ok, allowed.json.text], [true, "opened " + path.join(outside, "secret.txt")]);
    check("outside path asks once", asked, [path.join(outside, "secret.txt")]);

    asked.length = 0;
    allowNext = false;
    const shownBefore = shown.length;
    const denied = await call({ name: "open_file", arguments: { path: path.join(outside, "secret.txt") } });
    check("outside path, denied", [denied.status, denied.json.ok, denied.json.text], [200, false, "path is outside the workspace"]);
    check("denied path still asked", asked, [path.join(outside, "secret.txt")]);
    check("denied path was not opened", shown.length, shownBefore);

    // --- the child environment ------------------------------------------
    const env = bridge.mcpEnv("/ext").GOGEN_MCP_EXTRA;
    const extra = JSON.parse(env);
    check("one host server", extra.length, 1);
    check("host server name", extra[0].name, "vscode");
    check("host server command is this node", extra[0].command, process.execPath);
    check("host server runs as node", extra[0].env.ELECTRON_RUN_AS_NODE, "1");
    check("host server points at the bridge", extra[0].env.GOGEN_VSCODE_BRIDGE, "127.0.0.1:" + port);
    check("host server carries the token", extra[0].env.GOGEN_VSCODE_BRIDGE_TOKEN, bridge.token);
    ok("token never logged", logged.every((line) => !line.includes(bridge.token)), JSON.stringify(logged));

    // --- stop -----------------------------------------------------------
    bridge.stop();
    const afterStop = await request(port, { headers: auth }, "{}");
    ok("stop closes the listener", afterStop.error !== undefined, "got " + JSON.stringify(afterStop));
  } finally {
    bridge.stop();
    fs.rmSync(root, { recursive: true, force: true });
  }
}

main().then(
  () => {
    if (failed > 0) {
      console.error(failed + " check(s) failed");
      process.exit(1);
    }
    console.log("editor bridge ok");
  },
  (err) => {
    console.error(err && err.stack ? err.stack : String(err));
    fs.rmSync(root, { recursive: true, force: true });
    process.exit(1);
  },
);
