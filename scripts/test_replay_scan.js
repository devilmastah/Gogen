'use strict';
// Regression test for the history-replay (restore) scan budget.
//
// Appending a message used to call messagesDiv.querySelector('.empty-state')
// to clear the placeholder — a transcript-wide scan per message, which made
// every replay O(n²) (measured: ~60% of the replay time on a session of a few
// hundred messages; the scan walks every node above the new message). The
// placeholder is now probed through the transcript's first element child, so
// a replay must not issue per-message, transcript-wide querySelectors at all.
//
// The check counts querySelector calls whose receiver IS #messages (selector
// text agnostic, so a different selector could not sneak the cost back) while
// a synthesised history frame replays; a small constant is allowed for the
// deliberate whole-transcript probes (empty-state ensure, stale-snapshot
// checks).
//
// Loads the real index.html + app.js into jsdom (imports stripped, editor/
// marked/dompurify stubbed) with a controllable WebSocket stub.
//
// Run: node scripts/test_replay_scan.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('/tmp/gogen-jsdom/node_modules/jsdom');
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
const MESSAGES = 60; // 30 user + 30 assistant pairs

let failures = 0;
function check(desc, ok, detail) {
  console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}${ok || detail === undefined ? '' : ' — ' + detail}`);
  if (!ok) failures++;
}

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
    static CONNECTING = 0;
    send(m) { this.sent.push(JSON.parse(m)); }
    close() { this.readyState = 3; }
  }
  window.WebSocket = FakeWS;
  installEditorStubs(window);
  window.sealStreamColorize = () => {};
  window.marked = { use() {}, parse: (t) => String(t == null ? '' : t) };
  window.DOMPurify = { sanitize: (raw) => raw };
  loadAppJs(window);

  const doc = window.document;
  const messagesDiv = doc.getElementById('messages');
  const ws = sockets[0];
  const recv = (o) => ws.onmessage({ data: JSON.stringify(o) });

  // Count transcript-wide querySelector calls during the replay only.
  let wideScans = 0;
  const origQS = window.Element.prototype.querySelector;
  window.Element.prototype.querySelector = function (sel) {
    if (this === messagesDiv) wideScans++;
    return origQS.call(this, sel);
  };

  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  recv({
    type: 'config', sessionId: 'A', sessionLabel: 'A', mode: 'act',
    thinkingLevel: 'off', reasoningEfforts: [], model: 'm1',
    modelDescription: '', workingDir: '/tmp', globalMode: false,
  });

  const history = [];
  for (let i = 0; i < MESSAGES; i++) {
    history.push({ role: 'user', content: 'msg ' + i, index: i, createdAt: '2024-01-01T00:00:00Z' });
    history.push({ role: 'assistant', content: 'reply ' + i, index: i * 2 + 1, createdAt: '2024-01-01T00:00:01Z' });
  }
  const expected = MESSAGES * 2;
  wideScans = 0;
  // Record when the viewport is pinned relative to how much has rendered:
  // a rebuild must follow as it grows (pins before the end), not sit still
  // until the final settle after the whole transcript has painted.
  const pinAtRendered = [];
  const origPin = window.pinToBottom;
  window.pinToBottom = function (...args) {
    pinAtRendered.push(messagesDiv.querySelectorAll('.message').length);
    return origPin.apply(this, args);
  };
  recv({ type: 'session_state', sessionId: 'A', turnActive: false });
  recv({ type: 'history', sessionId: 'A', history, historyEpoch: 1 });
  // The rebuild runs blurred (see beginRestoring): the transcript stays on
  // screen — nothing is hidden (a blank pane read as a crash) — while the
  // 12-messages-per-frame startup motion is softened.
  check('rebuild is blurred while the replay runs',
    messagesDiv.classList.contains('restoring'));
  const deadline = Date.now() + 30000;
  let rendered = 0;
  while (Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 5));
    rendered = messagesDiv.querySelectorAll('.message').length;
    if (rendered >= expected) break;
  }
  window.Element.prototype.querySelector = origQS;

  check('replay rendered the whole history', rendered === expected, rendered + '/' + expected);
  check('rebuild blur is lifted after the settle', !messagesDiv.classList.contains('restoring'));
  check('no restoring placeholder row is injected (the transcript stays visible)',
    !messagesDiv.querySelector('.restoring-placeholder'));
  // A rebuild of the ALREADY-VISIBLE transcript (same session, messages on
  // screen — what the turn-end convergence refetch triggers) must not blur:
  // nothing is being restored, and the blur used to flash whenever a turn
  // completed. It still re-renders (that is the refetch's purpose).
  {
    const before = messagesDiv.querySelectorAll('.message').length;
    const more = history.concat([
      { role: 'assistant', content: 'reply final', index: expected, createdAt: '2024-01-01T00:00:02Z' },
    ]);
    recv({ type: 'history', sessionId: 'A', history: more, historyEpoch: 1 });
    check('convergence refetch does not blur the visible transcript',
      !messagesDiv.classList.contains('restoring'));
    const want = before + 1;
    const deadlineRefetch = Date.now() + 30000;
    let now = before;
    while (Date.now() < deadlineRefetch) {
      await new Promise((r) => setTimeout(r, 5));
      now = messagesDiv.querySelectorAll('.message').length;
      if (now >= want) break;
    }
    check('convergence refetch rendered the new message', now >= want, now + '/' + want);
    check('convergence refetch lifts cleanly (no blur left behind)',
      !messagesDiv.classList.contains('restoring'));
  }
  check('viewport follows the rebuild while it renders (not only at the end)',
    pinAtRendered.some((n) => n > 0 && n < expected),
    'pins at rendered counts: [' + pinAtRendered.join(', ') + '] of ' + expected);
  // Before the fix this was one scan per message (≈ expected). The deliberate
  // whole-transcript probes are a small constant.
  check('replay issues no per-message transcript-wide querySelector',
    wideScans <= 8, 'transcript-wide scans=' + wideScans + ' for ' + rendered + ' messages');
  // The placeholder must still be managed: it is gone while messages exist,
  // and comes back when the transcript is cleared.
  check('empty state absent while messages exist',
    !messagesDiv.firstElementChild || !messagesDiv.firstElementChild.classList.contains('empty-state'));
  window.eval('clearChat()');
  check('empty state returns after clearChat',
    !!messagesDiv.firstElementChild && messagesDiv.firstElementChild.classList.contains('empty-state'));
  window.eval('clearChat()');
  check('clearChat twice does not stack placeholders',
    messagesDiv.querySelectorAll('.empty-state').length === 1);

  // ── Live-turn index tracking + the conditional convergence attach ──
  // The server stamps every message it appends with a hist_index frame; the
  // client advances the pane's rendered-index mark, and the turn-end
  // convergence attach claims that fingerprint so the server can answer
  // "already current" (no snapshot, no rebuild) instead of shipping the full
  // history. A mark that trails still falls back to the snapshot.
  {
    // Rebuild a pane transcript to track: reuse the replayed history.
    recv({ type: 'history', sessionId: 'A', history, historyEpoch: 1 });
    const deadlineMark = Date.now() + 30000;
    while (Date.now() < deadlineMark) {
      await new Promise((r) => setTimeout(r, 5));
      if (messagesDiv.querySelectorAll('.message').length >= expected) break;
    }
    recv({ type: 'session_state', sessionId: 'A', turnActive: true }); // latches needsFreshHistory
    const markBefore = window.eval('activePane().renderedHistIdx');
    recv({ type: 'hist_index', sessionId: 'A', content: String(expected + 2) });
    const markAfter = window.eval('activePane().renderedHistIdx');
    check('hist_index advances the pane rendered-index mark',
      markBefore <= expected && markAfter === expected + 2,
      'before=' + markBefore + ' after=' + markAfter);

    const sentBefore = ws.sent.length;
    recv({ type: 'turn_end', sessionId: 'A' });
    const attach = ws.sent.slice(sentBefore).reverse().find((m) => m.type === 'session_attach');
    check('turn-end convergence attach claims the history fingerprint',
      !!attach && attach.knownHistoryEpoch === 1 && attach.knownHistoryIndex === expected + 2,
      JSON.stringify(attach || null));
    check('a skipped refetch leaves the transcript untouched',
      messagesDiv.querySelectorAll('.message').length >= expected);
  }

  // ── A dropped frame / wiped DOM must void the pane's claim ──
  // The mark may only prove what the pane actually RENDERED. Two ways it can
  // stop being a lower bound of that: the DOM is wiped (clearChat) while the
  // epoch/mark survive, and a position-stamped chunk arrives with a hole
  // before it (a frame dropped by the server's 5s send-queue timeout). Both
  // must void the claim, so the next attach takes the full snapshot instead
  // of proving "current" over a transcript with a hole.
  {
    // Re-establish a claimed, current pane.
    recv({ type: 'history', sessionId: 'A', history, historyEpoch: 1 });
    const deadlineClaim = Date.now() + 30000;
    while (Date.now() < deadlineClaim) {
      await new Promise((r) => setTimeout(r, 5));
      if (window.eval('activePane().histEpoch') === 1
        && messagesDiv.querySelectorAll('.message').length >= expected) break;
    }
    check('pane re-established with a history epoch',
      window.eval('activePane().histEpoch') === 1);

    // A wipe with no replay (a reconnect whose attach snapshot write was
    // dropped): clearChat must void the epoch + mark with the DOM.
    recv({ type: 'clear_chat' });
    check('clearChat voids the pane history epoch',
      window.eval('activePane().histEpoch') === undefined);
    check('clearChat resets the pane rendered-index mark',
      window.eval('activePane().renderedHistIdx') === -1);

    // Re-establish, latch the convergence refetch, then drop a stream frame:
    // the chunk after the hole starts past the rendered position.
    recv({ type: 'history', sessionId: 'A', history, historyEpoch: 1 });
    const deadlineAgain = Date.now() + 30000;
    while (Date.now() < deadlineAgain) {
      await new Promise((r) => setTimeout(r, 5));
      // Wait for the REPLAY to finish, not just its epoch stamp: live frames
      // arriving mid-replay are buffered and dispatched after it.
      if (window.eval('activePane().histEpoch') === 1
        && messagesDiv.querySelectorAll('.message').length >= expected) break;
    }
    recv({ type: 'session_state', sessionId: 'A', turnActive: true }); // latch the refetch
    recv({ type: 'stream', sessionId: 'A', content: 'abc', contentPos: 3 });
    recv({ type: 'stream', sessionId: 'A', content: 'zz', contentPos: 9 }); // 4 chars lost
    check('a position gap voids the pane history epoch',
      window.eval('activePane().histEpoch') === undefined);
    const sentGap = ws.sent.length;
    recv({ type: 'turn_end', sessionId: 'A' });
    const gapAttach = ws.sent.slice(sentGap).reverse().find((m) => m.type === 'session_attach');
    check('convergence attach after a gap does NOT claim the fingerprint',
      !!gapAttach && !gapAttach.knownHistoryEpoch && !gapAttach.knownHistoryIndex,
      JSON.stringify(gapAttach || null));

    // Contiguous chunks must NOT void anything (the rewind merge and the live
    // tail are exact by construction): a false positive here would cost a
    // full rebuild on every in-flight attach.
    recv({ type: 'history', sessionId: 'A', history, historyEpoch: 1 });
    const deadlineOk = Date.now() + 30000;
    while (Date.now() < deadlineOk) {
      await new Promise((r) => setTimeout(r, 5));
      if (window.eval('activePane().histEpoch') === 1
        && messagesDiv.querySelectorAll('.message').length >= expected) break;
    }
    recv({ type: 'session_state', sessionId: 'A', turnActive: true });
    recv({
      type: 'history', sessionId: 'A', rewindOnly: true, historyEpoch: 1,
      rewind: { content: 'abc', contentPos: 3 },
    });
    recv({ type: 'stream', sessionId: 'A', content: 'def', contentPos: 6 });
    check('contiguous rewind + live chunks keep the claim (no false gap)',
      window.eval('activePane().histEpoch') === 1,
      'epoch=' + window.eval('activePane().histEpoch'));
  }

  window.close();
  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => {
  console.error('HARNESS ERROR:', err && err.stack || err);
  process.exit(1);
});
