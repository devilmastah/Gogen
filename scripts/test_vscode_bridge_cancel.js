"use strict";

// gogen's tool-call timeout is what ends a path approval. This spawns the
// bridge, holds its HTTP call, then sends notifications/cancelled. The held
// request has to close, and the bridge has to report the cancel. A local
// timer in the bridge would leave the request open.

const http = require("http");
const path = require("path");
const { spawn } = require("child_process");

function fail(msg) {
  console.error(msg);
  process.exit(1);
}

const token = "test-token";
let held = null;
let gotRequest = null;
const requestArrived = new Promise((resolve) => {
  gotRequest = resolve;
});

const server = http.createServer((req, res) => {
  held = req;
  gotRequest();
  res.on("error", () => {});
});

server.listen(0, "127.0.0.1", () => {
  const port = server.address().port;
  const child = spawn(process.execPath, [path.join(__dirname, "..", "editors", "vscode", "bridge.js")], {
    env: Object.assign({}, process.env, {
      GOGEN_VSCODE_BRIDGE: "127.0.0.1:" + port,
      GOGEN_VSCODE_BRIDGE_TOKEN: token,
    }),
    stdio: ["pipe", "pipe", "pipe"],
  });
  let out = "";
  let errText = "";
  let done = false;
  child.stdout.on("data", (chunk) => {
    out += chunk;
  });
  child.stderr.on("data", (chunk) => {
    errText += chunk;
  });
  child.on("exit", (code) => {
    if (!done && code) {
      fail("bridge exited " + code + "\n" + errText);
    }
  });

  const deadline = setTimeout(() => {
    child.kill();
    server.close();
    fail("timed out waiting for the cancel to close the call\nstdout:\n" + out + "\nstderr:\n" + errText);
  }, 3000);

  child.stdin.write(JSON.stringify({
    jsonrpc: "2.0",
    id: 7,
    method: "tools/call",
    params: { name: "open_file", arguments: { path: "outside.txt" } },
  }) + "\n");

  requestArrived.then(() => new Promise((resolve) => {
    held.on("close", resolve);
    child.stdin.write(JSON.stringify({
      jsonrpc: "2.0",
      method: "notifications/cancelled",
      params: { requestId: 7, reason: "cancelled" },
    }) + "\n");
  })).then(() => new Promise((resolve) => {
    const waitOut = () => {
      if (out.includes("the call was cancelled")) {
        resolve();
        return;
      }
      setTimeout(waitOut, 20);
    };
    waitOut();
  })).then(() => {
    done = true;
    clearTimeout(deadline);
    child.kill();
    server.close();
    console.log("bridge cancel closed the held call");
  }).catch((err) => {
    done = true;
    clearTimeout(deadline);
    child.kill();
    server.close();
    fail(err && err.stack ? err.stack : String(err));
  });
});
