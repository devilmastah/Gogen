"use strict";

// Checks the path boundary in editors/vscode/pathCore.js.
// The VS Code question stays in editorBridge.js and is not loaded here.
// Run: node scripts/test_vscode_path.js

const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { resolvePath, realPath, insideRoots } = require(path.join(__dirname, "..", "editors", "vscode", "pathCore"));

let failed = 0;

function check(name, got, want) {
  if (got !== want) {
    console.error(name + ": got " + JSON.stringify(got) + ", want " + JSON.stringify(want));
    failed++;
  }
}

function throws(name, fn, fragment) {
  try {
    fn();
    console.error(name + ": expected a throw");
    failed++;
  } catch (err) {
    const text = err && err.message ? err.message : String(err);
    if (!text.includes(fragment)) {
      console.error(name + ": got " + JSON.stringify(text) + ", want " + JSON.stringify(fragment));
      failed++;
    }
  }
}

const root = fs.mkdtempSync(path.join(os.tmpdir(), "gogen-path-"));
try {
  const ws = path.join(root, "ws");
  const outside = path.join(root, "outside");
  const sibling = path.join(root, "ws-other");
  fs.mkdirSync(ws);
  fs.mkdirSync(outside);
  fs.mkdirSync(sibling);
  fs.writeFileSync(path.join(ws, "inside.txt"), "in");
  fs.writeFileSync(path.join(outside, "secret.txt"), "out");
  fs.writeFileSync(path.join(sibling, "file.txt"), "sib");

  const inside = resolvePath(ws, "inside.txt");
  check("relative file stays inside", insideRoots([ws], inside), true);
  check("dotdot leaves the folder", insideRoots([ws], resolvePath(ws, "../outside/secret.txt")), false);
  check("nested dotdot leaves the folder", insideRoots([ws], resolvePath(ws, "sub/../../outside/secret.txt")), false);
  check("absolute outside", insideRoots([ws], path.join(outside, "secret.txt")), false);
  check("sibling prefix is outside", insideRoots([ws], path.join(sibling, "file.txt")), false);
  check("missing file in the folder stays inside", insideRoots([ws], path.join(ws, "new.txt")), true);
  check("missing parent stays on the lexical path", insideRoots([ws], path.join(ws, "gone", "file.txt")), true);

  const link = path.join(ws, "link");
  const kind = process.platform === "win32" ? "junction" : "dir";
  fs.symlinkSync(outside, link, kind);
  check("symlink to outside", insideRoots([ws], path.join(link, "secret.txt")), false);
  const missingThroughLink = path.join(link, "new.txt");
  check("missing file through a symlink is outside", insideRoots([ws], missingThroughLink), false);
  const landed = realPath(missingThroughLink);
  const outsideFile = realPath(path.join(outside, "new.txt"));
  check("missing file is judged by the real parent", landed, outsideFile);

  throws("empty path", () => resolvePath(ws, "  "), "path is required");
  throws("relative without a folder", () => resolvePath("", "inside.txt"), "open a folder");
} finally {
  fs.rmSync(root, { recursive: true, force: true });
}

if (failed > 0) {
  console.error(failed + " check(s) failed");
  process.exit(1);
}
console.log("path boundary ok");
