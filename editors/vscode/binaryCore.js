"use strict";

// Pure helpers for choosing a published gogen build. The VS Code prompts
// live in binary.js so this file can be checked without the editor.

const RELEASE_REPO = "gompa/Gogen";

// maxDownloadBytes is the largest body the sidebar will hold for a release
// asset. A current Windows binary is under 100MB. The cap leaves room for a
// larger build and stops a response that never ends.
const maxDownloadBytes = 256 * 1024 * 1024;

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

// executableName is the program named in user settings. A workspace or
// folder value is ignored, so a repository cannot choose the binary.
// An empty user value uses the name on PATH.
function executableName(inspected) {
  const value = inspected && inspected.globalValue;
  const text = typeof value === "string" ? value.trim() : "";
  return text || "gogen";
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

// declaredTooLarge reports whether a Content-Length header is over the cap.
// A missing or non-numeric header is not large; the bytes still have a cap.
function declaredTooLarge(header, limit) {
  if (Array.isArray(header)) {
    header = header[0];
  }
  if (header == null || header === "") {
    return false;
  }
  const n = Number(header);
  return Number.isFinite(n) && n > limit;
}

// appendCapped keeps a download chunk when the total stays within the cap.
// The chunk is not stored once the total would pass it.
function appendCapped(chunks, size, chunk, limit) {
  const next = size + chunk.length;
  if (next > limit) {
    throw new Error("download is too large");
  }
  chunks.push(chunk);
  return next;
}

module.exports = {
  RELEASE_REPO,
  maxDownloadBytes,
  assetFor,
  checksumMatches,
  executableName,
  declaredTooLarge,
  appendCapped,
};
