"use strict";

// Stdio MCP server spawned by gogen. It does not touch the editor. Each
// tools/call is forwarded to the extension host, which is the only process
// that can call the VS Code API.

const http = require("http");

const bridgeHost = process.env.GOGEN_VSCODE_BRIDGE || "";
const token = process.env.GOGEN_VSCODE_BRIDGE_TOKEN || "";

const tools = [
  {
    name: "open_file",
    description: "Open a file in the VS Code editor for the user. Path may be relative to the working directory or absolute. Line and column are 1-based.",
    inputSchema: {
      type: "object",
      properties: {
        path: { type: "string", description: "File to open" },
        line: { type: "integer", description: "1-based line to reveal" },
        column: { type: "integer", description: "1-based column" },
      },
      required: ["path"],
    },
  },
  {
    name: "reveal_file",
    description: "Show a file or directory in the VS Code explorer. Path may be relative to the working directory or absolute.",
    inputSchema: {
      type: "object",
      properties: {
        path: { type: "string", description: "File or directory to reveal" },
      },
      required: ["path"],
    },
  },
  {
    name: "editor_state",
    description: "Read what the user is looking at in VS Code: the active file, the cursor, the selection, and diagnostics. Pass path to read diagnostics for that file instead of the active one.",
    inputSchema: {
      type: "object",
      properties: {
        path: { type: "string", description: "File whose diagnostics to read. Omit for the active editor." },
      },
    },
  },
];

let chain = Promise.resolve();
let buffer = "";

process.stdin.setEncoding("utf8");
process.stdin.on("data", (chunk) => {
  buffer += chunk;
  let nl;
  while ((nl = buffer.indexOf("\n")) >= 0) {
    const line = buffer.slice(0, nl).trim();
    buffer = buffer.slice(nl + 1);
    if (line) {
      chain = chain.then(() => dispatch(line)).catch(() => {});
    }
  }
});

async function dispatch(line) {
  let req;
  try {
    req = JSON.parse(line);
  } catch {
    return;
  }
  if (!req || req.id === undefined || req.id === null) {
    return;
  }
  try {
    const result = await answer(req);
    write({ jsonrpc: "2.0", id: req.id, result });
  } catch (err) {
    write({
      jsonrpc: "2.0",
      id: req.id,
      error: { code: -32000, message: err && err.message ? err.message : String(err) },
    });
  }
}

async function answer(req) {
  switch (req.method) {
    case "initialize":
      return {
        protocolVersion: "2024-11-05",
        capabilities: { tools: {} },
        serverInfo: { name: "gogen-vscode", version: "0.1.0" },
      };
    case "tools/list":
      return { tools };
    case "tools/call":
      return callTool(req.params || {});
    default:
      throw new Error("unknown method " + req.method);
  }
}

function callTool(params) {
  const name = params.name || "";
  const args = params.arguments || {};
  return new Promise((resolve, reject) => {
    if (!bridgeHost || !token) {
      reject(new Error("the VS Code bridge is not connected"));
      return;
    }
    const body = Buffer.from(JSON.stringify({ name, arguments: args }));
    const req = http.request(
      {
        host: "127.0.0.1",
        port: portOf(bridgeHost),
        path: "/call",
        method: "POST",
        headers: {
          "content-type": "application/json",
          "content-length": body.length,
          authorization: "Bearer " + token,
        },
      },
      (res) => {
        const chunks = [];
        res.on("data", (chunk) => chunks.push(chunk));
        res.on("end", () => {
          let payload;
          try {
            payload = JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}");
          } catch {
            reject(new Error("the VS Code bridge returned a bad response"));
            return;
          }
          resolve({
            content: [{ type: "text", text: payload.text || "" }],
            isError: payload.ok === false,
          });
        });
      },
    );
    req.on("error", (err) => reject(err));
    req.setTimeout(10000, () => {
      req.destroy();
      reject(new Error("the VS Code bridge timed out"));
    });
    req.end(body);
  });
}

function portOf(host) {
  const i = host.lastIndexOf(":");
  return i >= 0 ? host.slice(i + 1) : host;
}

function write(msg) {
  process.stdout.write(JSON.stringify(msg) + "\n");
}
