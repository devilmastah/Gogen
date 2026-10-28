'use strict';
// Regression checks for the subagent reasoning-effort picker (Agent
// settings tab). Loads the REAL index.html + app.js into jsdom (imports
// stripped, editor/marked/dompurify stubbed, no server needed) and asserts:
//   - the picker renders model-aware options: the selected subagent
//     model's catalog reasoningEfforts when a specific model is configured,
//     the active pane's model efforts while it is "Inherit", and the
//     default low/medium/high set for unknown models,
//   - the leading "Inherit" chip is the empty value (the parent session's
//     live level) and "Off" sends the explicit off,
//   - selections round-trip through configFields:["subagentThinkingLevel"],
//     including the server-push echo and the empty (inherit) clear,
//   - switching the subagent model to one that does not accept the stored
//     effort resets it to "Inherit" (and sends the clear),
//   - the picker's whole .setting-row hides while the subagent feature is off.
// Run: node scripts/test_subagent_thinking_picker.js
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

  const settingsBtn = document.getElementById('settings-btn');
  const settingsOverlay = document.getElementById('settings-overlay');
  const mainTabs = () => Array.from(document.querySelectorAll('.main-tab'));
  const openFromPane = (pane) => {
    mainTabs().forEach((t) => t.classList.toggle('active', t.dataset.pane === pane));
    settingsBtn.click();
  };

  // The feature gate hides the row that carries the label, not the inner block.
  const pickerRow = document.getElementById('row-subagent-thinking-picker');
  const optionsEl = document.getElementById('subagent-thinking-options');
  const modelList = document.getElementById('subagent-model-list');
  const chips = () => Array.from(optionsEl.querySelectorAll('.tb-thinking-chip'));
  const chip = (label) => chips().find((c) => c.textContent === label);
  const modelRows = () => Array.from(modelList.querySelectorAll('.tb-model-row'));
  const push = (extra) => window.applyServerConfig(Object.assign({
    board: 'off', subagent: 'on', subagentMaxDepth: 1,
    subagentModel: '', subagentThinkingLevel: '',
    // applyServerConfig overwrites the pane's efforts with the push value:
    // carry the pane model's set on every push so inherit-mode options
    // stay stable.
    reasoningEfforts: ['high', 'max'], model: 'gpt-4o',
  }, extra));
  const sentThinking = (value) => sent.find((s) =>
    s.includes('configFields') && s.includes('subagentThinkingLevel') &&
    s.includes(`"subagentThinkingLevel":${value === '' ? '""' : `"${value}"`}`));

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };

  // ── Visibility follows the subagent feature ──
  openFromPane('board'); // Agent tab
  push({ subagent: 'off' });
  // Hiding only the inner block left "Subagent reasoning effort" dangling
  // over empty space, so the gate hides the whole .setting-row.
  check('picker row hidden while subagents are disabled', pickerRow.hidden === true);
  push({ subagent: 'on' });
  check('picker row visible while subagents are enabled', pickerRow.hidden === false);

  // ── Inherit model: options follow the PANE model's efforts ──
  // The pane model accepts high/max (from the config push above).
  check('inherit chip rendered and active by default', !!chip('Inherit') && chip('Inherit').classList.contains('active'));
  check('off chip rendered', !!chip('Off'));
  check('inherit-mode options are the pane model efforts (High, Max)',
    !!chip('High') && !!chip('Max') && !chip('Low') && chips().length === 4);

  // Selecting sends the value through the runtime-config channel.
  chip('Max').click();
  check('clicking Max sends configFields subagentThinkingLevel "max"', !!sentThinking('max'));
  check('Max chip active after click', chip('Max').classList.contains('active'));
  check('Inherit chip un-highlighted after pick', !chip('Inherit').classList.contains('active'));

  // Server push echo (another tab) keeps the selection.
  push({ subagentThinkingLevel: 'max' });
  check('server push keeps the picked chip active', chip('Max').classList.contains('active'));

  // ── Specific model: options follow the CATALOG model's efforts ──
  window.handleModels({
    models: [
      { id: 'gpt-4o', provider: 'default', reasoningEfforts: ['low', 'medium', 'high'] },
      { id: 'glm-5.2', provider: 'default', reasoningEfforts: ['high', 'max'] },
      { id: 'tiny-1', provider: 'default', reasoningEfforts: ['low'] },
      { id: 'local-unknown', provider: 'local' },
    ],
    model: 'gpt-4o',
  });
  modelRows().find((r) => r.textContent.startsWith('glm-5.2')).click();
  check('options follow the selected model (High, Max)',
    !!chip('High') && !!chip('Max') && !chip('Low') && chips().length === 4);
  check('stored "max" survives a model change that still accepts it', chip('Max').classList.contains('active'));

  // A model that does NOT accept the stored effort: reset to Inherit and
  // send the clear (the stored level must always match the options shown).
  modelRows().find((r) => r.textContent.startsWith('tiny-1')).click();
  check('stale effort reset to Inherit on incompatible model change', chip('Inherit').classList.contains('active'));
  check('reset sends the empty (inherit) value', !!sentThinking(''));
  check('tiny-1 options are its own set (Low only)', !!chip('Low') && !chip('High') && chips().length === 3);

  // Unknown model (no catalog efforts): the default low/medium/high set.
  modelRows().find((r) => r.textContent.startsWith('local-unknown')).click();
  check('unknown model falls back to the default set (Low, Medium, High)',
    !!chip('Low') && !!chip('Medium') && !!chip('High') && chips().length === 5);

  // ── Off and Inherit selections ──
  chip('Off').click();
  check('clicking Off sends the explicit off value', !!sentThinking('off'));
  check('Off chip active', chip('Off').classList.contains('active'));
  chip('Inherit').click();
  check('clicking Inherit sends the empty value', !!sentThinking(''));
  check('Inherit chip active again', chip('Inherit').classList.contains('active'));

  // Empty push (clear by another tab) re-highlights Inherit. The pushes
  // carry a specific subagent model (gpt-4o: low/medium/high) so the
  // Medium chip exists in the pushed state.
  push({ subagentModel: 'gpt-4o', subagentThinkingLevel: 'medium' });
  check('pushed level highlights its chip', chip('Medium').classList.contains('active'));
  push({ subagentModel: 'gpt-4o', subagentThinkingLevel: '' });
  check('empty push re-highlights Inherit', chip('Inherit').classList.contains('active'));

  if (failures > 0) {
    console.error(`test_subagent_thinking_picker: ${failures} FAILURE(S)`);
    process.exit(1);
  }
  console.log('test_subagent_thinking_picker: all checks passed');
  process.exit(0);
}

main().catch((err) => {
  console.error('test_subagent_thinking_picker:', err);
  process.exit(1);
});
