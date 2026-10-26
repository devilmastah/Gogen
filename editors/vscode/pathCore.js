"use strict";

// Pure path checks for a model-supplied file. The question that asks the
// user lives in editorBridge.js so this file can be checked without the editor.

const fs = require("fs");
const path = require("path");

// resolvePath turns a model-supplied path into an absolute path. A relative
// path is resolved from the working folder. ".." is collapsed here, before
// the workspace check.
function resolvePath(folder, raw) {
  const text = String(raw || "").trim();
  if (!text) {
    throw new Error("path is required");
  }
  if (path.isAbsolute(text)) {
    return path.normalize(text);
  }
  if (!folder) {
    throw new Error("open a folder before using a relative path");
  }
  return path.resolve(folder, text);
}

// realPath resolves symlinks. A missing file is judged by the parent that
// does exist, so a symlink out of the workspace is not accepted. When no
// parent exists, the lexical path is returned.
function realPath(file) {
  const resolved = path.resolve(file);
  try {
    return pathKey(fs.realpathSync(resolved));
  } catch {
    try {
      return pathKey(path.join(fs.realpathSync(path.dirname(resolved)), path.basename(resolved)));
    } catch {
      return pathKey(resolved);
    }
  }
}

// insideRoots reports whether file is one of the roots or a file inside it.
// Both sides are real paths, so a symlink is judged by where it lands.
function insideRoots(roots, file) {
  const target = realPath(file);
  return roots.some((root) => {
    const base = realPath(root);
    return target === base || target.startsWith(base + path.sep);
  });
}

function pathKey(file) {
  const normal = path.normalize(file);
  return process.platform === "win32" ? normal.toLowerCase() : normal;
}

module.exports = { resolvePath, realPath, insideRoots };
