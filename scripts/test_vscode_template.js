"use strict";

// Checks the config-copy helpers in editors/vscode/templateCopy.js.
// The VS Code questions stay in host.js and are not loaded here.
// Run: node scripts/test_vscode_template.js

const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const {
  templateSources,
  templateDest,
  gitignoreIgnoresGogen,
  gitignoreEntries,
  withGitignoreEntries,
  copiedIntoGogen,
} = require(path.join(__dirname, "..", "editors", "vscode", "templateCopy"));

let failed = 0;

function check(name, got, want) {
  const left = JSON.stringify(got);
  const right = JSON.stringify(want);
  if (left !== right) {
    console.error(name + ": got " + left + ", want " + right);
    failed++;
  }
}

function write(dir, rel, body) {
  const file = path.join(dir, rel);
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, body);
}

const root = fs.mkdtempSync(path.join(os.tmpdir(), "gogen-template-"));
try {
  const exe = path.join(root, "exe");
  const project = path.join(root, "project");
  fs.mkdirSync(exe);
  write(exe, "gogen.conf", "model: a\n");
  write(exe, path.join(".gogen", "gogen.conf"), "model: b\n");
  write(exe, path.join(".gogen", "gogen.md"), "rules\n");
  write(exe, "GOGEN.md", "other\n");
  const both = templateSources(exe).map((found) => found.rel);
  check("conf and guidelines", both, ["gogen.conf", path.join(".gogen", "gogen.md")]);

  const mdOnly = path.join(root, "md-only");
  write(mdOnly, "GOGEN.md", "rules\n");
  write(mdOnly, path.join(".cursor", "rules", "gogen.md"), "later\n");
  check("guidelines only", templateSources(mdOnly).map((found) => found.rel), ["GOGEN.md"]);

  check("conf destination", templateDest(project, "gogen.conf"), path.join(project, ".gogen", "gogen.conf"));
  check("guidelines destination", templateDest(project, "GOGEN.md"), path.join(project, "GOGEN.md"));
  check("inside .gogen", copiedIntoGogen(project, path.join(project, ".gogen", "gogen.conf")), true);
  check("root guidelines", copiedIntoGogen(project, path.join(project, "GOGEN.md")), false);

  check("named dir", gitignoreIgnoresGogen(".gogen\n"), true);
  check("named dir slash", gitignoreIgnoresGogen("/.gogen/\n"), true);
  check("contents", gitignoreIgnoresGogen(".gogen/*\n"), true);
  check("other entry", gitignoreIgnoresGogen("node_modules\n"), false);
  check("comment", gitignoreIgnoresGogen("# .gogen\n"), false);
  check("later un-ignore", gitignoreIgnoresGogen(".gogen/\n!.gogen/\n"), false);
  check("later ignore", gitignoreIgnoresGogen("!.gogen\n.gogen/\n"), true);
  check("append", withGitignoreEntries("node_modules\n", [".gogen/"]), "node_modules\n.gogen/\n");
  check("append bare", withGitignoreEntries("node_modules", [".gogen/"]), "node_modules\n.gogen/\n");
  check("append crlf", withGitignoreEntries("node_modules\r\n", [".gogen/"]), "node_modules\r\n.gogen/\r\n");

  const conf = path.join(project, ".gogen", "gogen.conf");
  const rootMd = path.join(project, "GOGEN.md");
  const innerMd = path.join(project, ".gogen", "gogen.md");
  const cursorMd = path.join(project, ".cursor", "rules", "gogen.md");
  check("conf and root guidelines", gitignoreEntries(project, [conf, rootMd], "node_modules\n"), [".gogen/", "GOGEN.md"]);
  check("inner guidelines covered", gitignoreEntries(project, [conf, innerMd], ""), [".gogen/"]);
  check("root guidelines only", gitignoreEntries(project, [rootMd], ".gogen/\n"), ["GOGEN.md"]);
  check("cursor guidelines", gitignoreEntries(project, [cursorMd], ""), [".cursor/rules/gogen.md"]);
  check("already ignored guidelines", gitignoreEntries(project, [rootMd], "GOGEN.md\n"), []);
  check("append both", withGitignoreEntries("node_modules\n", [".gogen/", "GOGEN.md"]), "node_modules\n.gogen/\nGOGEN.md\n");
} finally {
  fs.rmSync(root, { recursive: true, force: true });
}

if (failed > 0) {
  console.error(failed + " failed");
  process.exit(1);
}
console.log("template copy helpers ok");
