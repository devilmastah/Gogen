'use strict';
// Regression test for the per-pane transcript cache + conditional attach:
// switching panes must NOT re-render a settled pane's transcript from a
// fresh server snapshot, and the re-attach must tell the server what the
// pane already has (knownHistoryEpoch / knownHistoryIndex) so the server
// can skip the history snapshot entirely. A rewind-only history frame must
// merge the in-flight partial ONTO the cached transcript without clearing
// it, and a genuinely new snapshot must still rebuild.
//
// Loads the real index.html + app.js into jsdom (imports stripped, editor/
// marked/dompurify stubbed) with a controllable WebSocket stub.
// Run: node scripts/test_pane_cache.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

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
  // app.js imports escapeHtml from editor.js; the shared stub list does not
  // cover it (no other harness replays user text), so stub it here.
  window.escapeHtml = window.escapeHtml || ((s) => String(s == null ? '' : s));
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
  const doc = window.document;
  const messagesDiv = doc.getElementById('messages');

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };
  const lastAttach = (sid) => {
    const list = ws.sent.filter((m) => m.type === 'session_attach' && m.sessionId === sid);
    return list[list.length - 1];
  };

  // 1. Connect and adopt session A on the initial pane.
  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  recv({
    type: 'config', sessionId: 'A', sessionLabel: 'A-label',
    mode: 'act', thinkingLevel: 'off', reasoningEfforts: [], model: 'm1',
    modelDescription: '', workingDir: '/tmp', globalMode: false,
  });
  check('pane A adopted', evalJs('activePane().id') === 'A');

  // 2. Deliver A's transcript. historyEpoch is OMITTED (epoch 0 — the
  //    never-reshaped case): the pane must still record a numeric epoch so
  //    the conditional attach / stale-skip comparisons work for the most
  //    common kind of session.
  recv({
    type: 'history', sessionId: 'A',
    history: [
      { role: 'user', content: 'question one', index: 0, createdAt: '2024-01-01T00:00:00Z' },
      { role: 'assistant', content: 'answer one', index: 1, createdAt: '2024-01-01T00:00:01Z' },
    ],
  });
  await sleep(200);
  const userA = messagesDiv.querySelector('.message.user');
  const assistantA = messagesDiv.querySelector('.message.assistant');
  check('A transcript rendered', !!userA && !!assistantA);
  check('A pane histEpoch recorded (0 = never reshaped)', evalJs('activePane().histEpoch') === 0);
  check('A pane newest rendered index = 1', evalJs('newestDomHistIdx()') === 1);

  // 3. Open session B as a new pane: A goes background and — being settled —
  //    its transcript must be cached, not wiped.
  window.openSessionPane('B');
  check('pane B active', evalJs('activePane().id') === 'B');
  check('A transcript cached', evalJs('findPaneBySession("A").domCache !== null'));
  check('B attach sent without known fields (fresh pane)', !lastAttach('B').knownHistoryEpoch && !lastAttach('B').knownHistoryIndex);
  const emptyState = messagesDiv.querySelector('.empty-state');
  check('B pane shows empty state', !!emptyState);

  // 4. Focus A again: the cached transcript must be restored AS-IS (same
  //    DOM nodes, no replay) and the attach must carry the pane's
  //    fingerprint so the server can skip the snapshot.
  window.openSessionPane('A');
  check('pane A active again', evalJs('activePane().id') === 'A');
  check('A transcript restored without re-render',
    messagesDiv.querySelector('.message.user') === userA
    && messagesDiv.querySelector('.message.assistant') === assistantA);
  check('TOC rail rebuilt for the restored transcript',
    doc.getElementById('toc-rail').querySelectorAll('.toc-dot').length === 1);
  const attachA = lastAttach('A');
  check('A re-attach carries knownHistoryEpoch=0', attachA && attachA.knownHistoryEpoch === 0);
  check('A re-attach carries knownHistoryIndex=1', attachA && attachA.knownHistoryIndex === 1);

  // 5. Conditional attach, rewind-only frame: the server proved the
  //    transcript is current and ships only the in-flight partial. The
  //    cached transcript must survive and the partial must render after it.
  recv({
    type: 'history', rewindOnly: true, sessionId: 'A', historyEpoch: 0,
    rewind: { content: 'streaming partial', contentPos: 17 },
  });
  await sleep(80);
  check('rewind-only keeps the cached transcript',
    messagesDiv.querySelector('.message.user') === userA
    && messagesDiv.querySelector('.message.assistant') === assistantA);
  const partial = [...messagesDiv.querySelectorAll('.message.assistant')]
    .find((el) => (el.textContent || '').includes('streaming partial'));
  check('rewind partial rendered after the transcript', !!partial);

  // 6. A genuinely new snapshot (epoch reshaped + one more message) must
  //    rebuild the transcript from scratch — the cache is discarded.
  recv({
    type: 'history', sessionId: 'A', historyEpoch: 5,
    history: [
      { role: 'user', content: 'question one', index: 0, createdAt: '2024-01-01T00:00:00Z' },
      { role: 'assistant', content: 'answer one', index: 1, createdAt: '2024-01-01T00:00:01Z' },
      { role: 'user', content: 'question two', index: 2, createdAt: '2024-01-01T00:00:02Z' },
    ],
  });
  await sleep(200);
  check('new snapshot rebuilt the transcript',
    messagesDiv.querySelector('.message.user') !== userA
    && messagesDiv.querySelectorAll('.message.user').length === 2);
  check('rebuilt pane epoch updated', evalJs('activePane().histEpoch') === 5);

  // 7. Switching to B and back again now restores B's cache (its empty
  //    state) without a re-render, and the REBUILT transcript is cached on
  //    the switch away (so the next A focus can re-verify it).
  window.openSessionPane('B');
  check('B cache restored (empty state node identity)',
    messagesDiv.querySelector('.empty-state') === emptyState);
  check('A rebuilt transcript cached on switch away',
    evalJs('findPaneBySession("A").domCache !== null'));
  window.openSessionPane('A');
  check('A rebuilt transcript restored without re-render',
    messagesDiv.querySelectorAll('.message.user').length === 2);

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
