'use strict';
// Regression test for the mobile chat-sidebar drawer: on phones the sidebar
// is an overlay drawer, and any in-sidebar action that switches the chat —
// selecting a saved session (openSessionPane), focusing an open pane
// (focusPane) or starting a new one (newSession) — must close it. The tap
// lands INSIDE #sidebar, so the outside-click closer never fires; without
// this the drawer stays over the transcript (and focusPane's inputArea.focus
// raises the keyboard under it).
//
// window.innerWidth is redefined (writable) to flip the <=768px breakpoint,
// since app.js reads it live inside closeMobileSidebar.
// Run: node scripts/test_sidebar_drawer.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');

async function main() {
  const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');
  const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;
  const { document } = window;

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

  const sockets = [];
  class FakeWS {
    constructor() { this.readyState = 0; this.sent = []; sockets.push(this); }
    static OPEN = 1;
    static CONNECTING = 0;
    send(msg) { this.sent.push(JSON.parse(msg)); }
    close() { this.readyState = 3; }
  }
  window.WebSocket = FakeWS;

  installEditorStubs(window);
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };

  loadAppJs(window);

  // innerWidth must be flippable: jsdom exposes it read-only by default.
  const setWidth = (px) => Object.defineProperty(window, 'innerWidth', { value: px, configurable: true, writable: true });

  const sidebar = document.getElementById('sidebar');
  const ws = sockets[0];
  ws.readyState = FakeWS.OPEN;

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };

  // ── Narrow viewport: focusPane dismisses an open drawer ──
  setWidth(375);
  sidebar.classList.add('open');
  window.focusPane('does-not-exist');
  check('narrow: focusPane closes the drawer', !sidebar.classList.contains('open'));

  // ── Narrow viewport: openSessionPane dismisses an open drawer ──
  sidebar.classList.add('open');
  window.openSessionPane('sess-1');
  check('narrow: openSessionPane closes the drawer', !sidebar.classList.contains('open'));

  // ── narrow: newSession dismisses an open drawer ──
  sidebar.classList.add('open');
  window.newSession();
  check('narrow: newSession closes the drawer', !sidebar.classList.contains('open'));

  // ── Wide viewport: the drawer state is untouched ──
  setWidth(1024);
  sidebar.classList.add('open');
  window.focusPane('does-not-exist');
  check('wide: focusPane leaves the sidebar state alone', sidebar.classList.contains('open'));
  sidebar.classList.remove('open');

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
