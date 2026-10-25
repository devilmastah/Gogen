# gogen-ai

Sidebar chat for [GoGen](https://github.com/gompa/Gogen), a local coding agent. The open folder is the working directory. Settings and the board open as editor tabs. The agent can open a file, reveal it in the explorer, and read the active editor when MCP is on.

## Requirements

- Visual Studio Code 1.99 or newer
- A gogen program, or a network connection to download one

## First launch

If `gogen` is already on your `PATH`, or **GoGen: Executable** points at a program that exists, the sidebar starts it.

Otherwise it asks you to pick an existing program, or a folder to download into. The download is the latest [gompa/Gogen](https://github.com/gompa/Gogen/releases) release for this computer, and it is kept only when it matches `checksums.txt`. The path is saved in your user settings.

The chat then asks for three things, and skips any that are already saved:

1. An OpenAI-compatible endpoint
2. A model
3. MCP, so the agent can open files in the editor. Turning it on restarts gogen.

## Settings

`gogen.executable` is the program the sidebar launches. Leave it as `gogen` to use `PATH`.

## Publishing

The Marketplace id is `gogen.gogen-ai`. The publisher id is `gogen`. Create that publisher, then from this folder:

```bash
npx @vscode/vsce package
npx @vscode/vsce publish
```

Open VSX uses the same `.vsix` (`npx ovsx publish`). Both need their own token. This repository does not publish them yet.

## License

GNU Affero General Public License v3.0. Source: https://github.com/gompa/Gogen
