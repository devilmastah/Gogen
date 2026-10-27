'use strict';
// Regression tests for the streaming-path performance work in the web UI:
//
//   P1 (tail colorize): streaming-tail renders pass {streamingTail:true} to
//       colorizeNode, so an uncached code block in the in-flight tail
//       tokenizes in the no-cache mode — background colorization stays
//       visible while the block grows (the pre-optimization UX), but the
//       result is never written to the shared LRU (a still-growing source
//       can never be hit again) and stale applies are dropped by element
//       identity/text. Completed blocks (stable .md-block nodes) and full
//       single-shot renders (stream end) keep the cached path.
//
//   P2 (live output): appendToolCardLiveOutput appends incrementally (one
//       merged text node) instead of rewriting the whole buffer per frame,
//       keeps the 128KB cap + truncation-marker semantics, and ignores
//       frames after truncation.
//
// Loads the real index.html + app.js into jsdom (imports stripped, editor/
// marked/dompurify stubbed) with a controllable WebSocket stub, then drives
// server frames through ws.onmessage like the live client does.
//
// Run: node scripts/test_stream_perf.js
// APPJS env var selects the app.js copy to test (defaults to the real one).
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
const APPJS = process.env.APPJS || path.join(ROOT, 'internal/server/web/app.js');

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

  // Recorder stub installed BEFORE app.js eval: renderBlockNode calls the
  // global colorizeNode, so every (node, opts) pair is captured. This is
  // the P1 oracle — assertions run on the recorded opts, not the DOM.
  const colorizeCalls = [];
  window.colorizeNode = (node, opts) => {
    colorizeCalls.push({ node, streamingTail: !!(opts && opts.streamingTail) });
  };
  // markdown.js seals the streaming DOM before recreating elements; this
  // test's oracle is the colorizeNode recorder, so the seal is a no-op.
  window.sealStreamColorize = () => {};

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

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };

  const doc = window.document;
  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  recv({
    type: 'config', sessionId: 'A', sessionLabel: 'A-label',
    mode: 'act', thinkingLevel: 'off', reasoningEfforts: [], model: 'm1',
    modelDescription: '', workingDir: '/tmp', globalMode: false,
  });
  check('pane A adopted', evalJs('activePane().id') === 'A');

  // ── P1: streaming tail must not enqueue background tokenization ──
  const streamText = 'para one\n\n```js\nconst x = 1;\n';
  recv({ type: 'stream', sessionId: 'A', content: streamText, contentPos: streamText.length });
  // Force the (rAF-scheduled) flush synchronously for determinism.
  evalJs('flushStreamRender()');

  check('streaming tail node exists', !!doc.querySelector('.md-tail'));
  check('stable block node exists', !!doc.querySelector('.md-block:not(.md-tail)'));

  const isTail = (n) => !!(n && n.classList && n.classList.contains('md-tail'));
  const isStableBlock = (n) => !!(n && n.classList && n.classList.contains('md-block') && !n.classList.contains('md-tail'));
  const tailCalls = colorizeCalls.filter((c) => isTail(c.node));
  const stableCalls = colorizeCalls.filter((c) => isStableBlock(c.node));
  check('tail render reached colorizeNode', tailCalls.length > 0);
  check('tail renders pass streamingTail (no-cache tokenize mode)', tailCalls.length > 0 && tailCalls.every((c) => c.streamingTail));
  check('stable block renders colorize normally', stableCalls.length > 0 && stableCalls.every((c) => !c.streamingTail));

  // Stream end: the full single-shot render must colorize normally.
  recv({ type: 'stream_end', sessionId: 'A' });
  const msgText = doc.querySelector('.message .msg-text');
  check('final .msg-text render exists', !!msgText);
  const finalCalls = colorizeCalls.filter((c) => c.node === msgText);
  check('stream-end full render colorizes normally', finalCalls.length > 0 && finalCalls.every((c) => !c.streamingTail));

  // ── P2: tool-card live output appends incrementally with the cap ──
  recv({ type: 'tool_call_start', sessionId: 'A', index: 0, tool: 'execute_command' });
  recv({
    type: 'tool_call', sessionId: 'A', index: 0, toolCallId: 't1',
    tool: 'execute_command', args: { command: 'x' },
  });
  recv({ type: 'term_opened', sessionId: 'A', termId: 't1', toolCallId: 't1', tool: 'execute_command', content: '$ x' });

  const live = doc.querySelector('.tool-live-output');
  check('live output region created', !!live);
  check('command echo captured', !!live && live.textContent === '$ x\n');

  recv({ type: 'term_output', sessionId: 'A', termId: 't1', content: 'line1\n' });
  recv({ type: 'term_output', sessionId: 'A', termId: 't1', content: 'line2\n' });
  recv({ type: 'term_output', sessionId: 'A', termId: 't1', content: 'line3\n' });
  check('output accumulates verbatim', !!live && live.textContent === '$ x\nline1\nline2\nline3\n');
  check('appends merge into a single text node',
    !!live && live.childNodes.length === 1 && live.firstChild.nodeType === 3 /* TEXT_NODE */);

  // Cap: a chunk that fits, then one that overflows the 128 KB limit.
  const big = 'y'.repeat(100 * 1024); // 22 + 102400 < 131072: still fits
  recv({ type: 'term_output', sessionId: 'A', termId: 't1', content: big });
  const expectedBig = '$ x\nline1\nline2\nline3\n' + big;
  check('large in-cap chunk appended', !!live && live.textContent === expectedBig);
  check('still one text node after large chunk', !!live && live.childNodes.length === 1);

  recv({ type: 'term_output', sessionId: 'A', termId: 't1', content: 'z'.repeat(64 * 1024) }); // overflows
  const capped = live.textContent;
  check('overflow trips the truncation marker',
    capped.endsWith('output truncated in card (see terminal tab for the full log)'));
  check('pre-cap buffer preserved through truncation', capped.startsWith(expectedBig));

  recv({ type: 'term_output', sessionId: 'A', termId: 't1', content: 'post-truncation frame' });
  check('frames after truncation are ignored', live.textContent === capped);

  recv({ type: 'term_exit', sessionId: 'A', termId: 't1', success: true });
  recv({ type: 'tool_result', sessionId: 'A', toolCallId: 't1', tool: 'execute_command', result: 'done', success: true });
  check('card result renders after exit', !!doc.querySelector('.tool-result-body'));

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => {
  console.error('HARNESS ERROR:', err && err.stack || err);
  process.exit(1);
});
