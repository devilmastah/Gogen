// Regression test for the sidebar's first paint during a history replay:
// the sessions payload is connection-scoped sidebar metadata whose handler
// never touches the transcript, so it must render IMMEDIATELY even while a
// restored session is still replaying. It used to be buffered in
// replayEventBuffer like a live stream event, leaving the sidebar empty for
// the whole rebuild ("the sidebar loads slow on refresh").
//
// Also pins the other half of the contract: transcript-bearing frames
// (stream/thinking/tool/history/…) STAY buffered during the replay and flush
// after it, so live output cannot interleave mid-rebuild.
//
// Run: node scripts/test_sessionlist_during_replay.js
'use strict';
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
  window.Terminal = class { open() {} write() {} fit() {} onData() {} onResize() { return { dispose() {} }; } dispose() {} reset() {} focus() {} };
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
  window.marked = { use() {}, parse: (t) => String(t == null ? '' : t) };
  window.DOMPurify = { sanitize: (r) => r };

  loadAppJs(window);

  const ws = sockets[0];
  if (!ws) throw new Error('app.js did not construct a WebSocket');
  const recv = (obj) => ws.onmessage({ data: JSON.stringify(obj) });
  const doc = window.document;
  const rows = () => doc.querySelectorAll('#session-list .session-row').length;
  const messages = () => doc.querySelectorAll('#messages .message').length;
  const waitFor = async (pred, ms = 10000) => {
    const until = Date.now() + ms;
    while (!pred() && Date.now() < until) await new Promise((r) => setTimeout(r, 5));
    return pred();
  };

  let failures = 0;
  const check = (desc, ok, detail) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}${!ok && detail ? ' — ' + detail : ''}`);
    if (!ok) failures++;
  };

  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  recv({
    type: 'config', sessionId: 'A', sessionLabel: 'A', mode: 'act', thinkingLevel: 'off',
    reasoningEfforts: [], model: 'm1', modelDescription: '', workingDir: '/tmp', globalMode: false,
  });

  // A restored session big enough that its chunked replay spans many frames.
  const history = [];
  for (let i = 0; i < 240; i++) {
    history.push({ role: i % 2 ? 'assistant' : 'user', content: 'message number ' + i + ' padding padding padding', index: i, createdAt: new Date().toISOString() });
  }
  recv({ type: 'history', history, historyEpoch: 0, sessionId: 'A' });
  // The sidebar payload and a live stream event arrive while the replay runs.
  recv({
    type: 'sessions', sessions: [
      { id: 'A', label: 'A', messageCount: 240, updatedAt: new Date().toISOString(), active: true },
      { id: 'B', label: 'B', messageCount: 10, updatedAt: new Date().toISOString() },
    ],
  });
  recv({ type: 'stream', sessionId: 'A', content: 'late stream event', contentPos: 17 });

  const sidebarShown = await waitFor(() => rows() > 0, 5000);
  const messagesWhenShown = messages();
  check('sidebar renders during the replay (not after it)', sidebarShown && messagesWhenShown < history.length,
    JSON.stringify({ rows: rows(), messagesWhenShown, total: history.length }));
  check('sidebar shows every session from the payload', rows() === 2, 'rows=' + rows());
  check('the buffered stream event did NOT render mid-replay',
    !doc.getElementById('messages').textContent.includes('late stream event'));

  // The replay finishes and the buffered event flushes after it, in order.
  check('replay completed', await waitFor(() => messages() >= history.length, 10000), 'messages=' + messages());
  check('buffered stream event flushed after the replay', await waitFor(
    () => doc.getElementById('messages').textContent.includes('late stream event'), 3000));
  const streamIdx = [...doc.querySelectorAll('#messages .message')]
    .findIndex((el) => el.textContent.includes('late stream event'));
  check('buffered event landed after the replayed transcript',
    streamIdx >= history.length, 'streamIdx=' + streamIdx + ' total=' + history.length);

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => {
  console.error('HARNESS ERROR:', err && err.stack || err);
  process.exit(1);
});
