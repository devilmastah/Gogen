"use strict";

const fs = require("fs");
const path = require("path");

// configRels is the sidebar template, then the .conf names gogen searches.
// gogen.conf beside the program is not a project search path.
const configRels = ["gogen.conf", path.join(".gogen", "gogen.conf"), "GOGEN.conf"];

// guidelineRels matches guidelineSearchPaths in internal/projectfile/discover.go.
const guidelineRels = [
  path.join(".gogen", "gogen.md"),
  "GOGEN.md",
  path.join(".gogen", "rules.md"),
  path.join(".cursor", "rules", "gogen.md"),
];

// projectConfRels matches configSearchPaths in internal/projectfile/discover.go.
const projectConfRels = [
  path.join(".gogen", "gogen.conf"),
  "GOGEN.conf",
  path.join(".gogen", "gogen.md"),
  "GOGEN.md",
];

function isNonEmptyFile(file) {
  try {
    const info = fs.statSync(file);
    return info.isFile() && info.size > 0;
  } catch {
    return false;
  }
}

function firstFile(dir, rels) {
  for (const rel of rels) {
    const file = path.join(dir, rel);
    if (isNonEmptyFile(file)) {
      return { file, rel };
    }
  }
  return null;
}

// templateSources returns the config beside the executable and the guidelines
// file gogen would read if that directory were the working directory. One
// file that is both is returned once. The bytes are copied as they are.
function templateSources(exeDir) {
  const config = firstFile(exeDir, configRels);
  const guidelines = firstFile(exeDir, guidelineRels);
  const files = [];
  if (config) {
    files.push(config);
  }
  if (guidelines && (!config || path.resolve(config.file) !== path.resolve(guidelines.file))) {
    files.push(guidelines);
  }
  return files;
}

// templateDest maps a template onto the opened folder. A .conf becomes
// .gogen/gogen.conf. A guidelines file keeps the path gogen searches.
function templateDest(folder, rel) {
  if (rel.endsWith(".conf")) {
    return path.join(folder, ".gogen", "gogen.conf");
  }
  return path.join(folder, rel);
}

// gitignoreIgnoresGogen reports whether a .gitignore already ignores the
// .gogen directory by name. The last matching line wins, so a later ! un-ignores it.
function gitignoreIgnoresGogen(text) {
  let ignored = false;
  for (const raw of String(text || "").split(/\r?\n/)) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) {
      continue;
    }
    const negated = line.startsWith("!");
    const body = (negated ? line.slice(1) : line).trim();
    if (namesGogenDir(body)) {
      ignored = !negated;
    }
  }
  return ignored;
}

function namesGogenDir(pattern) {
  let p = pattern.replace(/\\/g, "/");
  if (p.endsWith("/")) {
    p = p.slice(0, -1);
  }
  if (p.startsWith("/")) {
    p = p.slice(1);
  }
  return p === ".gogen"
    || p === ".gogen/**"
    || p === ".gogen/*"
    || p === "**/.gogen"
    || p === "**/.gogen/**"
    || p === "**/.gogen/*";
}

// gitignoreEntries is what a copy should add. .gogen/ covers a guidelines
// file inside that directory. A guidelines file outside it, such as GOGEN.md,
// is listed on its own. A path the file already ignores is left out.
function gitignoreEntries(folder, dests, text) {
  const entries = [];
  let coverGogen = gitignoreIgnoresGogen(text);
  if (dests.some((dest) => copiedIntoGogen(folder, dest)) && !coverGogen) {
    entries.push(".gogen/");
    coverGogen = true;
  }
  for (const dest of dests) {
    if (path.extname(dest).toLowerCase() !== ".md") {
      continue;
    }
    if (coverGogen && copiedIntoGogen(folder, dest)) {
      continue;
    }
    const rel = gitPath(folder, dest);
    if (!rel || gitignoreIgnoresPath(text, rel)) {
      continue;
    }
    entries.push(rel);
  }
  return entries;
}

function gitPath(folder, dest) {
  const rel = path.relative(folder, dest);
  if (!rel || rel.startsWith("..") || path.isAbsolute(rel)) {
    return "";
  }
  return rel.split(path.sep).join("/");
}

function gitignoreIgnoresPath(text, rel) {
  let ignored = false;
  for (const raw of String(text || "").split(/\r?\n/)) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) {
      continue;
    }
    const negated = line.startsWith("!");
    const body = (negated ? line.slice(1) : line).trim();
    if (namesPath(body, rel)) {
      ignored = !negated;
    }
  }
  return ignored;
}

function namesPath(pattern, rel) {
  let p = pattern.replace(/\\/g, "/");
  if (p.startsWith("/")) {
    p = p.slice(1);
  }
  return p === rel;
}

// withGitignoreEntries appends ignore lines, keeping the file's newline.
function withGitignoreEntries(text, entries) {
  const nl = String(text || "").includes("\r\n") ? "\r\n" : "\n";
  let next = String(text || "");
  if (next.length && !next.endsWith("\n")) {
    next += nl;
  }
  for (const entry of entries) {
    next += entry + nl;
  }
  return next;
}

// copiedIntoGogen reports whether dest landed inside folder/.gogen.
function copiedIntoGogen(folder, dest) {
  const rel = path.relative(folder, dest);
  return rel === ".gogen" || rel.startsWith(".gogen" + path.sep);
}

module.exports = {
  projectConfRels,
  templateSources,
  templateDest,
  gitignoreIgnoresGogen,
  gitignoreEntries,
  withGitignoreEntries,
  copiedIntoGogen,
  isNonEmptyFile,
};
