'use strict';
// Regression test: the edit-resend flush must wait for the fork reply's
// rebuild (clear_chat → history), not for the FIRST config that carries the
// new session id.
//
// sessionFork launches background model validation whose hook broadcasts its
// own config for the FORKED session (validateModelAsync →
// pushConfigForAgent). On a warm catalog cache that config beats the reply's
// synchronous response/clear_chat/history writes. Flushing the pending
// resent message from it appends the optimistic bubble only for the reply's
// clear_chat to wipe it: the history replay then rebuilds the truncated
// prefix WITHOUT the resent message, and flush already cleared
// needsFreshHistory, so turn_end never refetches — the edited message
// vanishes even though the resent turn runs server-side.
//
// Loads the real index.html + app.js into jsdom (imports stripped, editor/
// marked/dompurify stubbed) with a controllable WebSocket stub and drives
// both orders:
//   1. the racy order (early validation config → response → clear_chat →
//      history → context → config): no message may be sent before the
//      rebuild, exactly one message after it, addressed to the fork.
//   2. the normal order (response → clear_chat → history → context →
//      config): unchanged behaviour — exactly one message.
// Plus the edit button's mid-turn guard must give feedback (toast), not a
// silent no-op.
//
// Run: node scripts/test_resend_flush_order.js
// APPJS env var selects the app.js copy to test (defaults to the real one).
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
  // jsdom has no layout: innerText is unimplemented. The inline-edit send
  // path reads tw.innerText, so map it to textContent (browser-faithful
  // enough for plain text).
  if (!('innerText' in window.HTMLElement.prototype)) {
    Object.defineProperty(window.HTMLElement.prototype, 'innerText', {
      get() { return this.textContent; },
      set(v) { this.textContent = v; },
      configurable: true,
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
  const recv = (obj) => ws.onmessage({ data: JSON.stringify(obj) });
  const sentOf = (type) => ws.sent.filter((m) => m.type === type);
  const $ = (id) => window.document.getElementById(id);
  const turnActive = () => $('input-area').classList.contains('turn-active');
  const userBubbles = () => [...window.document.querySelectorAll('.message.user')];
  return { window, ws, recv, sentOf, $, turnActive, userBubbles };
}

const configFrame = (sid) => ({
  type: 'config', sessionId: sid, sessionLabel: sid + '-label',
  mode: 'act', thinkingLevel: 'off', reasoningEfforts: [],
  model: 'm1', modelDescription: '', workingDir: '/tmp', globalMode: false,
});

// The conversation both scenarios fork: q2 (histIdx 2) is the edit target.
const SOURCE_HISTORY = [
  { role: 'user', content: 'question one', index: 0, createdAt: '2026-01-01T00:00:00Z' },
  { role: 'assistant', content: 'answer one', index: 1, createdAt: '2026-01-01T00:00:01Z' },
  { role: 'user', content: 'question two', index: 2, createdAt: '2026-01-01T00:01:00Z' },
  { role: 'assistant', content: 'answer two', index: 3, createdAt: '2026-01-01T00:01:01Z' },
];
// The fork keeps messages[:messageIndex+1] = q1 + a1.
const FORK_PREFIX = SOURCE_HISTORY.slice(0, 2);
const EDITED = 'question two EDITED';

/** Drive: load history → edit q2 → send → return the harness mid-reply. */
async function editAndSend(env, label) {
  const { recv, sentOf, $, userBubbles } = env;
  recv(configFrame('A'));
  recv({ type: 'history', sessionId: 'A', historyEpoch: 5, history: SOURCE_HISTORY });
  await waitFor(`${label}: history replayed`, () => userBubbles().length === 2);

  const bubble = userBubbles()[1];
  check(`${label}: edit target stamped histIdx=2`, bubble.dataset.histIdx === '2');
  const editBtn = bubble.querySelector('.edit-btn');
  check(`${label}: edit button attached`, !!editBtn);
  editBtn.click();
  const bar = bubble.querySelector('.inline-edit-bar');
  check(`${label}: inline editor opened`, !!bar && bubble.querySelector('.msg-text').contentEditable === 'true');
  bubble.querySelector('.msg-text').textContent = EDITED;
  bubble.querySelector('.inline-edit-send').click();

  const forks = sentOf('session_fork');
  check(`${label}: session_fork sent with messageIndex 1`,
    forks.length === 1 && forks[0].messageIndex === 1 && forks[0].sessionId === 'A');
}

/** The fork reply sequence; `earlyConfig` prepends the validation push. */
async function deliverForkReply(env, label, earlyConfig) {
  const { recv, sentOf, turnActive } = env;
  if (earlyConfig) {
    // pushConfigForAgent: new session id, NO sessionAction, arrives first.
    recv(configFrame('F'));
    await sleep(50);
    check(`${label}: early validation config does NOT flush the resend`,
      sentOf('message').length === 0 && !turnActive());
  }
  recv({ type: 'response', content: 'Forked new session F.', sessionId: 'F', sessionAction: 'clear_chat' });
  recv({ type: 'clear_chat', sessionId: 'F' });
  recv({ type: 'history', sessionId: 'F', historyEpoch: 1, history: FORK_PREFIX });
  await waitFor(`${label}: fork prefix replayed`,
    () => env.userBubbles().length > 0 && !env.userBubbles().some((b) => (b.textContent || '').includes('question two')));
  recv({ type: 'context', sessionId: 'F', usedTokens: 10, contextLimit: 1000 });
  recv(configFrame('F')); // writeSessionCommandResult's post-history config
  await waitFor(`${label}: resent message flushed`, () => sentOf('message').length >= 1);
  await sleep(100);

  const msgs = sentOf('message');
  check(`${label}: exactly one resent message`, msgs.length === 1);
  check(`${label}: resent message carries the edited text`,
    msgs.length === 1 && msgs[0].content === EDITED);
  check(`${label}: resent message addressed to the forked session`,
    msgs.length === 1 && msgs[0].sessionId === 'F');
  check(`${label}: resent turn marked active`, turnActive());
  check(`${label}: pane re-keyed to the fork`, env.window.eval('activePane().id') === 'F');
  check(`${label}: edited bubble present in the transcript`,
    env.userBubbles().some((b) => (b.textContent || '').includes(EDITED)));

  // A later config for the same session must not double-send.
  recv(configFrame('F'));
  await sleep(50);
  check(`${label}: later config does not re-flush`, sentOf('message').length === 1);
}

async function scenarioEarlyConfig() {
  console.log('— scenario 1: validation config races ahead of the fork reply —');
  const env = boot();
  await editAndSend(env, 'early');
  await deliverForkReply(env, 'early', true);
  // Mid-turn guard feedback: the resent turn is active, the edit button on
  // an already-stamped bubble must toast instead of silently ignoring.
  const stamped = env.userBubbles().find((b) => b.querySelector('.edit-btn'));
  check('mid-turn guard target exists', !!stamped);
  if (stamped) {
    stamped.querySelector('.edit-btn').click();
    const toast = [...env.window.document.querySelectorAll('.toast')]
      .map((t) => t.textContent).join(' ');
    check('mid-turn edit click gives feedback (toast)', toast.includes('busy'));
    check('mid-turn edit click stays closed', !stamped.querySelector('.inline-edit-bar'));
  }
}

async function scenarioNormalOrder() {
  console.log('— scenario 2: normal reply order (no early config) —');
  const env = boot();
  await editAndSend(env, 'normal');
  await deliverForkReply(env, 'normal', false);
}

async function main() {
  await scenarioEarlyConfig();
  await scenarioNormalOrder();
  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
