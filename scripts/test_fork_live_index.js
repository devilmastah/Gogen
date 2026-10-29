'use strict';
// Regression test: every fork button must fork from ITS OWN message.
//
// A bubble rendered from history carries the server's raw index
// (entry.index → data-hist-idx), and its button read it correctly. A bubble
// rendered LIVE by the stream did not: its button hardcoded -1, which the
// server reads as "fork from the LAST assistant message that produced
// output" (ForkMessages: "last"). With two live replies in one page session,
// clicking fork on the FIRST one silently produced a session containing the
// SECOND one.
//
// The server stamps a live bubble with hist_index as its round is appended
// (wsStreamSink.OnMessageAppended → handleHistIndex), so the fix is to read
// that index at CLICK time, with -1 only as the fallback for a bubble that
// has none. Never dataset.msgIdx: that is a page-local bubble counter, not a
// server index.
//
// Run: node scripts/test_fork_live_index.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
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
    toasts: () => [...window.document.querySelectorAll('.toast')].map((t) => t.textContent).join(' | '),
    bubbles: () => [...window.document.querySelectorAll('.message.assistant')],
    bubbleWith: (text) => [...window.document.querySelectorAll('.message.assistant')]
      .find((el) => (el.textContent || '').includes(text)),
  };
}

const configFrame = (sid) => ({
  type: 'config', sessionId: sid, sessionLabel: sid + '-label',
  mode: 'act', thinkingLevel: 'off', reasoningEfforts: [],
  model: 'm1', modelDescription: '', workingDir: '/tmp', globalMode: false,
});

// Two exchanges: the assistant entries carry the server's raw indexes 1 and 3.
const HISTORY = [
  { role: 'user', content: 'question one', index: 0, createdAt: '2026-01-01T00:00:00Z' },
  { role: 'assistant', content: 'answer one', index: 1, createdAt: '2026-01-01T00:00:01Z' },
  { role: 'user', content: 'question two', index: 2, createdAt: '2026-01-01T00:01:00Z' },
  { role: 'assistant', content: 'answer two', index: 3, createdAt: '2026-01-01T00:01:01Z' },
];

// One live round exactly as the server emits it: thinking → tokens →
// hist_index for the appended assistant message → stream_end (endStream adds
// the fork button) → turn_end.
async function streamLiveTurn(env, sid, text, histIndex) {
  env.recv({ type: 'thinking', sessionId: sid });
  env.recv({ type: 'stream', sessionId: sid, content: text });
  env.recv({ type: 'hist_index', sessionId: sid, content: String(histIndex) });
  env.recv({ type: 'stream_end', sessionId: sid });
  env.recv({ type: 'turn_end', sessionId: sid });
  await waitFor(`live bubble "${text}"`, () => !!env.bubbleWith(text));
}

function forkIndexOf(env, bubble) {
  const btn = bubble.querySelector('.fork-btn');
  if (!btn) return { clicked: false, index: undefined };
  btn.click();
  const sent = env.sentOf('session_fork');
  return { clicked: true, index: sent.length ? sent[sent.length - 1].messageIndex : undefined };
}

async function scenarioHistoryBubble() {
  console.log('\n-- history-replayed bubble --');
  const env = boot();
  env.recv(configFrame('A'));
  env.recv({ type: 'history', sessionId: 'A', historyEpoch: 5, history: HISTORY });
  await waitFor('history replayed', () => env.bubbles().length === 2);
  const first = env.bubbleWith('answer one');
  check('history bubble carries its server index', first && first.dataset.histIdx === '1');
  const got = forkIndexOf(env, first);
  check('fork sends the clicked bubble\'s index (1)', got.index === 1);
}

async function scenarioSingleLiveBubble() {
  console.log('\n-- single live bubble --');
  const env = boot();
  env.recv(configFrame('A'));
  env.recv({ type: 'history', sessionId: 'A', historyEpoch: 5, history: HISTORY });
  await waitFor('history replayed', () => env.bubbles().length === 2);
  await streamLiveTurn(env, 'A', 'live one', 4);

  const live = env.bubbleWith('live one');
  check('live bubble was stamped with hist_index', live && live.dataset.histIdx === '4');
  const got = forkIndexOf(env, live);
  check('fork from the live bubble sends its own index (4), not -1', got.index === 4);
}

async function scenarioTwoLiveBubbles() {
  console.log('\n-- two live bubbles: fork the OLDER one --');
  const env = boot();
  env.recv(configFrame('A'));
  env.recv({ type: 'history', sessionId: 'A', historyEpoch: 5, history: HISTORY });
  await waitFor('history replayed', () => env.bubbles().length === 2);
  await streamLiveTurn(env, 'A', 'live one', 4);
  await streamLiveTurn(env, 'A', 'live two', 6);

  const older = env.bubbleWith('live one');
  const newer = env.bubbleWith('live two');
  check('both live bubbles exist', !!older && !!newer);
  check('older live bubble keeps its own index (4)', older && older.dataset.histIdx === '4');
  check('newer live bubble carries the newest index (6)', newer && newer.dataset.histIdx === '6');

  const got = forkIndexOf(env, older);
  check('fork from the OLDER live bubble sends 4 (not the newest 6, not -1)', got.index === 4);
  check('no refusal toast (the fork was accepted)', env.toasts() === '');
}

async function main() {
  await scenarioHistoryBubble();
  await scenarioSingleLiveBubble();
  await scenarioTwoLiveBubbles();
  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
