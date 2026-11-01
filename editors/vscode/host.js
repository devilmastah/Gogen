const fs = require("fs");
const path = require("path");
const vscode = require("vscode");
const { Sidecar } = require("./sidecar");
const { resolveBinary } = require("./binary");
const { EditorBridge } = require("./editorBridge");
const { queueIdOk } = require("./queueId");
const { withCodeIndexLine, acceptCodeIndexSeq } = require("./codeIndexView");
const {
  projectConfRels,
  templateSources,
  templateDest,
  gitignoreEntries,
  withGitignoreEntries,
  isNonEmptyFile,
} = require("./templateCopy");

// GogenHost owns the one sidecar for this window. The chat, the settings
// tab, and the board tab all talk through it, so there is one process and
// one .gogen directory.
class GogenHost {
  constructor(log, context) {
    this.log = log;
    this.context = context;
    this.sidecar = null;
    this.providers = [];
    this.catalog = [];
    this.model = "";
    this.modelProvider = "";
    this.appliedSavedModel = false;
    this.modelsRequested = false;
    this.refreshModels = false;
    this.lastConfig = null;
    this.lastStatus = null;
    this.listeners = new Set();
    this.modelsWait = null;
    this.resumeDone = false;
    this.chatted = false;
    this.lastContext = null;
    this.turnActive = false;
    this.tokensPerSec = 0;
    this.editorBridge = new EditorBridge(log, () => this.folderPath(), (file, signal) => this.askOutsidePath(file, signal));
    this.pathAsk = null;
    this.pathAskSeq = 0;
    this.approvalQueue = [];
    this.approvalSeen = new Set();
    this.approvalShowing = false;
    this.starting = null;
  }

  overrideFolder() {
    return this.context?.workspaceState.get("gogen.workspaceOverride") || "";
  }

  knownFolders() {
    const saved = this.context?.globalState.get("gogen.workspaces");
    return Array.isArray(saved) ? saved.filter((f) => typeof f === "string" && f) : [];
  }

  folderPath() {
    return this.overrideFolder() || vscode.workspace.workspaceFolders?.[0]?.uri.fsPath || "";
  }

  async rememberFolder(folder) {
    if (!folder || !this.context) {
      return;
    }
    const next = [folder].concat(this.knownFolders().filter((f) => !samePath(f, folder))).slice(0, 20);
    await this.context.globalState.update("gogen.workspaces", next);
  }

  async setOverride(folder) {
    if (!this.context) {
      return;
    }
    await this.context.workspaceState.update("gogen.workspaceOverride", folder || "");
  }

  subscribe(fn) {
    this.listeners.add(fn);
    if (this.lastStatus) {
      fn(this.lastStatus);
    }
    const config = this.configView();
    if (config) {
      fn(config);
    }
    if (this.catalog.length) {
      fn(this.modelsView());
    }
    const folder = this.folderPath();
    if (folder) {
      fn({ type: "workspace", label: path.basename(folder), folder });
    }
    if (this.lastContext) {
      fn(this.contextView(this.lastContext));
    }
    if (this.lastCodeIndex) {
      fn({ type: "codeIndex", phase: this.lastCodeIndex.phase, line: this.lastCodeIndex.line });
    }
    fn({ type: "turn", active: !!this.turnActive, tokensPerSec: this.tokensPerSec || 0 });
    return () => this.listeners.delete(fn);
  }

  // configView is the last server config push, plus the chat's model label.
  // Settings reads every field the web UI reads; the chat only uses the model.
  configView() {
    if (!this.lastConfig) {
      return null;
    }
    return withCodeIndexLine(Object.assign({}, this.lastConfig, {
      type: "config",
      modelLabel: this.currentModelLabel(),
    }), this.lastCodeIndex);
  }

  modelsView() {
    return {
      type: "models",
      model: this.model,
      modelProvider: this.modelProvider,
      modelLabel: this.currentModelLabel(),
      models: this.catalog,
    };
  }

  emit(event) {
    for (const fn of this.listeners) {
      fn(event);
    }
  }

  currentModelLabel() {
    if (!this.model) {
      return "Choose a model";
    }
    if (this.modelProvider && this.modelProvider !== "default") {
      return this.modelProvider + " · " + this.model;
    }
    return this.model;
  }

