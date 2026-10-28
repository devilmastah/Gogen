"use strict";

// Checks the sidebar queue-id rule in editors/vscode/queueId.js against the
// server's validQueueID cases.
// Run: node scripts/test_vscode_queue_id.js

const path = require("node:path");
const { queueIdOk } = require(path.join(__dirname, "..", "editors", "vscode", "queueId"));

let failed = 0;

const cases = [
  ["webview id", "q-0123456789abcdef", true],
  ["plain token", "q-keep", true],
  ["dot and underscore", "q-ok_2.x", true],
  ["64 characters", "x".repeat(64), true],
  ["empty", "", false],
  ["65 characters", "x".repeat(65), false],
  ["space and bang", "bad id!", false],
  ["quote", 'q"1', false],
  ["not a string", 7, false],
  ["undefined", undefined, false],
];

for (const [name, id, want] of cases) {
  const got = queueIdOk(id);
  if (got !== want) {
    console.error(name + ": got " + got + ", want " + want);
    failed++;
  }
}

if (failed > 0) {
  console.error(failed + " check(s) failed");
  process.exit(1);
}
console.log("queue id ok");
