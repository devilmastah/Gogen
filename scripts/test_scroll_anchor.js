'use strict';
// Regression test for the per-session scroll anchor: switching away from a
// pane whose transcript must be REBUILT on return (mid-turn switch-away →
// no domCache → history replay) must put the viewport back on the part of
// the transcript the user was reading, instead of the replay's default
// pin-to-bottom. Also covers the guard rails, each observable through
// where the viewport lands after the rebuild:
//   - a valid anchor restores twice (kept on use — turn_end convergence
//     refetches restore again),
//   - a reshaped history (historyEpoch bump: compaction / rollback)
//     invalidates the anchor → bottom,
//   - a session change (resume) delivering the NEW session's history while
//     the pane still holds the OLD id must not apply the old anchor,
//   - closing the pane / the session being removed drops the anchor (a
//     matching-epoch rebuild afterwards stays at the bottom),
//   - a settled pane keeps the exact domCache restore path (raw scrollTop,
//     same DOM nodes — the anchor is only the rebuild fallback).
//
// Loads the real index.html + app.js into jsdom (imports stripped, editor/
// marked/dompurify stubbed) with a controllable WebSocket stub. jsdom has
// no layout, so getBoundingClientRect is overridden with a small fake
// geometry: each history-indexed message is 100px tall, stacked at
// top = index*100 in content coordinates; rects are computed against the
// container's live scrollTop so the anchor math is exercised end to end.
// (Module-scope state like the anchor Map itself is not reachable from the
// harness — eval-scoped lets/consts don't become window properties — hence
// the behavioral assertions.)
// Run: node scripts/test_scroll_anchor.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
const MSG_PX = 100;      // each message is 100px tall
const VIEW_PX = 500;     // container clientHeight
const CONTENT_PX = 1400; // container scrollHeight (max scrollTop = 900)

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
  window.escapeHtml = window.escapeHtml || ((s) => String(s == null ? '' : s));
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };

  loadAppJs(window);

  const evalJs = (code) => window.eval(code);
  const ws = sockets[0];
  if (!ws) throw new Error('app.js did not construct a WebSocket');
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
  const check = (desc, ok, detail) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}${ok || detail === undefined ? '' : ` — ${detail}`}`);
    if (!ok) failures++;
  };

  // ── Fake geometry ──
  // Message i occupies content-y [i*100, (i+1)*100]; the container's top
  // edge is viewport-y 0, so a message's rect top is its content top minus
  // the container's scrollTop. Everything else falls through to jsdom's
  // all-zeros rects (same as the other harnesses).
  const nativeGBCR = window.Element.prototype.getBoundingClientRect;
  const rect = (top) => ({ top, bottom: top, left: 0, right: 0, width: 0, height: MSG_PX });
  window.Element.prototype.getBoundingClientRect = function () {
    if (this === messagesDiv) return rect(0);
    const idx = this.dataset && this.dataset.histIdx;
    if (idx !== undefined && messagesDiv.contains(this)) {
      return rect(parseInt(idx, 10) * MSG_PX - messagesDiv.scrollTop);
    }
    return nativeGBCR.call(this);
  };
  // Container metrics (data properties shadow jsdom's prototype accessors).
  Object.defineProperty(messagesDiv, 'scrollHeight', { value: CONTENT_PX, configurable: true });
  Object.defineProperty(messagesDiv, 'clientHeight', { value: VIEW_PX, configurable: true });

  const history = (n) => {
    const out = [];
    for (let i = 0; i < n; i++) {
      out.push({ role: i % 2 === 0 ? 'user' : 'assistant', content: `msg ${i}`, index: i, createdAt: '2024-01-01T00:00:00Z' });
    }
    return out;
  };
  const bottom = () => CONTENT_PX - VIEW_PX;

  // 1. Connect and adopt session A.
  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  recv({
    type: 'config', sessionId: 'A', sessionLabel: 'A-label',
    mode: 'act', thinkingLevel: 'off', reasoningEfforts: [], model: 'm1',
    modelDescription: '', workingDir: '/tmp', globalMode: false,
  });
  check('pane A adopted', evalJs('activePane().id') === 'A');

  // 2. Deliver A's transcript (epoch 0 — never reshaped). The replay's
  //    default settlement pins to the bottom.
  recv({ type: 'history', sessionId: 'A', history: history(6) });
  await sleep(120);
  check('A transcript rendered', messagesDiv.querySelectorAll('.message').length === 6);
  check('A pinned at the bottom after the replay',
    evalJs('isPinned()') === true && messagesDiv.scrollTop === bottom());

  // 3. Simulate the user reading mid-history: scroll to 300 (message 3's
  //    top crosses the container's top edge) and unpin the way a wheel-up
  //    does.
  messagesDiv.scrollTop = 300;
  evalJs('unpinFromBottom()');
  check('reading position: unpinned at scrollTop 300',
    evalJs('isPinned()') === false && messagesDiv.scrollTop === 300);

  // 4. Start a turn on A (the pane is no longer settled) and switch to B:
  //    the outgoing transcript must be WIPED (no domCache) — the reading
  //    position is captured as an anchor for session A (proven by the
  //    restore in step 5).
  recv({ type: 'waiting', sessionId: 'A' });
  window.openSessionPane('B');
  check('pane B active', evalJs('activePane().id') === 'B');
  check('mid-turn pane A was not cached (wipe path taken)',
    evalJs('findPaneBySession("A").domCache') === null);

  // 5. Switch back to A: no cache → the server ships a fresh snapshot
  //    (same epoch, grown by the background turn's round). The rebuild
  //    must settle on the anchor (message 3 back at the viewport top),
  //    not pin to the bottom. scrollTop is reset first: a real browser
  //    clamps scrollTop to 0 while clearChat() empties the container.
  window.openSessionPane('A');
  messagesDiv.scrollTop = 0;
  recv({ type: 'history', sessionId: 'A', history: history(8) });
  await sleep(120);
  check('rebuilt transcript has the grown history', messagesDiv.querySelectorAll('.message').length === 8);
  check('reading position restored onto the anchor (scrollTop 300, unpinned)',
    messagesDiv.scrollTop === 300 && evalJs('isPinned()') === false);

  // 6. A second rebuild for the same session (turn_end convergence
  //    refetch shape — the snapshot must GROW, an identical one is
  //    correctly skipped as stale without any rebuild) must restore
  //    AGAIN — the anchor is kept on use, not consumed.
  messagesDiv.scrollTop = 0;
  recv({ type: 'history', sessionId: 'A', history: history(10) });
  await sleep(120);
  check('anchor kept after use: the convergence refetch restores too',
    messagesDiv.querySelectorAll('.message').length === 10
    && messagesDiv.scrollTop === 300
    && evalJs('isPinned()') === false);

  // 7. Reshaped history (epoch bump — compaction/rollback): the anchor's
  //    indexes are garbage; the rebuild must fall back to the bottom and
  //    NOT apply the stale anchor.
  recv({ type: 'history', sessionId: 'A', historyEpoch: 5, history: history(4) });
  await sleep(120);
  check('reshaped snapshot rebuilds and pins to the bottom',
    messagesDiv.querySelectorAll('.message').length === 4
    && messagesDiv.scrollTop === bottom()
    && evalJs('isPinned()') === true);

  // 8. Session change: /resume delivers the NEW session's history while
  //    the pane still holds the old id — the old session's anchor (epoch
  //    0, which would MATCH the incoming snapshot's epoch) must never
  //    position another session's transcript.
  recv({ type: 'waiting', sessionId: 'A' });
  window.resumeSession('C');
  // (resumeSession early-returns when a change is already in flight, so
  // the sent frame also proves the flag was clear before it.)
  check('resume sent', ws.sent.some((m) => m.type === 'session_resume' && m.sessionId === 'C'));
  messagesDiv.scrollTop = 0;
  recv({ type: 'history', sessionId: 'C', history: history(2) });
  await sleep(120);
  check('resumed session lands at the bottom, not on session A\'s anchor',
    messagesDiv.scrollTop === bottom() && evalJs('isPinned()') === true);

  // 9. Closing the pane drops its anchor: switch to B while unpinned
  //    mid-history (so the switch captures a fresh anchor for A), close
  //    the pane, reopen the saved session and ship a MATCHING-epoch
  //    snapshot — if the anchor had survived the close, it would restore;
  //    it must stay at the bottom.
  messagesDiv.scrollTop = 300;
  evalJs('unpinFromBottom()');
  window.openSessionPane('B');
  const aKey = evalJs('findPaneBySession("A").key');
  window.closePane(aKey);
  window.openSessionPane('A');
  messagesDiv.scrollTop = 0;
  recv({ type: 'history', sessionId: 'A', history: history(4) });
  await sleep(120);
  check('closed pane\'s anchor dropped: reopen lands at the bottom',
    messagesDiv.scrollTop === bottom() && evalJs('isPinned()') === true);

  // 10. Session removal while the pane is ACTIVE (deleted elsewhere while
  //     viewing): handleSessionRemoved drops the session's anchor — pure
  //     hygiene, since a removed session can never be re-rendered (unlike
  //     the closePane case above there is no scroll-behavioral probe) —
  //     and replaces the pane with a fresh session. Assert that flow is
  //     intact with the anchor cleanup in the handler.
  recv({ type: 'session_removed', sessionId: 'A' });
  check('session_removed with an active pane starts a fresh session',
    evalJs('activePane().id') === null);

  // 11. Settled panes keep the exact domCache restore (the anchor is only
  //     the rebuild fallback): capture at a negative subPx offset, reopen,
  //     and the SAME DOM nodes must come back at the exact offset.
  //     (B must be ACTIVE while its history arrives — background panes
  //     don't build DOM; they re-derive on focus.)
  window.openSessionPane('B');
  recv({ type: 'history', sessionId: 'B', historyEpoch: 0, history: history(6) });
  await sleep(120);
  messagesDiv.scrollTop = 350; // message 3 starts 50px above the top edge
  evalJs('unpinFromBottom()');
  const userB = messagesDiv.querySelector('.message.user');
  window.openSessionPane('C');
  check('settled pane B was dom-cached, not wiped',
    evalJs('findPaneBySession("B").domCache') !== null);
  window.openSessionPane('B');
  check('settled restore keeps the exact offset (domCache path)',
    messagesDiv.scrollTop === 350,
    `scrollTop=${messagesDiv.scrollTop}`);
  check('settled restore brings back the same DOM nodes',
    messagesDiv.querySelector('.message.user') === userB);

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