  hasEndpoint() {
    return this.providers.some((p) => p && p.baseUrl);
  }

  async ensureStarted() {
    if (!vscode.workspace.isTrusted) {
      if (this.sidecar) {
        const old = this.sidecar;
        this.sidecar = null;
        await old.stop();
      }
      const text = "Trust this folder before GoGen starts. GoGen runs commands in the open folder.";
      this.emit({ type: "status", state: "error", text });
      throw new Error(text);
    }
    if (this.starting) {
      return this.starting;
    }
    if (this.sidecar) {
      return;
    }
    const folder = this.folderPath();
    if (!folder) {
      this.emit({ type: "status", state: "error", text: "Open a folder. That folder is gogen's working directory." });
      throw new Error("Open a folder first");
    }
    const run = this.startSidecar(folder).finally(() => {
      if (this.starting === run) {
        this.starting = null;
      }
    });
    this.starting = run;
    return run;
  }

  async startSidecar(folder) {
    let executable;
    try {
      executable = await resolveBinary(this.log);
    } catch (err) {
      this.emit({ type: "status", state: "error", text: err.message || String(err) });
      throw err;
    }
    await copyTemplateConf(folder, executable, this.log);
    await this.editorBridge.start();
    const env = this.editorBridge.mcpEnv(this.context.extensionPath);
    const sidecar = new Sidecar(folder, executable, this.log, env);
    this.sidecar = sidecar;
    this.providers = [];
    this.catalog = [];
    this.model = "";
    this.modelProvider = "";
    this.appliedSavedModel = false;
    this.modelsRequested = false;
    this.refreshModels = false;
    this.lastConfig = null;
    this.lastContext = null;
    this.turnActive = false;
    this.tokensPerSec = 0;
    sidecar.onEvent((event) => this.onSidecar(event));
    this.emit({ type: "status", state: "starting", text: "Starting gogen in " + path.basename(folder) + "…" });
    try {
      await sidecar.start();
      this.emit({ type: "status", state: "ready", text: "connected · " + path.basename(folder) });
      this.emit({ type: "workspace", label: path.basename(folder), folder });
      this.rememberFolder(folder);
      this.requestResume();
    } catch (err) {
      // A restart stops this start on purpose, so the failure it causes is
      // not a startup error and must not flash the status line: the next
      // start owns it. Only the start that is still current reports.
      const current = this.sidecar === sidecar;
      if (current) {
        this.sidecar = null;
      }
      await sidecar.stop();
      if (current) {
        this.emit({ type: "status", state: "error", text: err.message });
      }
      throw err;
    }
  }

  async restart() {
    this.resumeDone = false;
    this.chatted = false;
    const pending = this.starting;
    if (this.sidecar) {
      const old = this.sidecar;
      this.sidecar = null;
      await old.stop();
    }
    if (pending) {
      await pending.catch(() => {});
    }
    await this.ensureStarted();
  }

  // onWorkspaceFoldersChanged restarts the sidecar when the first workspace
  // folder changes. gogen's working directory is fixed at process start, so
  // the new folder's .gogen is a different process. Adding another folder
  // beside the first one leaves the running process where it is.
  onWorkspaceFoldersChanged() {
    const run = this.folderWatch || Promise.resolve();
    this.folderWatch = run.then(() => this.applyWorkspaceFolder()).catch((err) => {
      this.log.appendLine(String(err && err.message ? err.message : err));
    });
  }

  async applyWorkspaceFolder() {
    if (!this.sidecar || this.overrideFolder()) {
      return;
    }
    const folder = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath || "";
    if (samePath(this.sidecar.folder, folder)) {
      return;
    }
    this.log.appendLine("workspace is now " + (folder || "(none)"));
    this.emit({ type: "history", entries: [] });
    if (!folder) {
      const old = this.sidecar;
      this.sidecar = null;
      await old.stop();
      this.lastConfig = null;
      this.providers = [];
      this.catalog = [];
      this.model = "";
      this.modelProvider = "";
      this.emit({ type: "status", state: "error", text: "Open a folder. That folder is gogen's working directory." });
      return;
    }
    await this.restart();
  }

