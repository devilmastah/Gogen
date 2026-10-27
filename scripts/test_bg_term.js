'use strict';
// Regression test: background-job terminal output streams into the web UI
// chat card.
//
// execute_command background=true returns its tool result (the job id)
// immediately while the job keeps running, so the card leaves
// pendingToolCards BEFORE most of the output arrives. The client keeps a
// bgTermCards mirror (keyed by term id) so handleTermOutput keeps appending
// to the card's live region until term_exit — and the "Started background
// job ..." result must still be shown (it is not a duplicate of the
// streamed output, unlike a foreground command's result).
//
// Frame sequence driven here (the server order since the background-job
// streaming fix):
//   tool_call → term_opened (command echo, before the result)
//   → tool_result (job id) → term_output* (post-result, post-turn_end)
//   → term_exit (real exit status)
//
// Loads the real index.html + app.js into jsdom (imports stripped,
// editor/marked/dompurify stubbed) with a controllable WebSocket stub.
//
// Run: node scripts/test_bg_term.js
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

  const CMD = 'sleep 5 && echo bg-hello';
  const TERM = 'tb1';

  // Tool call with background=true, then the terminal opens BEFORE the
  // result (the agent fires the open signal at job launch).
  recv({ type: 'tool_call_start', sessionId: 'A', index: 0, tool: 'execute_command' });
  recv({
    type: 'tool_call', sessionId: 'A', index: 0, toolCallId: TERM,
    tool: 'execute_command', args: { command: CMD, background: true },
  });
  recv({ type: 'term_opened', sessionId: 'A', termId: TERM, toolCallId: TERM, tool: 'execute_command', content: '$ ' + CMD });

  const live = doc.querySelector('.tool-live-output');
  check('live output region created', !!live);
  check('command echo captured before the result', !!live && live.textContent === '$ ' + CMD + '\n');

  // The tool result arrives while the job is still running.
  const result = 'Started background job job-abc123.\nCommand: ' + CMD + '\nPoll with background_job (action=status, job_id: "job-abc123") or cancel with action=cancel.';
  recv({ type: 'tool_result', sessionId: 'A', toolCallId: TERM, tool: 'execute_command', result, success: true });
  check('job-id result is shown (not swallowed as a duplicate)',
    !!doc.querySelector('.tool-result-body') && doc.querySelector('.tool-result-body').textContent.includes('job-abc123'));

  // Output keeps streaming AFTER the result — this is the fix: the card
  // must keep mirroring it.
  recv({ type: 'term_output', sessionId: 'A', termId: TERM, content: 'bg-hello\n' });
  check('post-result output mirrored into the chat card',
    !!live && live.textContent === '$ ' + CMD + '\nbg-hello\n');

  // The turn ends while the job is still running; streaming continues.
  recv({ type: 'turn_end', sessionId: 'A' });
  recv({ type: 'term_output', sessionId: 'A', termId: TERM, content: 'more\n' });
  check('post-turn_end output still mirrored',
    !!live && live.textContent === '$ ' + CMD + '\nbg-hello\nmore\n');

  // Job exits: the live region is stamped done/success and the mirror
  // entry is dropped (late frames must no longer reach the card).
  recv({ type: 'term_exit', sessionId: 'A', termId: TERM, success: true });
  check('live region stamped done+success on exit',
    !!live && live.classList.contains('done') && live.classList.contains('success'));
  const textAtExit = live.textContent;
  recv({ type: 'term_output', sessionId: 'A', termId: TERM, content: 'late\n' });
  check('late frame after exit is not mirrored', live.textContent === textAtExit);

  // A foreground card must NOT be kept in the bg mirror (its output is a
  // duplicate of the result and stops at the result).
  recv({ type: 'tool_call', sessionId: 'A', index: 0, toolCallId: 'tf1', tool: 'execute_command', args: { command: 'echo fg' } });
  recv({ type: 'term_opened', sessionId: 'A', termId: 'tf1', toolCallId: 'tf1', tool: 'execute_command', content: '$ echo fg' });
  recv({ type: 'term_exit', sessionId: 'A', termId: 'tf1', success: true });
  recv({ type: 'tool_result', sessionId: 'A', toolCallId: 'tf1', tool: 'execute_command', result: 'fg', success: true });
  // Its output stream is a duplicate of the result: the card must take the
  // streamed-body branch (copy button only, no expandable result text).
  const fgCard = doc.querySelectorAll('.tool-card')[1];
  const fgBody = fgCard && fgCard.querySelector('.tool-result-body');
  check('foreground card uses the streamed-body branch (no duplicate result)',
    !!fgBody && !!fgBody.querySelector('.tool-result-copy') && fgBody.textContent === 'OKCopy');

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => {
  console.error('HARNESS ERROR:', err && err.stack || err);
  process.exit(1);
});
