'use strict';
// Regression test for the MCP settings tab's server-list editor.
//
// The MCP tab used to be read-only ("mcp_servers in the config file"): the
// list could be tested but not edited, so every add/remove meant hand-editing
// the config file. It now round-trips over mcp_save / mcp_delete. This test
// loads the REAL index.html + app.js (imports stripped, editor stubs) and
// asserts:
//   - a pushed mcpServers list renders one row per server,
//   - a HOST row (started by this process, GOGEN_MCP_EXTRA) has no Edit or
//     Delete button,
//   - Edit loads the row into the form WITHOUT its env (a blank env keeps
//     the stored values on save),
//   - Add / Save sends mcp_save with the typed command/args/env,
//   - Delete (after the confirm) sends mcp_delete,
//   - an empty name is refused client-side (no message sent).
//
// Run: node scripts/test_mcp_settings.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');

async function main() {
  const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');
  const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
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
  window.openModal = (overlay) => overlay.classList.add('active');
  window.closeModal = (overlay) => overlay.classList.remove('active');
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };
  // Delete asks for confirmation first.
  let confirmAnswer = true;
  window.confirm = () => confirmAnswer;

  loadAppJs(window);

  const ws = sockets[0];
  if (!ws) throw new Error('no socket constructed');
  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  const recv = (obj) => ws.onmessage({ data: JSON.stringify(obj) });
  const sentOfType = (type) => ws.sent.filter((m) => m.type === type);

  let failures = 0;
  const check = (desc, ok, detail) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}${ok || detail === undefined ? '' : ' — ' + detail}`);
    if (!ok) failures++;
  };

  // The server pushes the configured list: one editable row, one host row.
  recv({
    type: 'config',
    mcpServers: [
      { name: 'fetch', command: 'npx', args: ['-y', 'server-fetch'], envSet: true },
      { name: 'host-only', command: 'hh', host: true },
    ],
  });

  const list = doc.getElementById('mcp-server-list');
  const rows = Array.from(list.querySelectorAll('.provider-row'));
  check('one row per pushed server', rows.length === 2, String(rows.length));
  const buttons = (row) => Array.from(row.querySelectorAll('.provider-row-actions button')).map((b) => b.textContent);
  check('editable row offers Test / Edit / Delete', buttons(rows[0]).join(',') === 'Test,Edit,Delete', buttons(rows[0]).join(','));
  // A host server (GOGEN_MCP_EXTRA) is owned by the host process: listed for
  // visibility, but there is nothing to test/edit/delete from here.
  check('host row offers no actions', buttons(rows[1]).length === 0, buttons(rows[1]).join(','));
  check('env values never reach the page (envSet only)', (list.textContent || '').includes('env •••'));

  // ── Edit loads the row into the form without the env ──
  rows[0].querySelectorAll('.provider-row-actions button')[1].click();
  check('Edit fills the name', doc.getElementById('mcp-test-name').value === 'fetch');
  check('Edit fills the command', doc.getElementById('mcp-test-command').value === 'npx');
  check('Edit fills the args', doc.getElementById('mcp-test-args').value === '-y server-fetch');
  check('Edit leaves the env blank (blank keeps stored)', doc.getElementById('mcp-test-env').value === '');

  // ── Add / Save sends mcp_save with the parsed form ──
  doc.getElementById('mcp-test-command').value = 'node';
  doc.getElementById('mcp-test-args').value = 'server.js  --flag';
  doc.getElementById('mcp-test-env').value = 'TOKEN=v1 DEBUG=1';
  doc.getElementById('mcp-save-btn').click();
  const saves = sentOfType('mcp_save');
  check('Add / Save sends mcp_save', saves.length === 1, String(saves.length));
  if (saves.length) {
    const op = saves[0].mcpOp || {};
    check('mcp_save carries the name', op.name === 'fetch', op.name);
    check('mcp_save carries the command', op.command === 'node', op.command);
    check('mcp_save splits the args', JSON.stringify(op.args) === JSON.stringify(['server.js', '--flag']), JSON.stringify(op.args));
    check('mcp_save parses the env', op.env && op.env.TOKEN === 'v1' && op.env.DEBUG === '1', JSON.stringify(op.env));
  }

  // ── An empty name is refused client-side ──
  doc.getElementById('mcp-test-name').value = '   ';
  const before = ws.sent.length;
  doc.getElementById('mcp-save-btn').click();
  check('empty name sends nothing', ws.sent.length === before, `${before} → ${ws.sent.length}`);
  check('empty name reports an error in the MCP note', (doc.getElementById('mcp-test-result').className || '').includes('error'));

  // ── Delete asks, then sends mcp_delete ──
  confirmAnswer = false;
  rows[0].querySelectorAll('.provider-row-actions button')[2].click();
  check('cancelled confirm sends nothing', sentOfType('mcp_delete').length === 0);
  confirmAnswer = true;
  rows[0].querySelectorAll('.provider-row-actions button')[2].click();
  const deletes = sentOfType('mcp_delete');
  check('Delete sends mcp_delete', deletes.length === 1, String(deletes.length));
  check('mcp_delete carries the name', deletes.length === 1 && deletes[0].mcpOp.name === 'fetch');

  // ── Test on a saved row probes it by name (server resolves the creds) ──
  rows[0].querySelectorAll('.provider-row-actions button')[0].click();
  const tests = sentOfType('test_mcp');
  check('Test sends test_mcp by name', tests.length === 1 && tests[0].mcpTest.name === 'fetch');

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