  onSidecar(event) {
    if (event.type === "status") {
      this.lastStatus = event;
      this.emit(event);
      return;
    }
    const msg = event.msg;
    if (!msg || typeof msg.type !== "string") {
      return;
    }
    switch (msg.type) {
      case "history":
        // clear_chat is not forwarded. The server sends this history frame
        // right after it, and the chat replaces the transcript from here.
        this.emit({ type: "history", entries: msg.history || [] });
        break;
      case "thinking_token":
        this.emit({ type: "thinking", text: msg.content || "" });
        break;
      case "stream":
        this.emit({ type: "token", text: msg.content || "" });
        break;
      case "tool_execute":
        this.emit({ type: "tool", name: msg.tool || "tool" });
        break;
      case "tool_result":
        this.emit({
          type: "toolResult",
          name: msg.tool || "tool",
          text: msg.result || "",
          success: msg.success !== false,
        });
        break;
      case "response":
        // A reply can answer a send that never started a turn. The chat
        // view settles that send.
        this.emit({ type: "notice", reply: true, queueId: msg.queueId || "", text: msg.content || "" });
        break;
      case "delete_approval":
      case "command_approval":
        this.enqueueApproval(msg);
        break;
      case "turn_end":
        this.tokensPerSec = 0;
        this.emit({ type: "turnEnd" });
        this.setTurn(false);
        break;
      case "context":
        this.lastContext = msg;
        this.emit(this.contextView(msg));
        break;
      case "stream_stats":
        this.tokensPerSec = msg.tokensPerSec || 0;
        this.emit({ type: "stats", tokensPerSec: this.tokensPerSec });
        break;
      case "session_state":
        this.setTurn(!!msg.turnActive);
        this.emit({ type: "queue", items: Array.isArray(msg.queue) ? msg.queue : [], left: [] });
        break;
      case "queue_update":
        // content is the message the started turn runs (queueLeft non-empty).
        this.emit({
          type: "queue",
          items: Array.isArray(msg.queue) ? msg.queue : [],
          left: Array.isArray(msg.queueLeft) ? msg.queueLeft : [],
          text: msg.content || "",
        });
        break;
      case "user_acked":
        // The server's turn for this message has started.
        this.setTurn(true);
        this.emit({ type: "userAcked" });
        break;
      case "code_index_progress":
        this.noteCodeIndex(msg.codeIndexPhase || "", msg.codeIndexLine || "", msg.codeIndexSeq);
        break;
      case "config":
        if (Array.isArray(msg.providers)) {
          this.providers = msg.providers;
        }
        this.lastConfig = msg;
        if (typeof msg.codeIndexPhase === "string") {
          this.noteCodeIndex(msg.codeIndexPhase, msg.codeIndexLine || "", msg.codeIndexSeq);
        }
        this.model = msg.model || "";
        this.modelProvider = msg.modelProvider || "";
        this.emit(this.configView());
        if (this.mcpRestartWait && String(msg.mcp || "").toLowerCase() === "on") {
          const finish = this.mcpRestartWait;
          finish();
        }
        this.applySavedModel(this.model);
        if (!this.modelsRequested || this.refreshModels) {
          this.modelsRequested = true;
          this.refreshModels = false;
          this.requestModels();
        }
        break;
      case "models":
        this.catalog = msg.models || [];
        if (msg.model) {
          this.model = msg.model;
          this.modelProvider = msg.modelProvider || "";
        }
        if (this.modelsWait) {
          const done = this.modelsWait;
          this.modelsWait = null;
          done(this.catalog);
        }
        this.emit(this.modelsView());
        break;
      case "provider_test":
        this.emit({ type: "providerTest", result: msg.providerTest || {} });
        break;
      case "mcp_test":
        this.emit({ type: "mcpTest", result: msg.mcpTestResult || {} });
        break;
      case "board_state":
        this.emit({ type: "board", state: msg.boardState || { columns: [], items: [] } });
        break;
      case "sessions":
        this.resumeLatest(msg.sessions || []);
        break;
      case "notice":
        if (msg.content) {
          this.emit({ type: "notice", kind: msg.kind || "", text: msg.content });
        }
        break;
      default:
        break;
    }
  }

