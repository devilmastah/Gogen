"use strict";

// Checks the webview CSP nonce in editors/vscode/webviewNonce.js.
// Run: node scripts/test_vscode_nonce.js

const path = require("node:path");
const { webviewNonce } = require(path.join(__dirname, "..", "editors", "vscode", "webviewNonce"));

let failed = 0;

function check(name, got, want) {
  if (got !== want) {
    console.error(name + ": got " + JSON.stringify(got) + ", want " + JSON.stringify(want));
    failed++;
  }
}

const a = webviewNonce();
const b = webviewNonce();
check("nonce length", a.length, 32);
check("nonce hex", /^[0-9a-f]{32}$/.test(a), true);
check("nonce differs", a !== b, true);

if (failed > 0) {
  console.error(failed + " check(s) failed");
  process.exit(1);
}
console.log("webview nonce ok");
