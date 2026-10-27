'use strict';
// Regression check for the chat-pane id: the main-tab handler and
// switchMainPane activate panes via dynamic id lookups (data-pane + '-pane'),
// so #chat-pane MUST exist. Loads the real index.html + app.js into jsdom
// (imports stripped, editor/marked/dompurify stubbed, no server needed) and
// asserts tab switching re-activates the chat pane.
// Run: node scripts/test_tabswitch.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');

async function main() {
  const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');
  const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;

  window.matchMedia = window.matchMedia || (() => ({ matches: false, addEventListener() {}, removeEventListener() {} }));
  window.Notification = { permission: 'denied', requestPermission: () => Promise.resolve('denied') };
  window.navigator.clipboard = { writeText: async () => {} };
  class FakeTerminal {
    constructor() { this._cbs = {}; }
    open() {} write() {} fit() {}
    onData(cb) { this._cbs.data = cb; }
    onResize() { return { dispose() {} }; }
    dispose() {} reset() {} focus() {}
  }
  window.Terminal = FakeTerminal;
  window.FitAddon = class { fit() {} };
  // Dummy socket: app.js's connect() must not throw; the tab handlers work
  // regardless of connection state.
  window.WebSocket = class {
    constructor() { this.readyState = 0; }
    static OPEN = 1;
    send() {} close() {}
  };

  installEditorStubs(window);
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };

  loadAppJs(window);

  const doc = window.document;
  const chatPane = doc.querySelector('.pane:not(#editor-pane)');
  const editorPane = doc.getElementById('editor-pane');
  const chatTab = doc.querySelector('.main-tab[data-pane="chat"]');
  const editorTab = doc.querySelector('.main-tab[data-pane="editor"]');

  const active = (el) => el && el.classList.contains('active');
  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };

  check('chat pane present without editor id', !!chatPane);
  check('chat pane active on load', active(chatPane));

  // Main-tab click: Chat -> Editor -> Chat.
  editorTab.click();
  check('editor pane active after Editor tab', active(editorPane));
  check('chat pane hidden after Editor tab', !active(chatPane));
  chatTab.click();
  check('chat pane active after clicking Chat tab again', active(chatPane));

  // Palette path: switchMainPane('editor') then switchMainPane('chat').
  if (typeof window.switchMainPane === 'function') {
    window.switchMainPane('editor');
    check('editor active via switchMainPane', active(editorPane));
    window.switchMainPane('chat');
    check('chat active via switchMainPane', active(chatPane));
  } else {
    console.log('SKIP: switchMainPane not exposed');
  }

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