  requestModels() {
    if (!this.sidecar) {
      return Promise.resolve(this.catalog);
    }
    return new Promise((resolve) => {
      const timer = setTimeout(() => {
        this.modelsWait = null;
        resolve(this.catalog);
      }, 8000);
      this.modelsWait = (models) => {
        clearTimeout(timer);
        resolve(models);
      };
      try {
        this.sidecar.sendRaw({ type: "list_models" });
      } catch (err) {
        clearTimeout(timer);
        this.modelsWait = null;
        this.emit({ type: "notice", text: err.message });
        resolve(this.catalog);
      }
    });
  }

  applySavedModel(current) {
    if (current || this.appliedSavedModel || !this.sidecar) {
      return;
    }
    const saved = this.providers.filter((p) => p && p.model);
    if (saved.length !== 1) {
      return;
    }
    this.appliedSavedModel = true;
    const p = saved[0];
    const selector = p.name && p.name !== "default" ? p.name + "/" + p.model : p.model;
    try {
      this.sidecar.sendRaw({ type: "set_model", model: selector });
    } catch (err) {
      this.appliedSavedModel = false;
      this.emit({ type: "notice", text: err.message });
    }
  }

  // contextView is the context frame the web toolbar already renders:
  // the meter, the near-compact flag, and the session totals.
  contextView(msg) {
    return {
      type: "context",
      usedTokens: msg.usedTokens || 0,
      contextLimit: msg.contextLimit || 0,
      usedSource: msg.usedSource || "",
      promptTokens: msg.promptTokens || 0,
      completionTokens: msg.completionTokens || 0,
      cachedTokens: msg.cachedTokens || 0,
      compactAt: msg.compactAt || 0,
      messageCount: msg.messageCount || 0,
      warnNearCompact: !!msg.warnNearCompact,
      totalPromptTokens: msg.totalPromptTokens || 0,
      totalCompletionTokens: msg.totalCompletionTokens || 0,
      totalCachedTokens: msg.totalCachedTokens || 0,
      totalTurns: msg.totalTurns || 0,
    };
  }

  // noteCodeIndex remembers the latest index snapshot and tells the chat.
  // The spinner is shown only while a refresh is running. A frame whose
  // sequence is older than the one already shown is ignored.
  noteCodeIndex(phase, line, seq) {
    const prev = this.lastCodeIndex && this.lastCodeIndex.seq;
    if (!acceptCodeIndexSeq(prev, seq)) return;
    this.lastCodeIndex = { phase: phase || "", line: line || "", seq: Number(seq) || 0 };
    this.emit({ type: "codeIndex", phase: this.lastCodeIndex.phase, line: this.lastCodeIndex.line });
  }

  setTurn(active) {
    this.turnActive = !!active;
    if (!this.turnActive) {
      this.tokensPerSec = 0;
    }
    this.emit({ type: "turn", active: this.turnActive, tokensPerSec: this.tokensPerSec || 0 });
  }

  // sendChat sends a message the chat has already drawn, tagged with its
  // queue id. The server either queues it or, when no turn is running, runs
  // it, and the chat moves the text when queue_update or user_acked says so.
  // A queue id that is not a short token is omitted; the server then assigns
  // its own.
  sendChat(text, queueId) {
    this.chatted = true;
    const msg = { type: "message", content: text };
    if (queueIdOk(queueId)) msg.queueId = queueId;
    this.sidecar.sendRaw(msg);
  }

  // sendQueueNow names the queued message the control is on. The server
  // ignores the request once that message has left the queue.
  sendQueueNow(id) {
    if (!queueIdOk(id)) {
      return;
    }
    this.sidecar.sendRaw({ type: "queue_send_now", queueId: id });
  }

  removeQueued(id) {
    if (!queueIdOk(id)) {
      return;
    }
    this.sidecar.sendRaw({ type: "queue_remove", queueId: id });
  }

  setMode(mode) {
    this.sidecar.sendRaw({ type: "set_mode", mode });
  }

  setThinking(level) {
    this.sidecar.sendRaw({ type: "set_thinking_level", thinkingLevel: level });
  }

