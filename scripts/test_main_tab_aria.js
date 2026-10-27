'use strict';
// Regression test for the main pinned tab-bar semantics. The tabs are plain
// <button>s, so the active view is exposed to assistive tech via
// aria-current + aria-controls (set in index.html, kept in sync by
// switchMainPane). Loads the real index.html + app.js into jsdom.
// Run: node scripts/test_main_tab_aria.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');

async function main() {
  const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');
  const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;
  const { document } = dom.window;

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
  window.WebSocket = class {
    constructor() { this.readyState = 0; }
    static OPEN = 1;
    send() {} close() {}
  };

  installEditorStubs(window);
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };

  loadAppJs(window);

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };

  const chatTab = document.querySelector('.main-tab[data-pane="chat"]');
  const editorTab = document.querySelector('.main-tab[data-pane="editor"]');
  const tabs = [...document.querySelectorAll('.main-tab')];

  check('every main tab declares aria-controls', tabs.every((t) => t.getAttribute('aria-controls')));
  check('chat tab controls #chat-pane', chatTab.getAttribute('aria-controls') === 'chat-pane');
  check('chat tab is current on load', chatTab.getAttribute('aria-current') === 'true');

  editorTab.click();
  check('editor tab is current after clicking Editor', editorTab.getAttribute('aria-current') === 'true');
  check('chat tab is no longer current', !chatTab.hasAttribute('aria-current'));

  window.switchMainPane('chat');
  check('chat tab is current again via switchMainPane', chatTab.getAttribute('aria-current') === 'true');
  check('editor tab lost aria-current', !editorTab.hasAttribute('aria-current'));

  // Only ever one current tab.
  check('exactly one aria-current tab', tabs.filter((t) => t.getAttribute('aria-current') === 'true').length === 1);

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
