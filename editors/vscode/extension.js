const vscode = require("vscode");
const { ChatViewProvider } = require("./chatView");
const { GogenHost, chooseModel, chooseWorkspace } = require("./host");
const { openSettings } = require("./settingsPanel");
const { openBoard } = require("./boardPanel");

function activate(context) {
  const log = vscode.window.createOutputChannel("GoGen");
  const host = new GogenHost(log, context);
  const provider = new ChatViewProvider(host);
  context.subscriptions.push(
    log,
    vscode.window.registerWebviewViewProvider("gogen.chat", provider, {
      webviewOptions: { retainContextWhenHidden: true },
    }),
    vscode.commands.registerCommand("gogen.restart", () => host.restart()),
    vscode.commands.registerCommand("gogen.pickModel", () => chooseModel(host)),
    vscode.commands.registerCommand("gogen.workspace", () => chooseWorkspace(host)),
    vscode.commands.registerCommand("gogen.settings", () => openSettings(host)),
    vscode.commands.registerCommand("gogen.board", () => openBoard(host)),
    vscode.workspace.onDidChangeWorkspaceFolders(() => host.onWorkspaceFoldersChanged()),
    { dispose: () => host.dispose() },
  );
}

function deactivate() {}

module.exports = { activate, deactivate };