  cancelTurn() {
    this.sidecar.sendRaw({ type: "cancel" });
  }

  compact() {
    this.sidecar.sendRaw({ type: "compact" });
  }

  pickThinking() {
    return chooseThinking(this).catch((err) => {
      this.emit({ type: "notice", text: err.message || String(err) });
    });
  }

  requestResume() {
    if (!this.sidecar || this.resumeDone || this.chatted) {
      return;
    }
    try {
      this.sidecar.sendRaw({ type: "list_sessions" });
    } catch (err) {
      this.log.appendLine(String(err && err.message ? err.message : err));
    }
  }

  // openSession attaches the chat to a session that already exists, such as
  // the agent a board ticket started. sessionId on this message is the
  // target, so the sidecar must not replace it with the live session.
  openSession(id) {
    if (!id || !this.sidecar) {
      return;
    }
    this.resumeDone = true;
    this.sidecar.sendRaw({ type: "session_resume", sessionId: id });
  }

  // resumeLatest opens the newest saved top-level session in this folder.
  // A fresh process would otherwise start blank even though the transcript
  // is still on disk. The board comes back on its own from .gogen/board.
  resumeLatest(sessions) {
    if (this.resumeDone || this.chatted || !this.sidecar) {
      return;
    }
    this.resumeDone = true;
    const current = this.sidecar.sessionId;
    const saved = sessions.find((s) => s && s.id && s.id !== current && !s.parentId && !s.oneshot && s.messageCount > 0);
    if (!saved) {
      return;
    }
    try {
      this.sidecar.sendRaw({ type: "session_resume", sessionId: saved.id });
    } catch (err) {
      this.resumeDone = false;
      this.emit({ type: "notice", text: err.message });
    }
  }

  pickModel(provider, id) {
    const name = provider || "default";
    const selector = name !== "default" ? name + "/" + id : id;
    this.sidecar.sendRaw({ type: "set_model", model: selector });
    this.rememberModel(name, id);
  }

  rememberModel(provider, id) {
    if (!id || !this.sidecar) {
      return;
    }
    const name = provider || "default";
    const known = this.providers.find((p) => p.name === name);
    const url = (known && known.baseUrl) || "";
    if (name !== "default" && !url) {
      return;
    }
    try {
      this.sidecar.sendRaw({
        type: "provider_save",
        providerOp: { name, baseUrl: url, model: id },
      });
    } catch (err) {
      this.emit({ type: "notice", text: err.message });
    }
  }

  saveProvider(name, baseUrl, apiKey, model) {
    const op = { name, baseUrl };
    if (apiKey) {
      op.apiKey = apiKey;
    }
    if (model !== undefined) {
      op.model = model;
    }
    this.refreshModels = true;
    this.sidecar.sendRaw({ type: "provider_save", providerOp: op });
  }

  deleteProvider(name) {
    this.refreshModels = true;
    this.sidecar.sendRaw({ type: "provider_delete", providerOp: { name } });
  }

  testProvider(op) {
    this.sidecar.sendRaw({ type: "test_provider", providerOp: op });
  }

  testMcp(mcpTest) {
    this.sidecar.sendRaw({ type: "test_mcp", mcpTest });
  }

  // sendFeature writes a feature flag the way the web settings modal does:
  // a config message with the flag fields and no configFields list.
  sendFeature(fields) {
    this.sidecar.sendRaw(Object.assign({ type: "config" }, fields));
  }

  reindexCode() {
    if (!this.sidecar) return;
    this.sidecar.sendRaw({ type: "reindex_code" });
  }

  // sendRuntime writes one runtime setting. configFields names it so an
  // explicit empty or zero value is applied instead of ignored.
  sendRuntime(field, prop, value) {
    const msg = { type: "config", configFields: [field] };
    msg[prop || field] = value;
    this.sidecar.sendRaw(msg);
  }

  boardOp(boardOp) {
    this.sidecar.sendRaw({ type: "board_op", boardOp });
  }

  enableBoard() {
    this.sidecar.sendRaw({ type: "config", board: "on" });
  }

