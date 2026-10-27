'use strict';
// Regression test: a compact that finishes while its pane is in the
// background must clear the pane's compacting flag. Without the fix, the
// restored state on focus blocks future compacts and mis-handles the next
// normal response (toast + duplicate system message).
//
// Loads the real index.html + app.js into jsdom (imports stripped, editor/
// marked/dompurify stubbed) with a controllable WebSocket stub, then:
//   1. connects, adopts session A via config
//   2. starts a compact on A (indicator shows, compact sent for A)
//   3. creates pane B (A goes background, A.compacting saved as true)
//   4. delivers A's compact response while A is background
//   5. focuses A via its sidebar row
//   6. asserts a second compact is NOT blocked and a normal response
//      renders as a single assistant bubble (no system-message dup)
//
// Run: node scripts/test_compact_bg.js
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

  // Controllable WebSocket stub: app.js's connect() constructs it; the
  // instance is captured so the test can drive onopen/onmessage and read
  // the messages the app sends.
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

  const evalJs = (code) => window.eval(code);
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
  const compactSends = () => ws.sent.filter((m) => m.type === 'compact').length;

  // 1. Connect: drive onopen (readyState must be OPEN for the guards).
  ws.readyState = FakeWS.OPEN;
  ws.onopen();

  // 2. Adopt session A on the initial pane via the config echo.
  recv({
    type: 'config', sessionId: 'A', sessionLabel: 'A-label',
    mode: 'act', thinkingLevel: 'off', reasoningEfforts: [], model: 'm1',
    modelDescription: '', workingDir: '/tmp', globalMode: false,
  });
  check('pane A adopted', evalJs('activePane().id') === 'A');

  // 3. Start a compact on A: request sent for A, indicator visible.
  evalJs('startCompact()');
  check('compact request sent for A', compactSends() === 1 && ws.sent.some((m) => m.type === 'compact' && m.sessionId === 'A'));
  const prog = window.document.getElementById('input-progress');
  check('compacting indicator shown', !!prog && prog.classList.contains('active') && (prog.textContent || '').includes('Compacting'));

  // 4. Create pane B (sidebar New): A goes background.
  evalJs('newSession()');
  check('pane B active (id not yet assigned)', evalJs('activePane().id') === null);
  check('session_new sent', ws.sent.some((m) => m.type === 'session_new'));

  // 5. A's compact response arrives while A is in the background.
  recv({ type: 'response', content: 'History compacted (1 messages remaining).', sessionId: 'A' });

  // 6. Render the session list and click A's sidebar row to focus it.
  recv({
    type: 'sessions',
    sessions: [
      { id: 'A', label: 'A-label', messageCount: 2, active: true, updatedAt: '' },
    ],
  });
  const rows = window.document.querySelectorAll('.session-row');
  const rowA = [...rows].find((r) => r.dataset.sessionId === 'A');
  check('session row for A rendered', !!rowA);
  rowA.querySelector('.session-row-content').click();
  check('A focused after row click', evalJs('activePane().id') === 'A');

  // 7. Compact again: must NOT be blocked by a stale flag (this is the
  //    assertion that fails without the fix).
  evalJs('startCompact()');
  check('second compact not blocked', compactSends() === 2);

  // 7b. Complete the second compact (active pane): the response is the
  //     compact result, so a system message IS expected here.
  recv({ type: 'response', content: 'History compacted (2 messages remaining).', sessionId: 'A' });
  const sysAfterCompact = [...window.document.querySelectorAll('.message.system')]
    .filter((el) => (el.textContent || '').includes('History compacted (2'));
  check('second compact response rendered as system message', sysAfterCompact.length === 1);

  // 8. A normal response must render as ONE assistant bubble and no
  //    system message (the stuck-compacting path duplicated it).
  recv({ type: 'response', content: 'hello reply', sessionId: 'A' });
  const doc = window.document;
  const systems = [...doc.querySelectorAll('.message.system')]
    .filter((el) => (el.textContent || '').includes('hello reply'));
  const assistants = [...doc.querySelectorAll('.message.assistant')]
    .filter((el) => (el.textContent || '').includes('hello reply'));
  check('no duplicated system message', systems.length === 0);
  check('reply rendered as assistant bubble', assistants.length === 1);

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
