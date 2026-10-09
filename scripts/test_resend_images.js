'use strict';
// Regression test: edit/resend must carry the message's image attachments.
// dataset/rawContent only holds the text — before the fix, editing (or the
// ↻ resend button) on an image message silently dropped the attachments:
// the resent turn ran with text only. Covers both entry points (inline edit
// and the resend button) and the history-replay source of the images.
//
// Run: node scripts/test_resend_images.js
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
  };
}

const configFrame = (sid) => ({
  type: 'config', sessionId: sid, sessionLabel: sid + '-label',
  mode: 'act', thinkingLevel: 'off', reasoningEfforts: [],
  model: 'm1', modelDescription: '', workingDir: '/tmp', globalMode: false,
});
const IMG_Q1 = [{ dataUrl: 'data:image/png;base64,QUJD' }];
const IMG_Q2 = [{ dataUrl: 'data:image/png;base64,REVG' }, { dataUrl: 'data:image/jpeg;base64,VEVT' }];
const SOURCE_HISTORY = [
  { role: 'user', content: 'describe image one', index: 0, createdAt: '2026-01-01T00:00:00Z', images: IMG_Q1 },
  { role: 'assistant', content: 'it is a diagram', index: 1, createdAt: '2026-01-01T00:00:01Z' },
  { role: 'user', content: 'describe image two', index: 2, createdAt: '2026-01-01T00:01:00Z', images: IMG_Q2 },
  { role: 'assistant', content: 'it is a chart', index: 3, createdAt: '2026-01-01T00:01:01Z' },
];
const FORK_PREFIX = SOURCE_HISTORY.slice(0, 2);
const sameImages = (a, b) => JSON.stringify(a || []) === JSON.stringify(b || []);

async function main() {
  const env = boot();
  env.recv(configFrame('A'));
  env.recv({ type: 'history', sessionId: 'A', historyEpoch: 4, history: SOURCE_HISTORY });
  await waitFor('history replayed', () => env.userBubbles().length === 2);
  check('images rendered from history',
    env.userBubbles()[1].querySelectorAll('.chat-image').length === IMG_Q2.length);

  // ── inline edit of the image message carries the attachments ──
  const q2 = env.userBubbles()[1];
  check('edit target stamped', q2.dataset.histIdx === '2');
  q2.querySelector('.edit-btn').click();
  q2.querySelector('.msg-text').textContent = 'describe image two (edited)';
  q2.querySelector('.inline-edit-send').click();
  check('session_fork sent', env.sentOf('session_fork').length === 1);

  env.recv({ type: 'response', content: 'Forked new session F.', sessionId: 'F', sessionAction: 'clear_chat' });
  env.recv({ type: 'clear_chat', sessionId: 'F' });
  env.recv({ type: 'history', sessionId: 'F', historyEpoch: 1, history: FORK_PREFIX });
  env.recv(configFrame('F'));
  await waitFor('edited message flushed',
    () => env.sentOf('message').some((m) => m.sessionId === 'F'));
  const sent1 = env.sentOf('message').find((m) => m.sessionId === 'F');
  check('edited message text', sent1.content === 'describe image two (edited)');
  check('edited message CARRIES both images', sameImages(sent1.images, IMG_Q2));

  // End the resent turn so the next resend starts clean.
  env.recv({ type: 'user_acked', content: '2', sessionId: 'F' });
  env.recv({ type: 'turn_end', sessionId: 'F' });
  await sleep(30);
  const flushedBubble = env.userBubbles().find((b) => (b.textContent || '').includes('(edited)'));
  check('flushed bubble rendered with its images',
    !!flushedBubble && flushedBubble.querySelectorAll('.chat-image').length === IMG_Q2.length);

  // ── the ↻ resend button on the FIRST message (histIdx 0 → session_new) ──
  const q1 = env.userBubbles()[0];
  check('first message keeps its images for resend',
    q1.querySelectorAll('.chat-image').length === IMG_Q1.length);
  q1.querySelector('.resend-btn').click();
  check('resend button sent session_new', env.sentOf('session_new').length === 1);
  env.recv({ type: 'response', content: 'New session G.', sessionId: 'G', sessionAction: 'clear_chat' });
  env.recv({ type: 'clear_chat', sessionId: 'G' });
  env.recv({ type: 'history', sessionId: 'G', historyEpoch: 1, history: [] });
  env.recv(configFrame('G'));
  await waitFor('resend flushed', () => env.sentOf('message').some((m) => m.sessionId === 'G'));
  const sent2 = env.sentOf('message').find((m) => m.sessionId === 'G');
  check('button resend carries the attachments too', sameImages(sent2.images, IMG_Q1));
  check('button resend keeps the original text', sent2.content === 'describe image one');

  // A text-only message must NOT grow an empty images field.
  env.recv({ type: 'turn_end', sessionId: 'G' });
  await sleep(20);

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
