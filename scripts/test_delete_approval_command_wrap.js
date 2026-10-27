'use strict';
// Regression test for the blocked-command text in the approval dialog.
//
// The dialog is small (max 480px) but a blocked command is whatever the
// model wrote — a long find/rm pipeline or a base64 blob is ONE line with no
// break opportunity. #delete-approval-command was a bare <pre>, so it kept
// the browser's white-space: pre and grew the dialog sideways: the command
// spilled past the modal edge (and the whole dialog off a narrow window).
//
// Drives the REAL command_approval path (index.html + styles.css + app.js +
// components/*.js via scripts/web-harness.js) and pins the layout contract
// that keeps the command inside the dialog: it wraps anywhere and scrolls
// inside a capped box, and the dialog itself may shrink (min-width: 0 — a
// flex item's automatic minimum size would otherwise be the min-content
// width of that unbreakable line).
//
// Run: node scripts/test_delete_approval_command_wrap.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');

// One line, no whitespace: only break-anywhere wrapping keeps it inside.
const LONG_COMMAND = 'git log --oneline ' + 'a'.repeat(400);

async function main() {
  const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');
  const css = fs.readFileSync(path.join(ROOT, 'internal/server/web/styles.css'), 'utf8');
  // Inline the real stylesheet so getComputedStyle reports the shipped rules.
  const dom = new JSDOM(html.replace('</head>', `<style>${css}</style></head>`), {
    url: 'http://localhost/',
    runScripts: 'dangerously',
    pretendToBeVisual: true,
  });
  const { window } = dom;
  const doc = window.document;

  window.matchMedia = window.matchMedia || (() => ({ matches: false, addEventListener() {}, removeEventListener() {} }));
  window.Notification = { permission: 'denied', requestPermission: () => Promise.resolve('denied') };
  window.navigator.clipboard = { writeText: async () => {} };
  window.HTMLElement.prototype.scrollIntoView = function () {};
  window.Terminal = class {
    constructor() { this._cbs = {}; }
    open() {} write() {} fit() {}
    onData(cb) { this._cbs.data = cb; }
    onResize() { return { dispose() {} }; }
    dispose() {} reset() {} focus() {}
  };
  window.FitAddon = class { fit() {} };

  const sockets = [];
  class FakeWS {
    constructor() { this.readyState = 0; this.sent = []; sockets.push(this); }
    static OPEN = 1;
    send(msg) { this.sent.push(JSON.parse(msg)); }
    close() { this.readyState = 3; }
  }
  window.WebSocket = FakeWS;

  installEditorStubs(window);
  // Real-ish modal show/hide: the overlay carries .active while open (the
  // harness stub is a no-op, so set these AFTER installEditorStubs).
  window.openModal = (overlay) => overlay.classList.add('active');
  window.closeModal = (overlay) => overlay.classList.remove('active');
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };

  loadAppJs(window);

  const ws = sockets[0];
  if (!ws) throw new Error('no socket constructed');
  const recv = (obj) => ws.onmessage({ data: JSON.stringify(obj) });

  let failures = 0;
  const check = (desc, ok, detail) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}${ok || detail === undefined ? '' : ' — ' + detail}`);
    if (!ok) failures++;
  };

  ws.readyState = FakeWS.OPEN;
  ws.onopen();

  // A blocked command escalates to the user as a command_approval frame.
  recv({
    type: 'command_approval',
    approvalId: 'c1',
    sessionId: 'A',
    reason: 'rm -rf',
    command: LONG_COMMAND,
  });

  const overlay = doc.getElementById('delete-approval-overlay');
  const command = doc.getElementById('delete-approval-command');
  const dialog = doc.getElementById('delete-approval-dialog');

  check('command approval opens the approval dialog', overlay.classList.contains('active'));
  check('the delete-paths block is hidden for a command approval', doc.getElementById('delete-approval-paths').style.display === 'none');
  check('the full command is rendered (not truncated)', command.textContent === LONG_COMMAND);

  const cmdStyle = window.getComputedStyle(command);
  const dlgStyle = window.getComputedStyle(dialog);

  // ── The command wraps instead of running past the dialog edge ──
  check(
    'command wraps at pre-wrap (not the <pre> default of pre)',
    cmdStyle.whiteSpace === 'pre-wrap',
    cmdStyle.whiteSpace
  );
  check(
    'command breaks unbreakable runs (overflow-wrap: anywhere)',
    cmdStyle.overflowWrap === 'anywhere' || cmdStyle.wordBreak === 'break-word',
    `overflow-wrap: ${cmdStyle.overflowWrap}, word-break: ${cmdStyle.wordBreak}`
  );
  check(
    'command text is not clipped sideways (no overflow-x: hidden)',
    cmdStyle.overflowX !== 'hidden',
    cmdStyle.overflowX
  );

  // ── A very long command scrolls inside a capped box, so the tall block
  //    cannot push the Allow/Deny row out of view ──
  check(
    'command block caps its height',
    /^\d/.test(cmdStyle.maxHeight) && parseFloat(cmdStyle.maxHeight) > 0,
    cmdStyle.maxHeight
  );
  check(
    'command block scrolls vertically when capped',
    cmdStyle.overflowY === 'auto' || cmdStyle.overflowY === 'scroll',
    cmdStyle.overflowY
  );

  // ── The dialog keeps its width and stays on screen ──
  check('dialog may shrink below its content (min-width: 0)', dlgStyle.minWidth === '0px', dlgStyle.minWidth);
  check(
    'dialog caps its height and scrolls as a whole',
    /^\d/.test(dlgStyle.maxHeight) && (dlgStyle.overflowY === 'auto' || dlgStyle.overflowY === 'scroll'),
    `max-height: ${dlgStyle.maxHeight}, overflow-y: ${dlgStyle.overflowY}`
  );

  // ── A pending command approval still blocks the composer ──
  check('composer is disabled while the approval is pending', doc.getElementById('message-input').disabled);

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