  // enableMcpAndRestart turns MCP on and restarts once the save is echoed.
  // The editor bridge is decided when the process starts, so the running
  // sidecar cannot pick the switch up itself.
  enableMcpAndRestart() {
    if (this.mcpRestarting || !this.sidecar) {
      return;
    }
    this.mcpRestarting = true;
    const finish = () => {
      if (this.mcpRestartWait !== finish) {
        return;
      }
      this.mcpRestartWait = null;
      this.restart().finally(() => {
        this.mcpRestarting = false;
      });
    };
    this.mcpRestartWait = finish;
    setTimeout(() => {
      if (this.mcpRestartWait !== finish) {
        return;
      }
      if (!this.lastConfig || String(this.lastConfig.mcp || "").toLowerCase() !== "on") {
        this.mcpRestartWait = null;
        this.mcpRestarting = false;
        this.emit({ type: "notice", text: "MCP did not turn on." });
        return;
      }
      finish();
    }, 4000);
    try {
      this.sendRuntime("mcp", "mcp", "on");
    } catch (err) {
      this.mcpRestartWait = null;
      this.mcpRestarting = false;
      this.emit({ type: "notice", text: err.message });
    }
  }

  // askOutsidePath shows the chat dialog for one path outside the workspace.
  // The notification is the same wait, so a background window still shows it.
  // Both close when the user answers or when gogen cancels the tool call.
  askOutsidePath(file, signal) {
    const id = ++this.pathAskSeq;
    let endNote = () => {};
    const gate = new Promise((resolve) => {
      endNote = resolve;
    });
    vscode.window.withProgress(
      {
        location: vscode.ProgressLocation.Notification,
        title: "GoGen is waiting for a path outside this workspace",
      },
      () => gate,
    );
    return new Promise((resolve, reject) => {
      let ended = false;
      const finish = (err, allow) => {
        if (ended) {
          return;
        }
        ended = true;
        if (this.pathAsk && this.pathAsk.id === id) {
          this.pathAsk = null;
        }
        endNote();
        this.emit({ type: "pathApproval", id, hide: true });
        if (err) {
          reject(err);
          return;
        }
        resolve(allow);
      };
      this.pathAsk = { id, path: file, finish };
      this.emit({ type: "pathApproval", id, path: file });
      if (signal) {
        const onAbort = () => finish(new Error("the path approval ended"));
        if (signal.aborted) {
          onAbort();
        } else {
          signal.addEventListener("abort", onAbort, { once: true });
        }
      }
    });
  }

  answerPath(id, allow) {
    const ask = this.pathAsk;
    if (!ask || ask.id !== id) {
      return;
    }
    ask.finish(null, allow === true);
  }

  replayPathApproval() {
    const ask = this.pathAsk;
    if (ask) {
      this.emit({ type: "pathApproval", id: ask.id, path: ask.path });
    }
  }

  // enqueueApproval shows one Allow / Deny dialog at a time. The server
  // holds the turn until delete_approval_response or
  // command_approval_response arrives. Esc and closing the dialog deny.
  // approvalSeen blocks a second frame for an id that is queued or on
  // screen. The id is dropped once that dialog is answered, so a later
  // re-notify can ask again and the set does not grow for the life of the
  // window. Closing the window drops this queue. The server then waits out
  // its own approval hold, the same as the web UI when the page goes away.
  // dispose does not send a denial: the socket is already closing.
  enqueueApproval(msg) {
    if (!msg.approvalId || this.approvalSeen.has(msg.approvalId)) {
      return;
    }
    this.approvalSeen.add(msg.approvalId);
    this.approvalQueue.push(msg);
    this.presentApproval();
  }

