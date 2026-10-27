'use strict';
// Regression test for the subagent failure notice: when a background
// subagent FAILS, its failure reason (the subagent_finished event's
// subagentSummary) is surfaced as a TOAST — never as a chat line. The
// completion notice already reaches the parent MODEL as a persisted user
// message (the delivery service) and the nested sidebar row shows the
// failed state, so a rendered system bubble would duplicate the failure in
// the transcript (and it was ephemeral anyway — any history rebuild removed
// it). A successful finish toasts nothing, and a failure whose parent
// session has no open pane still toasts (the event is sidebar-global)
// while touching no transcript.
//
// Loads the real index.html + app.js into jsdom (imports stripped, editor/
// marked/dompurify stubbed) with a controllable WebSocket stub, then:
//   1. adopts a parent session A as the active pane
//   2. subagent_finished (failed) for child C of A → one error toast with
//      the label + reason, and NO transcript line
//   3. subagent_finished (success) for child S of A → no new toast
//   4. subagent_finished (failed) for child X of parent B (no pane) → a
//      toast, still no transcript line
//
// Run: node scripts/test_subagent_failure_notice.js
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
  const toasts = () => [...window.document.querySelectorAll('#toast-host .toast')];
  const chatMessages = () => [...window.document.querySelectorAll('#messages .message')];

  // 1. Connect and adopt the parent session A as the active pane.
  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  recv({
    type: 'config', sessionId: 'A', sessionLabel: 'parent-session',
    mode: 'act', thinkingLevel: 'off', reasoningEfforts: [], model: 'm1',
    modelDescription: '', workingDir: '/tmp', globalMode: false,
  });
  if (window.eval('activePane().id') !== 'A') throw new Error('parent pane A not adopted');

  // 2. A background child of A FAILS: the event carries the reason in
  //    subagentSummary. The failure must surface as an error toast with the
  //    label and the reason — and append NOTHING to the transcript (the
  //    delivered completion notice is the persisted copy).
  recv({
    type: 'subagent_started', subagentId: 'C', subagentParent: 'A',
    subagentLabel: 'subagent: fix parser', subagentJob: 'fix parser',
  });
  recv({
    type: 'subagent_finished', subagentId: 'C', subagentParent: 'A',
    subagentLabel: 'subagent: fix parser', subagentSuccess: false,
    subagentSummary: 'provider exploded',
  });
  let shown = toasts();
  check('failed child shows one error toast', shown.length === 1 && shown[0].dataset.kind === 'error');
  const text = shown[0] ? shown[0].textContent : '';
  check('toast names the child and the failure reason',
    text.includes('subagent: fix parser') && text.includes('failed') && text.includes('provider exploded'));
  check('failure appends no chat line', chatMessages().length === 0);

  // 3. A child of A that SUCCEEDS toasts nothing (and appends nothing).
  recv({
    type: 'subagent_started', subagentId: 'S', subagentParent: 'A',
    subagentLabel: 'subagent: good job', subagentJob: 'good job',
  });
  recv({
    type: 'subagent_finished', subagentId: 'S', subagentParent: 'A',
    subagentLabel: 'subagent: good job', subagentSuccess: true,
    subagentSummary: 'all done',
  });
  check('successful child shows no toast', toasts().length === 1);
  check('successful child appends no chat line', chatMessages().length === 0);

  // 4. A failure whose parent session has NO open pane still toasts (the
  //    event is sidebar-global) and touches no transcript.
  recv({
    type: 'subagent_started', subagentId: 'X', subagentParent: 'B',
    subagentLabel: 'subagent: other parent', subagentJob: 'other job',
  });
  recv({
    type: 'subagent_finished', subagentId: 'X', subagentParent: 'B',
    subagentLabel: 'subagent: other parent', subagentSuccess: false,
    subagentSummary: 'boom',
  });
  shown = toasts();
  check('pane-less parent failure still toasts',
    shown.length === 2 && (shown[1].dataset.message || '').includes('boom'));
  check('pane-less parent failure appends no chat line', chatMessages().length === 0);

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
