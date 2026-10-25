"use strict";

const crypto = require("crypto");
const fs = require("fs");
const https = require("https");
const path = require("path");
const { execFile } = require("child_process");
const vscode = require("vscode");
const { RELEASE_REPO, assetFor, checksumMatches } = require("./binaryCore");

// resolveBinary returns the gogen program to launch. An absolute
// gogen.executable that exists wins. Otherwise a name on PATH wins. When
// neither is there, the user picks an existing program or a folder to
// download the latest release into. The chosen path is saved in user settings.
async function resolveBinary(log) {
  const configured = configuredName();
  if (path.isAbsolute(configured)) {
    if (fs.existsSync(configured)) {
      return configured;
    }
  } else {
    const found = await lookPath(configured);
    if (found) {
      return found;
    }
  }
  const choice = await vscode.window.showQuickPick(
    [
      { label: "Use an existing gogen", id: "existing", description: "Pick a program already on this computer" },
      { label: "Download gogen", id: "download", description: "Latest release for this operating system" },
    ],
    { title: "GoGen needs its program", ignoreFocusOut: true, placeHolder: "gogen is not on PATH" },
  );
  if (!choice) {
    throw new Error("Choose a gogen program to start the chat.");
  }
  const file = choice.id === "download" ? await downloadBinary(log) : await pickExisting();
  await vscode.workspace.getConfiguration("gogen").update("executable", file, vscode.ConfigurationTarget.Global);
  return file;
}

function configuredName() {
  const value = vscode.workspace.getConfiguration("gogen").get("executable");
  const text = typeof value === "string" ? value.trim() : "";
  return text || "gogen";
}

function lookPath(name) {
  return new Promise((resolve) => {
    const command = process.platform === "win32" ? "where" : "which";
    execFile(command, [name], { windowsHide: true }, (err, stdout) => {
      if (err) {
        resolve("");
        return;
      }
      const line = String(stdout).split(/\r?\n/).map((item) => item.trim()).find(Boolean);
      resolve(line && fs.existsSync(line) ? line : "");
    });
  });
}

async function pickExisting() {
  const picked = await vscode.window.showOpenDialog({
    canSelectFiles: true,
    canSelectMany: false,
    title: "Choose the gogen program",
  });
  if (!picked || !picked[0]) {
    throw new Error("Choose a gogen program to start the chat.");
  }
  return picked[0].fsPath;
}

async function downloadBinary(log) {
  const asset = assetFor(process.platform, process.arch);
  if (!asset) {
    throw new Error("There is no published gogen build for this computer.");
  }
  const picked = await vscode.window.showOpenDialog({
    canSelectFolders: true,
    canSelectMany: false,
    title: "Choose a folder for gogen",
  });
  if (!picked || !picked[0]) {
    throw new Error("Choose a folder to download gogen into.");
  }
  const dest = path.join(picked[0].fsPath, asset);
  await vscode.window.withProgress(
    { location: vscode.ProgressLocation.Notification, title: "Downloading gogen" },
    async () => {
      const release = await getJson("https://api.github.com/repos/" + RELEASE_REPO + "/releases/latest");
      const assets = Array.isArray(release.assets) ? release.assets : [];
      const file = assets.find((item) => item.name === asset);
      const sums = assets.find((item) => item.name === "checksums.txt");
      if (!file || !sums || !file.browser_download_url || !sums.browser_download_url) {
        throw new Error("The latest release has no build for this computer.");
      }
      const body = await getBuffer(file.browser_download_url);
      const checksums = (await getBuffer(sums.browser_download_url)).toString("utf8");
      const hash = crypto.createHash("sha256").update(body).digest("hex");
      // checksums.txt comes from the same release. A match means the bytes
      // were not damaged. It does not prove the release itself is authentic.
      if (!checksumMatches(checksums, asset, hash)) {
        throw new Error("The downloaded gogen did not match its checksum.");
      }
      fs.writeFileSync(dest, body);
      if (process.platform !== "win32") {
        fs.chmodSync(dest, 0o755);
      }
    },
  );
  log.appendLine("downloaded gogen to " + dest);
  return dest;
}

function getJson(url) {
  return getBuffer(url, { accept: "application/vnd.github+json" }).then((body) => JSON.parse(body.toString("utf8")));
}

function getBuffer(url, headers, redirects) {
  const hops = redirects || 0;
  const extra = headers || { accept: "application/octet-stream" };
  return new Promise((resolve, reject) => {
    if (hops > 5) {
      reject(new Error("download redirected too many times"));
      return;
    }
    const req = https.get(url, { headers: Object.assign({ "user-agent": "gogen-vscode" }, extra) }, (res) => {
      if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
        res.resume();
        resolve(getBuffer(res.headers.location, extra, hops + 1));
        return;
      }
      if (res.statusCode !== 200) {
        res.resume();
        reject(new Error("download failed (" + res.statusCode + ")"));
        return;
      }
      const chunks = [];
      res.on("data", (chunk) => chunks.push(chunk));
      res.on("end", () => resolve(Buffer.concat(chunks)));
    });
    req.setTimeout(120000, () => {
      req.destroy();
      reject(new Error("download timed out"));
    });
    req.on("error", reject);
  });
}

module.exports = { resolveBinary };
