'use strict';
// Regression test: compaction reshapes the server's message indexes while
// the DOM keeps its old data-hist-idx stamps, and NO history frame follows
// a compact. The client must therefore:
//   1. void its history-epoch claim and mark indexes dirty on `compacting`;
//   2. refuse edit/resend/fork until the indexes are fresh (toast, no frame
//      — a fork at a stale index silently truncates at the WRONG point);
//   3. re-attach at turn_end (bare — the claim was voided) and clear the
//      dirty flag when the rebuilt history re-stamps the bubbles.
//
// Run: node scripts/test_compact_index_refresh.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('/tmp/gogen-jsdom/node_modules/jsdom');
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');

let failures = 0;
const check = (desc, ok) => {
  console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
  if (!ok) failures++;
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function waitFor(desc, cond, timeoutMs = 4000) {
  const t0 = Date.now();
  while (Date.now() - t0 < timeoutMs) {
    if (cond()) return true;
    await sleep(25);
  }
  console.log(`   (timeout waiting for: ${desc})`);
  return false;
}

function boot() {
  const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');
  const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;
  window.matchMedia = window.matchMedia || (() => ({ matches: false, addEventListener() {}, removeEventListener() {} }));
  window.navigator.clipboard = { writeText: async () => {} };
  if (!('innerText' in window.HTMLElement.prototype)) {
    Object.defineProperty(window.HTMLElement.prototype, 'innerText', {
      get() { return this.textContent; }, set(v) { this.textContent = v; }, configurable: true,
    });
  }
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
  window.localStorage.setItem('gogen_notifications', 'off');
  window.addEventListener('error', (e) => {
    console.error('WINDOW ERROR:', (e.error && e.error.stack) || e.message);
    failures++;
  });
  loadAppJs(window);
  const ws = sockets[0];
  if (!ws) throw new Error('app.js did not construct a WebSocket');
  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  return {
    window, ws,
    recv: (obj) => ws.onmessage({ data: JSON.stringify(obj) }),
    sentOf: (type) => ws.sent.filter((m) => m.type === type),
    $: (id) => window.document.getElementById(id),
    turnActive: () => window.document.getElementById('input-area').classList.contains('turn-active'),
    userBubbles: () => [...window.document.querySelectorAll('.message.user')],
    toasts: () => [...window.document.querySelectorAll('.toast')].map((t) => t.textContent).join(' | '),
  };
}

const configFrame = (sid) => ({
  type: 'config', sessionId: sid, sessionLabel: sid + '-label',
  mode: 'act', thinkingLevel: 'off', reasoningEfforts: [],
  model: 'm1', modelDescription: '', workingDir: '/tmp', globalMode: false,
});
const SOURCE_HISTORY = [
  { role: 'user', content: 'question one', index: 0, createdAt: '2026-01-01T00:00:00Z' },
  { role: 'assistant', content: 'answer one', index: 1, createdAt: '2026-01-01T00:00:01Z' },
  { role: 'user', content: 'question two', index: 2, createdAt: '2026-01-01T00:01:00Z' },
  { role: 'assistant', content: 'answer two', index: 3, createdAt: '2026-01-01T00:01:01Z' },
];
// After compaction: middle summarized away — q2 shifts from index 2 → 1.
const COMPACTED_HISTORY = [
  { role: 'user', content: 'question one', index: 0, createdAt: '2026-01-01T00:00:00Z' },
  { role: 'user', content: 'question two', index: 1, createdAt: '2026-01-01T00:01:00Z' },
];

async function main() {
  const env = boot();
  env.recv(configFrame('A'));
  env.recv({ type: 'history', sessionId: 'A', historyEpoch: 5, history: SOURCE_HISTORY });
  await waitFor('history replayed', () => env.userBubbles().length === 2);
  check('epoch applied', env.window.eval('activePane().histEpoch') === 5);
  check('indexes clean before compaction', env.window.eval('activePane().indexesDirty') === false);

  // ── compaction starts: indexes are now stale ──
  env.recv({ type: 'compacting', sessionId: 'A' });
  check('epoch claim voided', env.window.eval('activePane().histEpoch') === undefined);
  check('indexes marked dirty', env.window.eval('activePane().indexesDirty') === true);

  // ── edit/resend/fork must refuse (no frame at a stale index) ──
  const q2 = env.userBubbles()[1];
  q2.querySelector('.resend-btn').click();
  await sleep(20);
  check('no fork frame at a stale index',
    env.sentOf('session_fork').length === 0 && env.sentOf('session_new').length === 0);
  check('refusal explains itself (toast)', env.toasts().includes('indexes are refreshing after compaction'));

  // Same gate on the assistant fork button.
  const forkBtn = env.window.document.querySelector('.message.assistant .fork-btn');
  if (forkBtn) {
    forkBtn.click();
    await sleep(20);
    check('assistant fork also gated', env.sentOf('session_fork').length === 0);
  } else {
    check('assistant fork button present (needed for gate check)', false);
  }

  // ── the turn ends: convergence attach fires BARE (claim was voided) ──
  env.recv({ type: 'thinking', sessionId: 'A' });
  env.recv({ type: 'turn_end', sessionId: 'A' });
  await sleep(30);
  const attaches = env.sentOf('session_attach');
  check('turn_end triggers a convergence attach', attaches.length === 1);
  check('attach carries NO history claim (bare → full snapshot)',
    attaches.length === 1 && attaches[0].knownHistoryEpoch === undefined
    && attaches[0].knownHistoryIndex === undefined);
  check('dirty flag still set until the rebuild lands',
    env.window.eval('activePane().indexesDirty') === true);

  // ── the rebuilt history re-stamps and unlocks editing ──
  env.recv({ type: 'session_state', sessionId: 'A', turnActive: false });
  env.recv({ type: 'history', sessionId: 'A', historyEpoch: 6, history: COMPACTED_HISTORY });
  await waitFor('rebuild replayed', () => env.userBubbles().length === 2);
  check('indexes unlocked after the rebuild', env.window.eval('activePane().indexesDirty') === false);
  check('q2 re-stamped with its NEW index (2 → 1)',
    env.userBubbles()[1].dataset.histIdx === '1');

  env.userBubbles()[1].querySelector('.resend-btn').click();
  await sleep(20);
  check('resend now forks at the FRESH index (messageIndex 0)',
    env.sentOf('session_fork').length === 1 && env.sentOf('session_fork')[0].messageIndex === 0);

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