  async presentApproval() {
    if (this.approvalShowing || this.approvalQueue.length === 0) {
      return;
    }
    this.approvalShowing = true;
    const msg = this.approvalQueue.shift();
    const command = msg.type === "command_approval";
    const title = command ? "GoGen wants to run a blocked command" : "GoGen wants to delete files";
    const allow = command ? "Allow command" : "Allow delete";
    let detail = "";
    if (command) {
      detail = msg.command || "";
      if (msg.reason) {
        detail += (detail ? "\n\n" : "") + "Matched: " + msg.reason;
      }
    } else {
      const paths = Array.isArray(msg.paths) ? msg.paths : [];
      const shown = paths.slice(0, 20);
      detail = shown.join("\n");
      if (paths.length > shown.length) {
        detail += "\n… and " + (paths.length - shown.length) + " more";
      }
      if (msg.reason) {
        detail += (detail ? "\n\n" : "") + "Requested by: " + msg.reason;
      }
    }
    this.emit({ type: "notice", text: title });
    let pick;
    try {
      pick = await vscode.window.showWarningMessage(title, { modal: true, detail }, allow, "Deny");
    } catch (err) {
      this.log.appendLine("approval dialog failed: " + (err && err.message ? err.message : err));
      pick = undefined;
    }
    const approved = pick === allow;
    try {
      this.sidecar.sendRaw({
        type: command ? "command_approval_response" : "delete_approval_response",
        approvalId: msg.approvalId,
        approved,
        sessionId: msg.sessionId,
      });
    } catch (err) {
      this.log.appendLine("approval response failed: " + (err && err.message ? err.message : err));
    }
    this.emit({ type: "notice", text: (approved ? "Allowed" : "Denied") + (command ? " the command" : " the delete") });
    this.approvalShowing = false;
    this.approvalSeen.delete(msg.approvalId);
    this.presentApproval();
  }

  dispose() {
    if (this.sidecar) {
      // Unload cannot wait here. stop still force-kills if the process lingers.
      this.sidecar.stop();
      this.sidecar = null;
    }
    if (this.editorBridge) {
      this.editorBridge.stop();
    }
  }
}

async function chooseModel(host) {
  await host.ensureStarted();
  if (!host.hasEndpoint()) {
    const open = "Open Settings";
    const choice = await vscode.window.showInformationMessage("Save an endpoint before choosing a model.", open);
    if (choice === open) {
      await vscode.commands.executeCommand("gogen.settings");
    }
    return;
  }
  const models = host.catalog.length ? host.catalog : await vscode.window.withProgress(
    { location: vscode.ProgressLocation.Notification, title: "Loading models" },
    () => host.requestModels(),
  );
  if (!models.length) {
    vscode.window.showWarningMessage("The endpoint returned no models.");
    return;
  }
  const picked = await vscode.window.showQuickPick(models.map((m) => ({
    label: m.id,
    description: m.provider && m.provider !== "default" ? m.provider : "",
    provider: m.provider || "default",
    id: m.id,
  })), {
    placeHolder: host.currentModelLabel(),
    matchOnDescription: true,
  });
  if (!picked) {
    return;
  }
  host.pickModel(picked.provider, picked.id);
}

async function chooseThinking(host) {
  await host.ensureStarted();
  const cfg = host.lastConfig || {};
  if (cfg.reasoningEffortsUnsupported) {
    return;
  }
  const efforts = Array.isArray(cfg.reasoningEfforts) && cfg.reasoningEfforts.length
    ? cfg.reasoningEfforts
    : ["low", "medium", "high"];
  const current = cfg.thinkingLevel || "off";
  const items = [{ label: "Off", value: "off", description: current === "off" ? "current" : "" }];
  for (const value of efforts) {
    if (!value || value === "off") {
      continue;
    }
    const label = value.charAt(0).toUpperCase() + value.slice(1);
    items.push({ label, value, description: value === current ? "current" : "" });
  }
  const picked = await vscode.window.showQuickPick(items, { placeHolder: "Thinking level" });
  if (!picked) {
    return;
  }
  host.setThinking(picked.value);
}

async function chooseWorkspace(host) {
  const open = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath || "";
  const items = [];
  if (open) {
    items.push({ label: "$(folder) Open folder", description: open, useOpen: true });
  }
  for (const folder of host.knownFolders()) {
    items.push({
      label: path.basename(folder),
      description: folder,
      folder,
    });
  }
  items.push({ label: "$(folder-opened) Choose folder…", pick: true });
  const picked = await vscode.window.showQuickPick(items, {
    placeHolder: host.folderPath() || "GoGen workspace",
    matchOnDescription: true,
  });
  if (!picked) {
    return;
  }
  let folder = "";
  if (picked.pick) {
    const uris = await vscode.window.showOpenDialog({
      canSelectFolders: true,
      canSelectFiles: false,
      canSelectMany: false,
      openLabel: "Use as GoGen workspace",
    });
    if (!uris || !uris[0]) {
      return;
    }
    folder = uris[0].fsPath;
  } else if (!picked.useOpen) {
    folder = picked.folder;
  }
  await host.setOverride(folder);
  if (folder) {
    await host.rememberFolder(folder);
  }
  await host.restart();
}

