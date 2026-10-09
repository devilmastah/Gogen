'use strict';
// Shared loader for the web UI regression tests (scripts/test_*.js).
//
// Evals the REAL app.js + web/components/*.js into a jsdom window with
// the ESM import/export syntax stripped: the component modules are eval'd
// first (dependency order), so their top-level declarations land in the
// global scope and app.js's bare references resolve — the same mechanism
// the editor.js / marked / DOMPurify window stubs use. app.js's own
// top-level function declarations become window properties, which is how
// the tests drive the UI (window.openBoardStartPopover, ...).
//
// Stubs (window.WebSocket, window.marked, editor functions, ...) must be
// installed BEFORE calling loadAppJs: app.js runs top-level code
// (marked.use, DOM wiring, connect()) during the eval.
const fs = require('fs');
const path = require('path');

// The editor.js functions app.js imports (the import is stripped above, so
// bare references resolve to the global scope and need a stub). Install
// these BEFORE loadAppJs. The conditional assignment is the contract: a
// test that pre-sets or later overrides a name (real-ish openModal/closeModal,
// a colorizeNode spy, ...) keeps its own implementation.
const EDITOR_STUBS = [
  'connectEditorSocket', 'setupEditorUI', 'refreshExplorer', 'disposeChatEditors',
  'mountDiffEditor', 'updateDiffEditor', 'updateDiffFallback', 'chatDiffWheelEdge',
  'extractDiffValue', 'scanDiffValueIncremental', 'initMonaco', 'colorizeNode', 'colorizeElement', 'sealStreamColorize',
  'beginColorizeDeferral', 'endColorizeDeferral',
  'languageFromPath', 'setToastHandler', 'focusFindInFiles', 'editorUndo',
  'editorRedo', 'saveAll', 'saveActive', 'openFileAtLine', 'setMonacoTheme',
  'applyEditorPrefs', 'applyPatchFromDiff', 'openModal', 'closeModal', 'escapeHtml',
];

function installEditorStubs(window, names = EDITOR_STUBS) {
  // escapeHtml returns a STRING, not a promise: app.js feeds the result
  // straight into setMessageMarkdown, so it needs a string-passthrough stub
  // (set before the generic promise default below).
  window.escapeHtml = window.escapeHtml || ((s) => String(s == null ? '' : s));
  // scanDiffValueIncremental returns the {ok, value, complete} shape app.js
  // reads synchronously — not a promise.
  window.scanDiffValueIncremental = window.scanDiffValueIncremental
    || (() => ({ ok: false, value: '', complete: false }));
  for (const name of names) {
    window[name] = window[name] || (() => Promise.resolve());
  }
}

// Evals the REAL components/storage.js into the window. Tests that load a
// single module standalone (editor.js) must call this first: editor.js
// references storageGet/storageSet at module top level, and its stripped
// import would otherwise leave those names undefined — the same mechanism
// installEditorStubs uses for editor.js's other imports.
function installStorage(window) {
  window.eval(stripModuleSyntax(fs.readFileSync(path.join(ROOT, 'internal/server/web/components/storage.js'), 'utf8')));
}

const ROOT = path.join(__dirname, '..');

