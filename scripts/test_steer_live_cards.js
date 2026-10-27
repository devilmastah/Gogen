'use strict';
// Regression test for the steering send that races the in-flight turn's
// tool state:
//   - sending while the pane's turn is ACTIVE is QUEUED (steering) and the
//     in-flight turn keeps running, so its per-turn tool state must survive
//     the send (streaming args, cards awaiting their result, live terminal
//     mirror, the "tools started this round" flag);
//   - pre-fix the send wiped those maps (the old cancel-then-send
//     behavior), so every tool that was already running lost its card: the
//     next tool_result found no pending card and fell through to the raw
//     "[tool] <result>" system blob (the reported corruption), the live
//     terminal mirror went dark, and a still-streaming call's arg deltas
//     opened a SECOND card;
//   - a mid-turn re-attach (pane focus / reconnect while a tool runs) renders
//     the in-flight call from the rewind, whose result must ALSO land in that
//     card — the client never saw the live tool_call frame, so the card must
//     be registered by the call's id.
//
// Loads the real index.html + app.js into jsdom (imports stripped, editor/
// marked/dompurify stubbed) with a controllable WebSocket stub.
// Run: node scripts/test_steer_live_cards.js
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
    constructor() { this.readyState = 0; this.sent = []; sockets.push(this); }
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
  const input = doc.getElementById('message-input');

  let failures = 0;
  const check = (desc, ok, extra) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}${ok || !extra ? '' : ' — ' + extra}`);
    if (!ok) failures++;
  };
  const cards = () => [...messagesDiv.querySelectorAll('.tool-card')];
  const systemBlobs = () => [...messagesDiv.querySelectorAll('.message.system')];
  const lastSentMessage = () => ws.sent.filter((m) => m.type === 'message').pop();
  const send = (text) => { input.value = text; evalJs('sendMessage()'); };

  // Adopt session A (idle) with a one-message transcript.
  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  recv({
    type: 'config', sessionId: 'A', sessionLabel: 'A-label',
    mode: 'act', thinkingLevel: 'off', reasoningEfforts: [], model: 'm1',
    modelDescription: '', workingDir: '/tmp', globalMode: false,
  });
  recv({
    type: 'history', sessionId: 'A',
    history: [
      { role: 'user', content: 'question one', index: 0, createdAt: '2024-01-01T00:00:00Z' },
      { role: 'assistant', content: 'answer one', index: 1, createdAt: '2024-01-01T00:00:01Z' },
    ],
  });
  await sleep(200);
  check('pane A adopted', evalJs('activePane().id') === 'A');

  // ── Round 1: two tool calls announced, one already waiting on its result,
  //    the other still streaming its args. ──
  const CMD = 'grep -n hover internal/server/web/styles.css';
  const GREP_OUT = 'internal/server/web/styles.css:269:           and revealed on hover (app.js toggles .has-dots for presence and';
  recv({ type: 'thinking', sessionId: 'A' });
  recv({ type: 'stream', sessionId: 'A', content: 'checking the css', contentPos: 17 });
  recv({ type: 'tool_call_start', sessionId: 'A', index: 0, tool: 'execute_command' });
  recv({ type: 'tool_call_delta', sessionId: 'A', index: 0, toolCallId: 't1', tool: 'execute_command', argsDelta: '{"command":"grep -n hover internal/server/web/styles.css"}', argsPos: 56 });
  recv({ type: 'tool_call', sessionId: 'A', index: 0, toolCallId: 't1', tool: 'execute_command', args: { command: CMD } });
  recv({ type: 'tool_execute', sessionId: 'A', toolCallId: 't1', tool: 'execute_command' });
  recv({ type: 'term_opened', sessionId: 'A', termId: 't1', toolCallId: 't1', tool: 'execute_command', content: '$ ' + CMD });
  recv({ type: 'term_output', sessionId: 'A', termId: 't1', content: GREP_OUT + '\n' });

  // Second call still streaming its args when the user steers.
  recv({ type: 'tool_call_start', sessionId: 'A', index: 1, tool: 'read_file' });
  recv({ type: 'tool_call_delta', sessionId: 'A', index: 1, toolCallId: 't2', tool: 'read_file', argsDelta: '{"path":', argsPos: 8 });

  const t1Card = cards()[0];
  check('two cards exist before the send (one pending, one streaming)', cards().length === 2);
  check('the running card shows the command echo', !!t1Card && t1Card.querySelector('.tool-live-output') !== null);

  // ── The steering send: the pane is busy, so the message is QUEUED and the
  //    in-flight turn keeps running. ──
  send('keep going');
  const qid = lastSentMessage() && lastSentMessage().queueId;
  check('steering send is tagged with a queue id', typeof qid === 'string' && qid.length > 0);
  recv({ type: 'queue_update', sessionId: 'A', queue: [{ id: qid, text: 'keep going' }] });
  check('the send did not disturb the two in-flight cards', cards().length === 2);

  // The turn continues: the still-streaming call finishes its args.
  recv({ type: 'tool_call_delta', sessionId: 'A', index: 1, toolCallId: 't2', tool: 'read_file', argsDelta: '"x"}', argsPos: 13 });
  recv({ type: 'tool_call', sessionId: 'A', index: 1, toolCallId: 't2', tool: 'read_file', args: { path: 'x' } });
  check('post-send arg deltas continue the SAME card (no duplicate)', cards().length === 2);
  const t2Card = cards()[1];
  check('the continued card carries the full args', !!t2Card && /path=/.test(t2Card.textContent),
    t2Card && t2Card.textContent.slice(0, 120));

  // Live terminal output keeps mirroring into the running card.
  recv({ type: 'term_output', sessionId: 'A', termId: 't1', content: 'internal/server/web/styles.css:2393:  /* On pointer devices\n' });
  check('post-send live output still mirrors into the card',
    !!t1Card && /styles\.css:2393/.test(t1Card.querySelector('.tool-live-output') ? t1Card.querySelector('.tool-live-output').textContent : ''),
    t1Card && t1Card.querySelector('.tool-live-output') && t1Card.querySelector('.tool-live-output').textContent.slice(0, 120));

  // Results land in their cards — never as raw "[tool] result" blobs.
  recv({ type: 'tool_result', sessionId: 'A', toolCallId: 't1', tool: 'execute_command', result: GREP_OUT + '\nstyles.css:3703:  /* Touch devices have no hover', success: true });
  recv({ type: 'tool_result', sessionId: 'A', toolCallId: 't2', tool: 'read_file', result: 'file body', success: true });
  // The shell card streamed its output live, so the result body is the
  // streamed log (plus the OK status) — the point is that the CARD took the
  // result, not the transcript.
  check('the running tool result landed in its card',
    !!t1Card && !!t1Card.querySelector('.tool-result-body')
    && !!t1Card.querySelector('.tool-status-bar') && !t1Card.querySelector('.tool-waiting')
    && /styles\.css:2393/.test(t1Card.textContent),
    t1Card && t1Card.textContent.slice(0, 140));
  check('the second result landed in its card',
    !!t2Card && !!t2Card.querySelector('.tool-result-body') && /file body/.test(t2Card.querySelector('.tool-result-body').textContent));
  check('no raw "[tool] <result>" system blob was appended', systemBlobs().length === 0,
    systemBlobs().map((el) => el.textContent.slice(0, 80)).join(' | '));

  // ── Re-attach mid-turn (pane focused / reconnected while a tool runs):
  //    the rewind-only history frame carries the in-flight call, and its
  //    result must land in that card — not a raw "[tool] <result>" blob. ──
  const blobsBefore = systemBlobs().length;
  recv({
    type: 'history', sessionId: 'A', rewindOnly: true, historyEpoch: 0,
    rewind: {
      content: 'partial reply', contentPos: 13,
      toolCalls: [{ index: 0, id: 'r9', name: 'run_tests', args: '{"command":"make test"}', argsPos: 24 }],
    },
  });
  const rewindCard = cards()[cards().length - 1];
  check('the rewind rendered a card for the in-flight call',
    !!rewindCard && /run_tests/.test(rewindCard.textContent));
  recv({ type: 'tool_result', sessionId: 'A', toolCallId: 'r9', tool: 'run_tests', result: 'PASS: empty state returns after clearChat', success: true });
  check('the rewind card took its result instead of a raw blob',
    systemBlobs().length === blobsBefore
    && !!rewindCard.querySelector('.tool-result-body')
    && /PASS: empty state returns/.test(rewindCard.textContent),
    systemBlobs().map((el) => el.textContent.slice(0, 80)).join(' | '));

  // A result with no card at all (a frame this client never saw announced)
  // must still render as a card — never a raw "[tool] <result>" blob.
  const cardsBefore = cards().length;
  recv({ type: 'tool_result', sessionId: 'A', toolCallId: 'ghost', tool: 'run_lint', result: 'lint-web: OK', success: true });
  const ghostCard = cards()[cards().length - 1];
  check('an uncarded result renders as a card, not a raw blob',
    cards().length === cardsBefore + 1 && systemBlobs().length === 0
    && !!ghostCard.querySelector('.tool-result-body') && /lint-web: OK/.test(ghostCard.textContent),
    systemBlobs().map((el) => el.textContent.slice(0, 80)).join(' | '));

  // A late thinking token must not reopen a thought card mid-round.
  recv({ type: 'thinking_token', sessionId: 'A', content: 'stray reasoning', thinkingPos: 15 });
  check('late reasoning is still suppressed after a tool started the round',
    doc.querySelectorAll('.thought-card').length === 0);

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
