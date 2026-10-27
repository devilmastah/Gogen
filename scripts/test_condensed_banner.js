'use strict';
// Regression test for the last-resort condensation banner (Phase 0e): when a
// single message cannot fit the context window and is condensed in place,
// the web UI shows a dismissible banner in the chat ("message was N tokens
// vs M window, condensed to continue, original archived").
//
// Loads the real index.html + app.js into jsdom (imports stripped, editor/
// marked/dompurify stubbed) with a controllable WebSocket stub, then:
//   1. adopts a session as the active pane
//   2. receives a "condensed" event -> banner appears with the note
//   3. clicking the banner dismisses it
//   4. a second "condensed" event re-shows the banner
//
// Run: node scripts/test_condensed_banner.js
// APPJS env var selects the app.js copy to test (defaults to the real one).
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
const APPJS = process.env.APPJS || path.join(ROOT, 'internal/server/web/app.js');

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

  const sockets = [];
  class FakeWS {
    constructor() {
      this.readyState = 0;
      this.sent = [];
      sockets.push(this);
    }
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

  const ws = sockets[0];
  if (!ws) throw new Error('app.js did not construct a WebSocket');
  window.addEventListener('error', (e) => { console.error('WINDOW ERROR:', e.error && e.error.stack || e.message); });
  const recv = (obj) => {
    try {
      ws.onmessage({ data: JSON.stringify(obj) });
    } catch (err) {
      console.error('RECV ERROR:', err && err.stack || err);
      throw err;
    }
  };

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };
  const banner = () => window.document.querySelector('#condensed-banner');

  // 1. Connect and adopt the session as the active pane.
  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  recv({
    type: 'config', sessionId: 'A', sessionLabel: 'session',
    mode: 'act', thinkingLevel: 'off', reasoningEfforts: [], model: 'm1',
    modelDescription: '', workingDir: '/tmp', globalMode: false,
  });
  if (window.eval('activePane().id') !== 'A') throw new Error('pane A not adopted');

  // 2. A condensed event shows the banner with the note.
  const note = 'A message was ~3000 tokens vs a 2000-token context window; it was condensed to continue. The original was archived.';
  recv({ type: 'condensed', content: note });
  let b = banner();
  check('condensed event shows the banner', !!b && !b.hidden);
  check('banner carries the note', !!b && b.querySelector('#condensed-banner-text').textContent.includes('3000 tokens vs a 2000-token') && b.textContent.includes('archived'));

  // 3. Clicking the dismiss button hides the banner.
  b.querySelector('#condensed-banner-dismiss').click();
  b = banner();
  check('clicking dismiss hides the banner', !!b && b.hidden);

  // 4. A second condensed event re-shows the banner.
  recv({ type: 'condensed', content: 'condensed again' });
  b = banner();
  check('a second condensed event re-shows the banner', !!b && !b.hidden && b.textContent.includes('condensed again'));

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