// copyTemplateConf offers a config and guidelines file that sit beside the
// gogen executable when the opened folder has no conf of its own. The user
// chooses the copy, or the chat setup. Bytes are copied as they are. A
// folder that already has a conf is left alone, and the file beside the
// executable is never rewritten. Closing the question skips the copy.
async function copyTemplateConf(folder, executable, log) {
  if (!folder || !executable) {
    return;
  }
  if (projectHasConf(folder)) {
    return;
  }
  const sources = templateSources(path.dirname(executable)).filter((found) => {
    return !samePath(found.file, templateDest(folder, found.rel));
  });
  if (sources.length === 0) {
    return;
  }
  const copy = "Copy config and guidelines";
  const setup = "Start setup";
  let choice;
  try {
    choice = await vscode.window.showInformationMessage(
      "This folder has no GoGen config yet",
      {
        modal: true,
        detail: "Copy the config and guidelines from beside the gogen program, or set this folder up in the chat.\n\n" + sources.map((found) => found.rel).join("\n"),
      },
      copy,
      setup,
    );
  } catch (err) {
    log.appendLine("config copy ask failed: " + (err && err.message ? err.message : err));
    return;
  }
  if (choice !== copy) {
    log.appendLine("config copy skipped; setup will run in the chat");
    return;
  }
  const copied = [];
  for (const found of sources) {
    const dest = templateDest(folder, found.rel);
    try {
      fs.mkdirSync(path.dirname(dest), { recursive: true });
      fs.copyFileSync(found.file, dest);
    } catch (err) {
      log.appendLine("config copy failed: " + (err.message || String(err)));
      continue;
    }
    log.appendLine("copied config into " + dest);
    copied.push(dest);
  }
  if (copied.length > 0) {
    await offerGogenGitignore(folder, copied, log);
  }
}

// offerGogenGitignore asks before adding the copied paths to a .gitignore
// that is already in the folder. .gogen/ covers a guidelines file inside
// that directory. A guidelines file outside it is added on its own line.
// A folder with no .gitignore is left as it is.
async function offerGogenGitignore(folder, copied, log) {
  const ignorePath = path.join(folder, ".gitignore");
  let text;
  try {
    text = fs.readFileSync(ignorePath, "utf8");
  } catch {
    return;
  }
  const entries = gitignoreEntries(folder, copied, text);
  if (entries.length === 0) {
    return;
  }
  const add = "Add to .gitignore";
  let choice;
  try {
    choice = await vscode.window.showInformationMessage(
      "GoGen copied files that git does not ignore",
      {
        modal: true,
        detail: "Add these so the copy, including any keys, stays out of git?\n\n" + entries.join("\n"),
      },
      "Leave it",
      add,
    );
  } catch (err) {
    log.appendLine("gitignore ask failed: " + (err && err.message ? err.message : err));
    return;
  }
  if (choice !== add) {
    return;
  }
  try {
    fs.writeFileSync(ignorePath, withGitignoreEntries(text, entries));
  } catch (err) {
    log.appendLine("gitignore update failed: " + (err.message || String(err)));
    return;
  }
  log.appendLine("added " + entries.join(", ") + " to " + ignorePath);
}

function projectHasConf(folder) {
  return projectConfRels.some((rel) => isNonEmptyFile(path.join(folder, rel)));
}

function samePath(a, b) {
  if (!a || !b) {
    return a === b;
  }
  const left = path.resolve(a);
  const right = path.resolve(b);
  if (process.platform === "win32") {
    return left.toLowerCase() === right.toLowerCase();
  }
  return left === right;
}

module.exports = { GogenHost, chooseModel, chooseWorkspace };
