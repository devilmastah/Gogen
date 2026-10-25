"use strict";

// Pure helpers for choosing a published gogen build. The VS Code prompts
// live in binary.js so this file can be checked without the editor.

const RELEASE_REPO = "gompa/Gogen";

function assetFor(platform, arch) {
  const cpu = arch === "arm64" ? "arm64" : arch === "x64" ? "amd64" : "";
  if (!cpu) {
    return "";
  }
  if (platform === "win32") {
    return cpu === "amd64" ? "gogen-windows-amd64.exe" : "";
  }
  if (platform === "linux") {
    return "gogen-linux-" + cpu;
  }
  if (platform === "darwin") {
    return "gogen-darwin-" + cpu;
  }
  return "";
}

function checksumMatches(text, name, hash) {
  const want = String(hash || "").toLowerCase();
  if (!/^[0-9a-f]{64}$/.test(want)) {
    return false;
  }
  for (const line of String(text || "").split(/\r?\n/)) {
    const match = line.match(/^([0-9a-fA-F]{64})\s+\*?(\S+)$/);
    if (match && match[2] === name && match[1].toLowerCase() === want) {
      return true;
    }
  }
  return false;
}

module.exports = { RELEASE_REPO, assetFor, checksumMatches };
