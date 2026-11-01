# gogen-ai

Sidebar chat for [GoGen](https://github.com/gompa/Gogen), a local coding agent. The open folder is the working directory. GoGen explores that project, edits files, runs commands, and checks the result. Sessions, settings, and the project board stay in that folder's `.gogen` directory, the same state the terminal and the browser use.

The chat is the GoGen sidebar. **GoGen: Settings** and **GoGen: Board** open as editor tabs. **GoGen: Choose Model**, **GoGen: Workspace**, and **GoGen: Restart sidecar** are in the command palette. The header menu in the chat opens Settings, Board, and the working folder.

## Requirements

- Visual Studio Code 1.99 or newer
- A trusted folder. In Restricted Mode the sidebar stays off, because GoGen runs commands in that folder
- A gogen program, or a network connection to download one

## First launch

If `gogen` is already on your `PATH`, or **GoGen: Executable** points at a program that exists, the sidebar starts it.

Otherwise it asks you to pick an existing program, or a folder to download into. The download is the latest [gompa/Gogen](https://github.com/gompa/Gogen/releases) release for this computer, and it is kept only when it matches `checksums.txt` from that same release. That match catches a damaged download. It does not prove the release itself is the one the project published. The path is saved in your user settings.

The chat then asks for three things, and skips any that are already saved:

1. An OpenAI-compatible endpoint
2. A model
3. MCP, so the agent can open files in the editor. Turning it on restarts gogen.

## Chat

You describe the change in the composer. Replies, thinking, and tool calls are separate blocks. **Hide tools** collapses the tool cards.

**Act** lets the agent edit and run commands. **Plan** keeps the turn read-only. The model pill opens **GoGen: Choose Model**. The thinking control sets the reasoning level for this session. The ring beside them is how full the context window is. **Cancel** stops the turn that is running and drops messages you had queued behind it.

A message sent while a turn is running waits, pinned above the composer. It is not copied into the transcript until that turn takes it. Each queued message runs as its own turn, in order. With **Join queued messages** on (Agent settings, `join_queued` in `.gogen/gogen.conf`), the messages typed during the same turn go out as one message, in the order they were typed. **Send now**, on the last of them, lets the current round finish, including the tools it already asked for; the queue then runs from its oldest message. **Remove** drops one queued message. A send gogen refuses (the queue is full, or another tab's turn holds the session) comes back into the composer, ahead of anything you typed since. In the terminal UI the same action is `s` in `/queue`. In the browser, **Send now** is on the last queued message.

A delete, and a command the safety rules block, asks in an editor dialog. Closing that dialog denies the request.

When the folder has no config of its own yet, the sidebar asks whether to copy the config and guidelines that sit beside the gogen program, or to set the folder up in the chat. A copied config lands in `.gogen/gogen.conf`. A guidelines file keeps the same path it has beside the program. If the folder's `.gitignore` does not already ignore what was copied, the sidebar asks before adding it. `.gogen/` covers a guidelines file inside that directory. A guidelines file outside it, such as `GOGEN.md`, is added on its own line. A folder that already has a config keeps it. Saving settings writes that folder's file and leaves the file beside the program unchanged. A global config under `~/.config/gogen` is used only when gogen itself is started in global mode.

## Board

**GoGen: Board** is the project's kanban board, stored in `.gogen/board`. The columns are backlog, ready, in progress, in review, blocked, and done. Drag a card from any column to any other.

Add a ticket from the board. Open a card for its description, context, and activity. You can comment, edit, block it with a reason, mark it done, or remove it. **Start** runs an agent on that ticket. **Open in chat** resumes the session that agent already has.

If the board is switched off, the tab says so and can turn it on. With the review agent on, moving a ticket to **in review** starts a review session. That session then marks the ticket done, or comments and sends it back to **in progress**.

## Settings

**GoGen: Settings** edits the settings gogen stores for this folder. The pages are Agent, Subagents, Automations, Tools, Embeddings, Context, Security, Sessions, Providers, MCP, and Server.

That includes the endpoint and model, the code index, the project board, subagents, scheduled automations, web fetch and search, tree-sitter, context and compaction, the command guard, delete approval, and saved-session limits. A blank secret leaves the stored secret in place. MCP and the server options apply the next time gogen starts.

Optional features stay off until you turn them on. The command guard still blocks what it is set to block, and a delete still asks unless delete approval is off.

## Files in the editor

With MCP on, the agent can open a file, reveal it in the explorer, and read the tab you already have open. A path outside the VS Code workspace, and outside the folder gogen is running in, asks you once in the chat for that call. The question and its notification close when you answer, or when gogen ends the tool call.

## Working folder

One window runs one gogen process, in the folder VS Code has open. **GoGen: Workspace** points that process at another folder and restarts it there. The chat resumes that folder's latest session, and the board is that folder's board. Opening the folder again later picks up the same `.gogen` directory.

`gogen.executable` is the program the sidebar launches. Leave it as `gogen` to use `PATH`. It is a user setting. A folder's `.vscode/settings.json` cannot change it.

## License

GNU Affero General Public License v3.0. Source: https://github.com/gompa/Gogen
