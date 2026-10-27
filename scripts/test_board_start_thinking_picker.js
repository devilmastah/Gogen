'use strict';
// Regression checks for the board "Start agent" popover's reasoning-effort
// picker. Loads the REAL index.html + app.js into jsdom (imports stripped,
// editor/marked/dompurify stubbed, no server needed) and asserts:
//   - the popover renders the effort chips: leading "Inherit" (the empty
//     value = the active pane's live level, the pre-existing behavior),
//     "Off", and the model's accepted values,
//   - the chips are MODEL-AWARE: the selected ticket model's catalog
//     reasoningEfforts, the pane model's efforts for "Workspace default",
//     the default low/medium/high set for unknown models,
//   - the ticket's stored thinkingLevel pre-fills the selection,
//   - switching the model to one that does not accept the stored effort
//     resets it to "Inherit" (the selection must always match the options
//     shown; the server re-validates at start time),
//   - the start op carries thinkingLevel in the board_op payload.
// Run: node scripts/test_board_start_thinking_picker.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');

async function main() {
  const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');
  const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;
  const { document } = window;

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
  const sent = [];
  window.WebSocket = class {
    constructor() { this.readyState = 1; }
    static OPEN = 1;
    send(data) { sent.push(data); }
    close() {}
  };
  window.HTMLElement.prototype.scrollIntoView = function () {};

  installEditorStubs(window);
  window.openModal = (overlay) => overlay.classList.add('active');
  window.closeModal = (overlay) => overlay.classList.remove('active');
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };

  loadAppJs(window);

  // The pane model accepts high/max (from the config push); the catalog
  // carries per-model efforts for the model-aware cases.
  window.applyServerConfig({
    board: 'on', subagent: 'off', subagentMaxDepth: 1,
    subagentModel: '', subagentThinkingLevel: '',
    reasoningEfforts: ['high', 'max'], model: 'gpt-4o',
  });
  window.handleModels({
    models: [
      { id: 'gpt-4o', provider: 'default', reasoningEfforts: ['low', 'medium', 'high'] },
      { id: 'glm-5.2', provider: 'default', reasoningEfforts: ['high', 'max'] },
      { id: 'tiny-1', provider: 'default', reasoningEfforts: ['low'] },
      { id: 'local-unknown', provider: 'local' },
    ],
    model: 'gpt-4o',
  });

  // The popover anchors to a card button; a detached-in-body dummy gives
  // a zero rect (the positioning math clamps, the checks never measure).
  const anchor = document.createElement('button');
  document.body.appendChild(anchor);
  const openPopover = (item) => window.openBoardStartPopover(item, anchor);
  const pop = () => document.getElementById('board-start-popover');
  const chips = () => Array.from(pop().querySelectorAll('.board-start-thinking-grid .tb-thinking-chip'));
  const chip = (label) => chips().find((c) => c.textContent === label);
  const modelRows = () => Array.from(pop().querySelectorAll('.board-start-model-list .tb-model-row'));
  const startOp = () => sent
    .map((s) => { try { return JSON.parse(s); } catch { return null; } })
    .find((m) => m && m.type === 'board_op' && m.boardOp && m.boardOp.action === 'start');
  const sentLevels = () => sent
    .map((s) => { try { return JSON.parse(s); } catch { return null; } })
    .filter((m) => m && m.type === 'board_op' && m.boardOp && m.boardOp.action === 'start')
    .map((m) => m.boardOp.thinkingLevel);

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };

  // ── Default open: Inherit active, pane-model efforts ──
  openPopover({ id: '1', title: 'Fix parser crash', model: '', prompt: '' });
  check('popover opens with the effort row', !!pop() && pop().classList.contains('open') && chips().length > 0);
  check('Inherit chip rendered and active by default (empty value)',
    !!chip('Inherit') && chip('Inherit').classList.contains('active'));
  check('Off chip rendered', !!chip('Off'));
  check('default-model options are the pane model efforts (High, Max)',
    !!chip('High') && !!chip('Max') && !chip('Low') && chips().length === 4);

  // Selecting a chip highlights it; Start sends it in the board_op.
  chip('High').click();
  check('clicking High activates the chip', chip('High').classList.contains('active'));
  pop().querySelector('.board-card-start').click();
  let op = startOp();
  check('start op carries thinkingLevel "high"', !!op && op.boardOp.thinkingLevel === 'high');

  // ── Stored ticket level pre-fills the selection ──
  openPopover({ id: '2', title: 'Second ticket', model: '', prompt: '', thinkingLevel: 'max' });
  check('stored ticket level pre-fills the Max chip', !!chip('Max') && chip('Max').classList.contains('active'));

  // ── Model-aware options + stale-effort reset ──
  // glm-5.2 accepts high/max: the stored max survives the switch.
  modelRows().find((r) => r.textContent.startsWith('glm-5.2')).click();
  check('options follow the selected model (High, Max)',
    !!chip('High') && !!chip('Max') && !chip('Low') && chips().length === 4);
  check('stored "max" survives a model change that still accepts it', chip('Max').classList.contains('active'));

  // tiny-1 accepts only low: the stored max is stale → reset to Inherit
  // (the server would reject it at start time; the UI never offers it).
  modelRows().find((r) => r.textContent.startsWith('tiny-1')).click();
  check('stale effort reset to Inherit on incompatible model change', chip('Inherit').classList.contains('active'));
  check('tiny-1 options are its own set (Low only)', !!chip('Low') && !chip('High') && chips().length === 3);

  // Unknown model (no catalog efforts): the default low/medium/high set.
  modelRows().find((r) => r.textContent.startsWith('local-unknown')).click();
  check('unknown model falls back to the default set (Low, Medium, High)',
    !!chip('Low') && !!chip('Medium') && !!chip('High') && chips().length === 5);

  // ── Off and Inherit selections reach the payload ──
  chip('Off').click();
  pop().querySelector('.board-card-start').click();
  check('start op carries the explicit "off"', sentLevels().includes('off'));
  openPopover({ id: '3', title: 'Third ticket', model: '', prompt: '' });
  pop().querySelector('.board-card-start').click();
  check('start op carries the empty (inherit) value', sentLevels().includes(''));

  if (failures > 0) {
    console.error(`test_board_start_thinking_picker: ${failures} FAILURE(S)`);
    process.exit(1);
  }
  console.log('test_board_start_thinking_picker: all checks passed');
  process.exit(0);
}

main().catch((err) => {
  console.error('test_board_start_thinking_picker:', err);
  process.exit(1);
});
