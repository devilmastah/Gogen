'use strict';
// Regression test for the chat reading-width preference (board #97):
// Settings → Appearance "Chat width" toggles .chat-column on #messages and
// persists to localStorage (gogen_chat_width). Loads the REAL index.html +
// app.js into jsdom (imports stripped, editor/marked/dompurify stubbed, no
// server needed). Covers: default is full width, selecting "comfortable"
// applies + persists, selecting "full" reverts, a pre-set preference applies
// on load, and a cross-tab storage event syncs live.
//
// Run: node scripts/test_chat_width.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');

let failures = 0;
const check = (desc, ok) => {
  console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
  if (!ok) failures++;
};

// Build a jsdom window with the app's stubs installed but app.js NOT yet
// evaluated, so a caller can seed localStorage first.
function makeWindow() {
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
  window.WebSocket = class {
    constructor() { this.readyState = 1; }
    static OPEN = 1;
    send() {}
    close() {}
  };
  window.HTMLElement.prototype.scrollIntoView = function () {};
  installEditorStubs(window);
  window.openModal = (overlay) => overlay.classList.add('active');
  window.closeModal = (overlay) => overlay.classList.remove('active');
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };
  return window;
}

// ── Scenario A: default (nothing stored) then live toggling ──
{
  const window = makeWindow();
  loadAppJs(window);
  const { document } = window;
  const messages = document.getElementById('messages');
  const select = document.getElementById('chat-width-select');

  check('chat-width select exists', !!select);
  check('select defaults to full', select && select.value === 'full');
  check('default stores no preference', window.localStorage.getItem('gogen_chat_width') === null);
  check('messages is not a column by default', !messages.classList.contains('chat-column'));

  select.value = 'comfortable';
  select.dispatchEvent(new window.Event('change', { bubbles: true }));
  check('selecting comfortable adds .chat-column', messages.classList.contains('chat-column'));
  check('selecting comfortable persists', window.localStorage.getItem('gogen_chat_width') === 'comfortable');

  select.value = 'full';
  select.dispatchEvent(new window.Event('change', { bubbles: true }));
  check('selecting full removes .chat-column', !messages.classList.contains('chat-column'));
  check('selecting full persists', window.localStorage.getItem('gogen_chat_width') === 'full');

  // Cross-tab sync: a storage event from another tab applies live.
  window.dispatchEvent(new window.StorageEvent('storage', { key: 'gogen_chat_width', newValue: 'comfortable' }));
  check('storage event applies comfortable', messages.classList.contains('chat-column'));
  check('storage event syncs the select', select.value === 'comfortable');
}

// ── Scenario B: a pre-set preference applies on load ──
{
  const window = makeWindow();
  window.localStorage.setItem('gogen_chat_width', 'comfortable');
  loadAppJs(window);
  const { document } = window;
  check('pre-set comfortable applies on load',
    document.getElementById('messages').classList.contains('chat-column'));
  check('pre-set comfortable reflected in the select',
    document.getElementById('chat-width-select').value === 'comfortable');
}

if (failures) {
  console.error('\n' + failures + ' check(s) FAILED');
  process.exit(1);
}
console.log('\nall chat-width checks passed');
// jsdom's visual loop + the app's sockets/timers keep the event loop alive;
// exit explicitly (matches the other harness tests).
process.exit(0);
