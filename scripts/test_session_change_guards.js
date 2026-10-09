'use strict';
// Regression tests for the session-change state machine guards:
//   1. beginResend refuses to start while another session change is in
//      flight (pendingSessionResponse) — symmetric with resume/delete/fork.
//   2. Deleting the ACTIVE session mid-turn: the reply re-keys the pane
//      BEFORE the cancelled old turn's trailing frames can arrive; those
//      frames are dropped by paneForMessage, so the re-key must release the
//      held suppressTurnEnds unit — otherwise the NEXT turn_end is swallowed
//      and turnActive sticks true (busy chrome, dead edit guard).
//   3. The resend watchdog: a lost session-change reply clears the pending
//      flags with an error toast instead of poisoning every later edit with
//      "Resend already in progress"; a completed resend disarms it.
//
// Run: node scripts/test_session_change_guards.js
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

const RESEND_WATCHDOG_MS = 15000; // RESEND_FLUSH_TIMEOUT_MS in app.js

function boot(opts = {}) {
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

  // Watchdog interception (#3): record setTimeout entries at the watchdog's
  // delay so the test can fire one without waiting 15 real seconds.
  const timers = [];
  if (opts.trackTimers) {
    const origSet = window.setTimeout.bind(window);
    const origClear = window.clearTimeout.bind(window);
    window.setTimeout = (fn, delay, ...args) => {
      const id = origSet(fn, delay, ...args);
      if (delay === RESEND_WATCHDOG_MS) timers.push({ id, delay, fn, live: true });
      return id;
    };
    window.clearTimeout = (id) => {
      for (const t of timers) if (t.id === id) t.live = false;
      return origClear(id);
    };
  }

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
    window, ws, timers,
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
const HISTORY = [
  { role: 'user', content: 'question one', index: 0, createdAt: '2026-01-01T00:00:00Z' },
  { role: 'assistant', content: 'answer one', index: 1, createdAt: '2026-01-01T00:00:01Z' },
];

async function scenarioPendingSessionGuard() {
  console.log('— 1. beginResend guards pendingSessionResponse —');
  const env = boot();
  env.recv(configFrame('A'));
  env.recv({ type: 'history', sessionId: 'A', historyEpoch: 3, history: HISTORY });
  await waitFor('history replayed', () => env.userBubbles().length === 1);

  const bubble = env.userBubbles()[0];
  bubble.querySelector('.edit-btn').click();
  check('inline editor opened', !!bubble.querySelector('.inline-edit-bar'));

  // A typed session command goes in-flight first (sets pendingSessionResponse).
  env.$('message-input').value = '/new';
  env.$('send-btn').click();
  check('typed /new sent', env.sentOf('message').some((m) => m.content === '/new'));

  // Now the inline edit's send must refuse — no fork/new frame, clear toast.
  bubble.querySelector('.inline-edit-send').click();
  await sleep(30);
  check('no session_fork/session_new while a session change is in flight',
    env.sentOf('session_fork').length === 0 && env.sentOf('session_new').length === 0);
  check('refusal is not silent (toast)', env.toasts().includes('Session change already in progress'));
}

async function scenarioDeleteReleasesSuppress() {
  console.log('— 2. delete-of-active mid-turn releases the stranded suppress unit —');
  const env = boot();
  env.recv(configFrame('A'));
  env.recv({ type: 'history', sessionId: 'A', historyEpoch: 3, history: HISTORY });
  await waitFor('history replayed', () => env.userBubbles().length === 1);
  env.recv({ type: 'thinking', sessionId: 'A' });
  check('turn active before delete', env.turnActive());

  // Sidebar delete of the ACTIVE session: cancelActiveTurn() holds a
  // suppress unit, then session_delete re-keys the pane on the reply.
  // (The confirm modal's openModal is the harness's no-op stub, so the
  // overlay never gains its `active` class — the button wiring in
  // openDialog is real, and the frames below prove the flow ran.)
  windowDeleteSession(env, 'A');
  await sleep(30);
  const confirmBtn = env.window.document.getElementById('session-delete-confirm-btn');
  check('delete confirm button wired', !!confirmBtn);
  if (confirmBtn) confirmBtn.click();
  await waitFor('cancel + session_delete sent',
    () => env.sentOf('cancel').length === 1 && env.sentOf('session_delete').length === 1);

  // The reply re-keys FIRST; the old turn's trailing frames arrive after
  // and are dropped — the held unit must already be gone.
  env.recv({ type: 'response', content: 'Deleted session A (was current — started new session B).',
    sessionId: 'B', sessionAction: 'clear_chat' });
  env.recv({ type: 'cancelled', content: 'Cancelled.', sessionId: 'A' });
  env.recv({ type: 'turn_end', sessionId: 'A' });
  env.recv({ type: 'clear_chat', sessionId: 'B' });
  env.recv({ type: 'history', sessionId: 'B', historyEpoch: 1, history: [] });
  env.recv({ type: 'context', sessionId: 'B', usedTokens: 5, contextLimit: 1000 });
  env.recv(configFrame('B'));
  await sleep(30);
  check('pane re-keyed to the fresh session', env.window.eval('activePane().id') === 'B');

  // The NEXT turn must end normally: without the re-key release its turn_end
  // would be swallowed (suppress=1) and turnActive would stick true.
  env.$('message-input').value = 'post-delete turn';
  env.$('send-btn').click();
  await waitFor('message sent', () => env.sentOf('message').length > 0);
  env.recv({ type: 'user_acked', content: '0', sessionId: 'B' });
  env.recv({ type: 'thinking', sessionId: 'B' });
  check('turn active during turn', env.turnActive());
  env.recv({ type: 'turn_end', sessionId: 'B' });
  check('next turn_end processed (turnActive not stuck)', !env.turnActive());

  const stamped = env.userBubbles().find((b) => b.querySelector('.edit-btn'));
  check('edit button usable after the turn', (() => {
    if (!stamped) return false;
    stamped.querySelector('.edit-btn').click();
    return !!stamped.querySelector('.inline-edit-bar');
  })());
}

function windowDeleteSession(env, id) {
  // deleteSession lives in components/sessions.js — eval'd as a classic
  // script, so the declaration is a window property.
  if (typeof env.window.deleteSession !== 'function') {
    throw new Error('window.deleteSession not exposed by the harness');
  }
  env.window.deleteSession(id, 'A');
}

async function scenarioWatchdog() {
  console.log('— 3. resend watchdog (lost reply) —');
  const env = boot({ trackTimers: true });
  env.recv(configFrame('A'));
  env.recv({ type: 'history', sessionId: 'A', historyEpoch: 3, history: HISTORY });
  await waitFor('history replayed', () => env.userBubbles().length === 1);

  // Edit → fork sent → NO reply ever arrives.
  let bubble = env.userBubbles()[0];
  bubble.querySelector('.edit-btn').click();
  bubble.querySelector('.msg-text').textContent = 'edited text';
  bubble.querySelector('.inline-edit-send').click();
  check('session_new sent', env.sentOf('session_new').length === 1);
  const armed = env.timers.filter((t) => t.live && t.delay === RESEND_WATCHDOG_MS);
  check('watchdog armed for the pending resend', armed.length === 1);

  // Fire it as if 15s passed with no reply.
  if (armed[0]) armed[0].fn();
  await sleep(20);
  check('watchdog reports the failure (toast)', env.toasts().includes('Resend failed'));
  check('watchdog disarms itself', env.timers.filter((t) => t.live).length === 0);

  // The poison must be gone: a second edit+send goes through.
  bubble = env.userBubbles()[0];
  if (bubble && bubble.querySelector('.edit-btn')) {
    bubble.querySelector('.edit-btn').click();
    const bar = bubble.querySelector('.inline-edit-bar');
    check('edit reopens after the watchdog', !!bar);
    if (bar) {
      bubble.querySelector('.msg-text').textContent = 'retry text';
      bubble.querySelector('.inline-edit-send').click();
      await sleep(20);
      check('retry sends (flags cleared by the watchdog)',
        env.sentOf('session_new').length + env.sentOf('session_fork').length === 2);
    }
  } else {
    check('edit reopens after the watchdog (no edit button)', false);
  }

  // Normal path disarms: complete a resend and confirm no live watchdog.
  env.recv({ type: 'response', content: 'New session C.', sessionId: 'C', sessionAction: 'clear_chat' });
  env.recv({ type: 'clear_chat', sessionId: 'C' });
  env.recv({ type: 'history', sessionId: 'C', historyEpoch: 1, history: HISTORY });
  env.recv(configFrame('C'));
  await waitFor('flush', () => env.sentOf('message').some((m) => m.sessionId === 'C'));
  check('completed resend leaves no live watchdog',
    env.timers.filter((t) => t.live && t.delay === RESEND_WATCHDOG_MS).length === 0);
}

async function scenarioCancelAckDrain() {
  console.log('— 4. cancel_ack drains an idle cancel (next turn_end not swallowed) —');
  const env = boot();
  env.recv(configFrame('A'));
  env.recv({ type: 'history', sessionId: 'A', historyEpoch: 3, history: HISTORY });
  await waitFor('history replayed', () => env.userBubbles().length === 1);

  // Client believes a turn runs; the server answers cancel with cancel_ack
  // (nothing in flight) instead of the cancelled/turn_end pair.
  env.recv({ type: 'thinking', sessionId: 'A' });
  check('turn active before cancel', env.turnActive());
  env.$('cancel-btn').click();
  check('cancel frame sent', env.sentOf('cancel').length === 1);
  check('local cancel cleared the busy flag', !env.turnActive());
  env.recv({ type: 'cancel_ack' });
  await sleep(20);

  // WITHOUT the handler the unit stays held: this turn_end would be
  // swallowed and turnActive would stick true (dead edit guard).
  env.recv({ type: 'thinking', sessionId: 'A' });
  check('next turn runs', env.turnActive());
  env.recv({ type: 'turn_end', sessionId: 'A' });
  check('next turn_end processed (unit drained by cancel_ack)', !env.turnActive());
  const stamped = env.userBubbles().find((b) => b.querySelector('.edit-btn'));
  check('edit opens after the drained cancel', (() => {
    if (!stamped) return false;
    stamped.querySelector('.edit-btn').click();
    return !!stamped.querySelector('.inline-edit-bar');
  })());
}

async function main() {
  await scenarioPendingSessionGuard();
  await scenarioDeleteReleasesSuppress();
  await scenarioWatchdog();
  await scenarioCancelAckDrain();
  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
