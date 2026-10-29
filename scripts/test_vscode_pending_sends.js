"use strict";

// Checks the sidebar's pending-send bookkeeping (pendingSendsTracker in
// editors/vscode/chatView.js). The tracker runs inside the chat webview, so it
// stays in that document: this test reads the block between the
// pending-sends markers and runs it in a fresh context.
// Run: node scripts/test_vscode_pending_sends.js

const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const source = fs.readFileSync(path.join(__dirname, "..", "editors", "vscode", "chatView.js"), "utf8");
const begin = source.indexOf("// pending-sends:begin");
const end = source.indexOf("// pending-sends:end");
if (begin < 0 || end < begin) {
  console.error("pending-sends markers not found in chatView.js");
  process.exit(1);
}
const block = source.slice(begin, end);
// The block sits in the webview's template literal. Without escapes or
// substitutions, the webview runs exactly the text this test runs.
if (block.includes("\\") || block.includes("${") || block.includes("`")) {
  console.error("pending-sends block must not use backslashes, backticks or ${ (it sits in a template literal)");
  process.exit(1);
}
const newTracker = vm.runInNewContext(block + "\npendingSendsTracker;");

let failed = 0;
function check(name, got, want) {
  const g = JSON.stringify(got);
  const w = JSON.stringify(want);
  if (g !== w) {
    console.error(name + ": got " + g + ", want " + w);
    failed++;
  }
}
const ids = (t) => t.list().map((it) => it.id);

const cases = [
  {
    name: "queued send settles when the queue lists it",
    run(t) {
      t.add("a", "hello");
      t.queue([{ id: "a", text: "hello" }], []);
      return { pending: ids(t) };
    },
    want: { pending: [] },
  },
  {
    name: "queue-full refusal during a turn returns the send as failed",
    run(t) {
      t.add("a", "first");
      t.add("b", "second");
      const sent = t.replied("b", "Error: too many queued messages (limit 20)", true);
      return { sent, pending: ids(t) };
    },
    want: { sent: { id: "b", text: "second", failed: true }, pending: ["a"] },
  },
  {
    name: "busy rejection of a command during a turn returns that send",
    run(t) {
      t.add("a", "/models x");
      return { sent: t.replied("a", "Error: agent is busy with another client", true) };
    },
    want: { sent: { id: "a", text: "/models x", failed: true } },
  },
  {
    name: "a turn's own error does not take a queued send",
    run(t) {
      t.add("a", "next");
      return { sent: t.replied("", "Error: model failed", true), pending: ids(t) };
    },
    want: { sent: null, pending: ["a"] },
  },
  {
    name: "a tagged reply for a send already answered changes nothing",
    run(t) {
      t.add("a", "keep");
      return { sent: t.replied("gone", "Error: too many queued messages", true), pending: ids(t) };
    },
    want: { sent: null, pending: ["a"] },
  },
  {
    name: "an idle command reply answers the oldest send and it ran",
    run(t) {
      t.add("a", "/help");
      t.add("b", "later");
      return { sent: t.replied("", "Commands: ...", false), pending: ids(t) };
    },
    want: { sent: { id: "a", text: "/help", failed: false }, pending: ["b"] },
  },
  {
    name: "a session closed before the turn ran returns the send as failed",
    run(t) {
      t.add("a", "do it");
      return { sent: t.replied("", "Error: session was closed before the message ran — nothing was sent", false) };
    },
    want: { sent: { id: "a", text: "do it", failed: true } },
  },
  {
    name: "user_acked of a direct send returns it",
    run(t) {
      t.add("a", "go");
      return { sent: t.acked(), pending: ids(t) };
    },
    want: { sent: { id: "a", text: "go" }, pending: [] },
  },
  {
    name: "the ack after a queue start belongs to that start",
    run(t) {
      t.add("x", "typed while idle");
      t.queue([], ["q1"]);
      const first = t.acked();
      const second = t.acked();
      return { first, second };
    },
    want: { first: null, second: { id: "x", text: "typed while idle" } },
  },
  {
    name: "an untagged reply waits while a queue start's ack is owed",
    run(t) {
      t.add("a", "x");
      t.queue([], ["q1"]);
      return { sent: t.replied("", "Commands", false), pending: ids(t) };
    },
    want: { sent: null, pending: ["a"] },
  },
  {
    name: "turn end forgets owed acks",
    run(t) {
      t.add("a", "x");
      t.queue([], ["q1"]);
      t.turnEnded();
      return { sent: t.acked() };
    },
    want: { sent: { id: "a", text: "x" } },
  },
  {
    name: "reset forgets sends and owed acks",
    run(t) {
      t.add("a", "x");
      t.queue([], ["q1"]);
      t.reset();
      t.add("b", "y");
      return { pending: ids(t), sent: t.acked() };
    },
    want: { pending: ["b"], sent: { id: "b", text: "y" } },
  },
  {
    name: "drop forgets one send",
    run(t) {
      t.add("a", "x");
      t.add("b", "y");
      t.drop("a");
      return { pending: ids(t) };
    },
    want: { pending: ["b"] },
  },
];

for (const c of cases) {
  check(c.name, c.run(newTracker()), c.want);
}

if (failed > 0) {
  console.error(failed + " check(s) failed");
  process.exit(1);
}
console.log("pending sends ok");
