'use strict';
// Regression test: Esc must keep working for QUEUED delete approvals.
// respondDeleteApproval removes the overlay keydown listener when resolving
// the first approval; with more approvals queued it shows the next one
// WITHOUT re-registering the listener, so Esc silently stops working for
// the 2nd+ approval (the document-level Escape handler only preventDefaults
// for decision modals — it does not resolve them).
//
// Run: node scripts/test_delete_approval_esc.js
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
    constructor() { this.readyState = 0; this.sent = []; sockets.push(this); }
    static OPEN = 1;
    send(msg) { this.sent.push(JSON.parse(msg)); }
    close() { this.readyState = 3; }
  }
  window.WebSocket = FakeWS;

  installEditorStubs(window);
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };

  loadAppJs(window);

  const ws = sockets[0];
  if (!ws) throw new Error('no socket constructed');
  const recv = (obj) => ws.onmessage({ data: JSON.stringify(obj) });
  const responses = () => ws.sent.filter((m) => m.type === 'delete_approval_response');

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };

  ws.readyState = FakeWS.OPEN;
  ws.onopen();

  // Two approvals arrive; the second queues behind the first.
  recv({ type: 'delete_approval', approvalId: 'a1', sessionId: 'A', reason: 'r1', paths: ['/tmp/x'] });
  recv({ type: 'delete_approval', approvalId: 'a2', sessionId: 'B', reason: 'r2', paths: ['/tmp/y'] });
  const doc = window.document;
  check('first approval shown', (doc.getElementById('delete-approval-paths').textContent || '').includes('/tmp/x'));

  // Resolve the first via the Deny button.
  doc.getElementById('delete-deny-btn').click();
  check('first approval responded', responses().length === 1 && responses()[0].approvalId === 'a1');
  check('second approval now shown', (doc.getElementById('delete-approval-paths').textContent || '').includes('/tmp/y'));

  // Esc on the overlay must resolve the SECOND approval.
  doc.getElementById('delete-approval-overlay').dispatchEvent(
    new window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true })
  );
  check('Esc resolves queued approval', responses().length === 2 && responses()[1].approvalId === 'a2');

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
