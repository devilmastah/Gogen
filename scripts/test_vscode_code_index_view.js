"use strict";

// The settings page paints codeIndexLine from the config frame. Progress
// updates a newer snapshot that the cached config does not have. Opening
// settings has to send that newer line.
// Run: node scripts/test_vscode_code_index_view.js

const path = require("node:path");
const { withCodeIndexLine, acceptCodeIndexSeq } = require(path.join(__dirname, "..", "editors", "vscode", "codeIndexView"));

let failed = 0;

function check(name, got, want) {
  if (got !== want) {
    console.error(name + ": got " + JSON.stringify(got) + ", want " + JSON.stringify(want));
    failed++;
  }
}

const cached = { type: "config", codeIndexLine: "Not indexed", codeIndexPhase: "idle" };
const live = withCodeIndexLine(cached, { phase: "embedding", line: "Embedding 2/4 files" });
check("live line", live.codeIndexLine, "Embedding 2/4 files");
check("live phase", live.codeIndexPhase, "embedding");
check("cached left alone", cached.codeIndexLine, "Not indexed");

const kept = withCodeIndexLine(cached, null);
check("no snapshot", kept.codeIndexLine, "Not indexed");

check("first line", acceptCodeIndexSeq(0, 0), true);
check("newer progress", acceptCodeIndexSeq(1, 2), true);
check("same sequence", acceptCodeIndexSeq(2, 2), false);
check("older config", acceptCodeIndexSeq(4, 3), false);
check("missing sequence after progress", acceptCodeIndexSeq(4, 0), false);

if (failed > 0) {
  console.error(failed + " check(s) failed");
  process.exit(1);
}
console.log("code index view ok");
