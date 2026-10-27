'use strict';
// Regression test: an unsuccessful stream stop must produce a browser
// notification even when the turn never became active (no thinking/stream
// event arrived — e.g. "no model selected", a busy rejection, or a
// validation error before the turn started). The turn-end notification only
// fires on an active→idle transition, so without the fix those failures
// showed the reason in the chat but never notified — while a successful end
// always announced. Mid-stream errors must keep their single turn-end
// notification (no double-fire at response time + turn_end).
//
// Loads the real index.html + app.js into jsdom (imports stripped, editor/
// marked/dompurify stubbed) with a controllable WebSocket stub and a
// recording Notification stub, then drives:
//   1. pre-stream error response  → notifies immediately, once
//   2. its turn_end               → no second notification
//   3. mid-stream error           → notifies exactly once, at turn_end
//   4. successful turn            → generic turn-end notification unchanged
//   5. busy rejection (no turn_end at all) → still notifies
//
// Run: node scripts/test_error_notify.js
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

  // Recording Notification stub: captures every notification the app
  // creates instead of showing it, so the test can assert exactly when and
  // how many times an unsuccessful stop announces.
  const notifications = [];
  window.Notification = class {
    static permission = 'granted';
    static requestPermission() { return Promise.resolve('granted'); }
    constructor(title, opts) { notifications.push({ title, opts }); }
  };
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

  // Notification preference: 'on' (notify always) so the background-mode
  // focus check never gates the test.
  window.localStorage.setItem('gogen_notifications', 'on');

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
  // Null-safe: a missing notification fails its checks instead of crashing
  // the harness, so a regression reports every affected assertion.
  const last = (n) => notifications[n - 1] || { opts: {} };
  // turnActive is module-scoped (let inside the eval'd script), so observe
  // the input-area class setTurnActive toggles instead of the variable.
  const turnActiveClass = () => window.document.getElementById('input-area').classList.contains('turn-active');

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
  check('no notification before any turn', notifications.length === 0);

  // 3. Pre-stream failure: an error response with NO prior turn-active
  //    event (the model's stream failed before the first chunk, or the
  //    message was rejected before the turn started). The chat must show
  //    the reason AND a browser notification must fire — this is the
  //    assertion that fails without the fix.
  recv({ type: 'response', content: 'Error: no model selected — pick a model in Settings first', sessionId: 'A' });
  check('pre-stream error notifies immediately', notifications.length === 1);
  check('error notification tagged gogen-turn-error', last(1).opts.tag === 'gogen-turn-error');
  check('error notification carries the stop reason', last(1).opts.body === 'Error: no model selected — pick a model in Settings first');
  check('error notification titled GoGen — Error', last(1).title === 'GoGen — Error');
  const sys = [...window.document.querySelectorAll('.message.system')]
    .filter((el) => (el.textContent || '').includes('no model selected'));
  check('chat shows the reason as a system message', sys.length === 1);
  check('turn still inactive after pre-stream error', turnActiveClass() === false);

  // 4. The failed turn's turn_end must NOT double-notify: the turn never
  //    became active, so the turn-end transition is a no-op.
  recv({ type: 'turn_end', sessionId: 'A' });
  check('no second notification at the failed turn\'s turn_end', notifications.length === 1);

  // 5. Mid-stream failure: the turn became active (thinking), the error
  //    arrives while active (no direct notification), and the turn_end
  //    transition fires exactly one error notification — unchanged.
  recv({ type: 'thinking', sessionId: 'A' });
  recv({ type: 'response', content: 'Error: model returned no output (response was truncated mid-reasoning); please try again', sessionId: 'A' });
  check('mid-stream error does not notify at response time', notifications.length === 1);
  check('turn active during mid-stream error', turnActiveClass() === true);
  recv({ type: 'turn_end', sessionId: 'A' });
  check('mid-stream error notifies exactly once, at turn_end', notifications.length === 2);
  check('mid-stream notification carries the stop reason', last(2).opts.body === 'Error: model returned no output (response was truncated mid-reasoning); please try again');

  // 6. Successful turn: generic turn-end notification unchanged.
  recv({ type: 'thinking', sessionId: 'A' });
  recv({ type: 'stream', content: 'hello reply', contentPos: 11, sessionId: 'A' });
  recv({ type: 'stream_end', sessionId: 'A' });
  recv({ type: 'turn_end', sessionId: 'A' });
  check('successful turn notifies once', notifications.length === 3);
  check('success notification is the generic one', last(3).title === 'GoGen'
    && last(3).opts.tag === 'gogen-turn-end'
    && last(3).opts.body === 'Agent finished responding.');

  // 7. Busy rejection: an error response with NO turn_end at all must
  //    still notify (the turn-end path never runs for it).
  recv({ type: 'response', content: 'Error: agent is busy with another client', sessionId: 'A' });
  check('busy rejection notifies without a turn_end', notifications.length === 4);
  check('busy notification carries the reason', last(4).opts.body === 'Error: agent is busy with another client');

  // 8. The direct notification must not leak into a LATER active turn: a
  //    new turn start clears the consumed error, and its own end uses the
  //    generic notification.
  recv({ type: 'thinking', sessionId: 'A' });
  recv({ type: 'stream', content: 'second reply', contentPos: 12, sessionId: 'A' });
  recv({ type: 'stream_end', sessionId: 'A' });
  recv({ type: 'turn_end', sessionId: 'A' });
  check('later successful turn notifies with the generic title', notifications.length === 5 && last(5).title === 'GoGen');

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