// Component modules app.js imports, in eval (dependency) order.
const COMPONENT_MODULES = [
  // Safe localStorage wrappers (leaf module, no imports). First: board.js,
  // terminal.js and settings.js call storageGet/storageSet, so the function
  // declarations must land in the global scope before any of them eval.
  'internal/server/web/components/storage.js',
  // Leaf module (no imports of its own); first because several components
  // and app.js call icon() — its top-level declaration must land in the
  // global scope before any of them eval.
  'internal/server/web/components/icons.js',
  // Leaf module (no imports): off-screen paint skip that measures real
  // heights; app.js calls enablePaintSkip at each item's finalization.
  'internal/server/web/components/paint-skip.js',
  'internal/server/web/components/popover.js',
  // Shared decision-dialog plumbing (openDialog); imports editor.js
  // (openModal / closeModal, stubbed above). sessions.js and
  // delete-approval.js reference openDialog, so it must eval before them.
  'internal/server/web/components/dialog.js',
  // Markdown rendering pipeline; imports editor.js (colorizeNode /
  // openFileAtLine, stubbed) and the marked / DOMPurify vendor stubs.
  'internal/server/web/components/markdown.js',
  // Tool-result rendering; imports editor.js (colorizeElement, stubbed).
  'internal/server/web/components/tool-result.js',
  'internal/server/web/components/model-picker.js',
  'internal/server/web/components/board.js',
  // Automations tab; imports icons.js only. Eval'd before settings.js,
  // whose applyFeatureSettings toggles the automations tab.
  'internal/server/web/components/automations.js',
  'internal/server/web/components/toc.js',
  // Imports toc.js (updateTocActive / syncTocRailBox / hideTocTooltip) and
  // editor.js (chatDiffWheelEdge, stubbed above).
  'internal/server/web/components/scroll.js',
  // Command palette; imports editor.js (openModal / closeModal, stubbed).
  'internal/server/web/components/palette.js',
  'internal/server/web/components/terminal.js',
  'internal/server/web/components/sessions.js',
  // Status-display relay (browser mode); no imports, so order is free.
  'internal/server/web/components/status-display.js',
  // Declarative settings schema + renderer; settings.js imports both and
  // calls renderSettings() at eval time, so they must land first.
  'internal/server/web/components/settings-schema.js',
  'internal/server/web/components/settings-render.js',
  'internal/server/web/components/settings.js',
  // Delete-approval modal; imports editor.js (openModal / closeModal,
  // stubbed) and settings.js (sendNotification) — last, after both.
  'internal/server/web/components/delete-approval.js',
  // Composer input helpers (slash suggest + attachments); imports icons.js.
  'internal/server/web/components/composer.js',
];

// Strips the module syntax the tests run as classic scripts:
//   - import { a, b } from '...'  (multi-line, as in app.js's editor.js import)
//   - import X from '...'
//   - export function/async function/const/let/var/class declarations
//   - export { a, b }; re-export lists (e.g. editor.js's `export { monaco };`)
function stripModuleSyntax(src) {
  return src
    .replace(/import\s*\{[^}]*\}\s*from\s*['"][^'"]+['"];\s*/gs, '')
    .replace(/import\s+[A-Za-z_$][\w$]*\s+from\s*['"][^'"]+['"];\s*/g, '')
    .replace(/\bexport\s+(?=function|async\s+function|const|let|var|class)\s*/g, '')
    .replace(/\bexport\s*\{[^}]*\};\s*/g, '');
}

// jsdom does not implement Element.scrollTo/scrollBy, but app.js's
// pinToBottom/smartScroll use `messagesDiv.scrollTo({ top: 1 << 30 })`
// (browser clamps to the bottom). Emulate the clamped assignment so the
// old `scrollTop = scrollHeight` semantics hold under test (jsdom has no
// layout, so everything clamps to 0).
function installScrollStubs(window) {
  if (typeof window.Element.prototype.scrollTo === 'function') return;
  window.Element.prototype.scrollTo = function (opts) {
    const top = opts && typeof opts === 'object' ? opts.top : opts;
    const max = Math.max(0, this.scrollHeight - this.clientHeight);
    this.scrollTop = Math.min(Math.max(Number(top) || 0, 0), max);
  };
  window.Element.prototype.scrollBy = function (opts) {
    const dy = opts && typeof opts === 'object' ? opts.top : opts;
    this.scrollTop += Number(dy) || 0;
  };
}

// Evals the component modules (dependency order), then app.js, into the
// given jsdom window. APPJS env override (as before) for app.js only.
function loadAppJs(window) {
  installScrollStubs(window);
  for (const rel of COMPONENT_MODULES) {
    window.eval(stripModuleSyntax(fs.readFileSync(path.join(ROOT, rel), 'utf8')));
  }
  const appJs = process.env.APPJS || path.join(ROOT, 'internal/server/web/app.js');
  window.eval(stripModuleSyntax(fs.readFileSync(appJs, 'utf8')));
}

module.exports = { ROOT, COMPONENT_MODULES, stripModuleSyntax, loadAppJs, EDITOR_STUBS, installEditorStubs, installStorage, installScrollStubs };
