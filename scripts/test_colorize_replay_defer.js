// Integration check for the replay colorize deferral: app.js must bracket
// every history replay with beginColorizeDeferral()/endColorizeDeferral()
// (editor.js gates the Monaco import and all colorize jobs on it), so a
// restored session paints before any 4.5MB Monaco work starts.
//
// The colorize calls themselves are stubbed (the scheduler's own behavior is
// covered by test_colorize_schedule.js); this test pins the CONTRACT: one
// begin before the first message is rendered, one end after the last, for
// every replay, including a second (pane-switch style) replay.
//
// Run: node scripts/test_colorize_replay_defer.js
'use strict';
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');

async function main() {
  const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');
  const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;

  window.matchMedia = window.matchMedia || (() => ({ matches: false, addEventListener() {}, removeEventListener() {} }));
  window.Notification = { permission: 'denied', requestPermission: () => Promise.resolve('denied') };
  window.navigator.clipboard = { writeText: async () => {} };
  window.Terminal = class { open() {} write() {} fit() {} onData() {} onResize() { return { dispose() {} }; } dispose() {} reset() {} focus() {} };
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

  // Recorders installed BEFORE app.js evaluates: the stripped import resolves
  // these globals at call time.
  const calls = [];
  const bubblesOnScreen = () => window.document.querySelectorAll('#messages .message').length;
  window.beginColorizeDeferral = () => { calls.push({ call: 'begin', messages: bubblesOnScreen() }); };
  window.endColorizeDeferral = () => { calls.push({ call: 'end', messages: bubblesOnScreen() }); };

  window.marked = { use() {}, parse: (t) => String(t == null ? '' : t) };
  window.DOMPurify = { sanitize: (r) => r };

  loadAppJs(window);

  const evalJs = (code) => window.eval(code);
  const ws = sockets[0];
  if (!ws) throw new Error('app.js did not construct a WebSocket');
  const recv = (obj) => ws.onmessage({ data: JSON.stringify(obj) });

  let failures = 0;
  const check = (desc, ok, detail) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}${!ok && detail ? ' — ' + detail : ''}`);
    if (!ok) failures++;
  };
  const waitFor = async (pred, ms = 5000) => {
    const until = Date.now() + ms;
    while (!pred() && Date.now() < until) await new Promise((r) => setTimeout(r, 5));
    return pred();
  };

  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  recv({
    type: 'config', sessionId: 'A', sessionLabel: 'A', mode: 'act', thinkingLevel: 'off',
    reasoningEfforts: [], model: 'm1', modelDescription: '', workingDir: '/tmp', globalMode: false,
  });
  check('pane A adopted', evalJs('activePane().id') === 'A');

  const history = [
    { role: 'user', content: 'first question', index: 0, createdAt: new Date().toISOString() },
    { role: 'assistant', content: 'first answer', index: 1, createdAt: new Date().toISOString() },
    { role: 'user', content: 'second question', index: 2, createdAt: new Date().toISOString() },
    {
      role: 'assistant', content: '', index: 3, createdAt: new Date().toISOString(),
      toolCalls: [{ id: 'c1', name: 'read_file', args: { path: 'x' } }],
    },
    { role: 'tool', toolCallId: 'c1', content: 'file contents', index: 4, createdAt: new Date().toISOString() },
    { role: 'assistant', content: 'second answer with a fence:\n\n```js\nconst x = 1;\n```', index: 5, createdAt: new Date().toISOString() },
  ];
  const expectedBubbles = 4; // 2 user + 2 assistant with content

  recv({ type: 'history', history, historyEpoch: 0, sessionId: 'A' });
  check('replay bracketed (begin + end seen)', await waitFor(() => calls.length >= 2), JSON.stringify(calls));
  check('exactly one begin and one end', calls.length === 2 && calls[0].call === 'begin' && calls[1].call === 'end',
    JSON.stringify(calls));
  check('begin runs before any transcript message is rendered', calls[0] && calls[0].messages === 0,
    'messages at begin=' + (calls[0] && calls[0].messages));
  check('end runs after the whole transcript is rendered', calls[1] && calls[1].messages >= expectedBubbles,
    'messages at end=' + (calls[1] && calls[1].messages));

  // A second replay (a pane switch / re-attach with new content) must bracket
  // again. (An identical snapshot is skipped by the stale-history guard, so
  // the second frame carries one more message.)
  calls.length = 0;
  const history2 = history.concat([
    { role: 'assistant', content: 'third answer', index: 6, createdAt: new Date().toISOString() },
  ]);
  recv({ type: 'history', history: history2, historyEpoch: 0, sessionId: 'A' });
  check('second replay bracketed', await waitFor(() => calls.length >= 2), JSON.stringify(calls));
  check('second replay brackets the same way',
    calls.length === 2 && calls[0].call === 'begin' && calls[0].messages === 0
      && calls[1].call === 'end' && calls[1].messages >= expectedBubbles,
    JSON.stringify(calls));

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => {
  console.error('HARNESS ERROR:', err && err.stack || err);
  process.exit(1);
});
