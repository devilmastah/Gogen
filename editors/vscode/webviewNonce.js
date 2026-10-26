"use strict";

// The per-document CSP nonce for a sidebar webview. Sixteen random bytes,
// hex encoded, so the value is safe in the HTML attribute. Math.random is
// not used.

const crypto = require("crypto");

function webviewNonce() {
  return crypto.randomBytes(16).toString("hex");
}

module.exports = { webviewNonce };
