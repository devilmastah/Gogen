const { spawn } = require("child_process");
const http = require("http");
const net = require("net");

// Sidecar owns one gogen --web process for the workspace folder and the
// chat socket. The socket stays in the extension host: a webview Origin
// is not a loopback host, and the server rejects it.
class Sidecar {
  constructor(folder, executable, log, env) {
    this.folder = folder;
    this.executable = executable;
    this.log = log;
    this.env = env || null;
    this.proc = null;
    this.ws = null;
    this.port = 0;
    this.sessionId = "";
    this.listeners = new Set();
  }

  onEvent(fn) {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  emit(event) {
    for (const fn of this.listeners) {
      fn(event);
    }
  }

  async start() {
    this.stop();
    this.sessionId = "";
    this.port = await freePort();
    const addr = `127.0.0.1:${this.port}`;
    this.log.appendLine(`starting ${this.executable} --web --host ${addr}`);
    this.log.appendLine(`cwd ${this.folder}`);
    const proc = spawn(this.executable, ["--web", "--host", addr], {
      cwd: this.folder,
      env: this.env ? Object.assign({}, process.env, this.env) : process.env,
      windowsHide: true,
      stdio: ["ignore", "pipe", "pipe"],
    });
    this.proc = proc;
    proc.stdout.on("data", (buf) => this.log.append(buf.toString()));
    proc.stderr.on("data", (buf) => this.log.append(buf.toString()));
    let failed = null;
    proc.once("error", (err) => {
      failed = err.code === "ENOENT"
        ? new Error(`cannot find ${this.executable}. Set gogen.executable to the binary path.`)
        : err;
    });
    proc.on("exit", (code, signal) => {
      if (this.proc !== proc) {
        return;
      }
      this.proc = null;
      this.closeSocket();
      const why = signal ? `signal ${signal}` : `exit ${code}`;
      this.emit({ type: "status", state: "closed", text: `gogen stopped (${why})` });
    });

    await waitForHTTP(this.port, () => failed);
    if (this.proc !== proc) {
      throw new Error("gogen exited before it accepted connections");
    }
    this.sessionReady = new Promise((resolve, reject) => {
      this._resolveSession = resolve;
      this._sessionTimer = setTimeout(() => reject(new Error("session is not ready yet")), 10000);
    });
    try {
      await this.connect();
      await this.sessionReady;
    } catch (err) {
      clearTimeout(this._sessionTimer);
      throw err;
    }
  }

  connect() {
    if (typeof WebSocket === "undefined") {
      return Promise.reject(new Error("this VS Code is too old to open a WebSocket from the extension host"));
    }
    this.closeSocket();
    const url = `ws://127.0.0.1:${this.port}/ws`;
    const ws = new WebSocket(url);
    this.ws = ws;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        reject(new Error("timed out opening the chat socket"));
      }, 10000);
      ws.addEventListener("open", () => {
        clearTimeout(timer);
        this.emit({ type: "status", state: "ready", text: `connected on ${this.port}` });
        resolve();
      });
      ws.addEventListener("error", () => {
        clearTimeout(timer);
        reject(new Error("chat socket failed"));
      });
      ws.addEventListener("message", (ev) => {
        let msg;
        try {
          msg = JSON.parse(String(ev.data));
        } catch {
          return;
        }
        if (msg.sessionId) {
          this.sessionId = msg.sessionId;
          if (this._resolveSession) {
            clearTimeout(this._sessionTimer);
            this._resolveSession();
            this._resolveSession = null;
          }
        }
        this.emit({ type: "ws", msg });
      });
      ws.addEventListener("close", () => {
        if (this.ws === ws) {
          this.ws = null;
        }
      });
    });
  }

  send(text) {
    this.sendRaw({ type: "message", content: text });
  }

  // sendRaw writes one socket message. sessionId is filled here so every
  // call, including settings, hits this sidecar's session.
  sendRaw(msg) {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) {
      throw new Error("not connected");
    }
    if (!this.sessionId) {
      throw new Error("session is not ready yet");
    }
    // A caller that already set sessionId is naming a target (session
    // resume). Every other message is for this sidecar's live session.
    if (!msg.sessionId) {
      msg.sessionId = this.sessionId;
    }
    this.ws.send(JSON.stringify(msg));
  }

  closeSocket() {
    if (this.ws) {
      this.ws.close();
      this.ws = null;
    }
  }

  stop() {
    this.closeSocket();
    const proc = this.proc;
    this.proc = null;
    if (proc) {
      proc.kill();
    }
  }
}

function freePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer();
    srv.once("error", reject);
    srv.listen(0, "127.0.0.1", () => {
      const { port } = srv.address();
      srv.close(() => resolve(port));
    });
  });
}

function waitForHTTP(port, failed) {
  const deadline = Date.now() + 20000;
  return new Promise((resolve, reject) => {
    const attempt = () => {
      const early = failed();
      if (early) {
        reject(early);
        return;
      }
      const req = http.get({ host: "127.0.0.1", port, path: "/", timeout: 1000 }, (res) => {
        res.resume();
        resolve();
      });
      req.on("error", () => {
        const err = failed();
        if (err) {
          reject(err);
          return;
        }
        if (Date.now() > deadline) {
          reject(new Error("gogen did not start listening"));
          return;
        }
        setTimeout(attempt, 200);
      });
      req.on("timeout", () => req.destroy());
    };
    attempt();
  });
}

module.exports = { Sidecar };
