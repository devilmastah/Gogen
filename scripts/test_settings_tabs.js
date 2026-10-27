'use strict';
// Regression checks for the settings-modal sidebar rework and the subagent
// model picker. Loads the REAL index.html + app.js into jsdom (imports
// stripped, editor/marked/dompurify stubbed, no server needed) and asserts:
//   - the settings modal shows ONE section at a time (sidebar tabs),
//   - opening settings auto-selects the tab matching the active screen
//     (chat -> Chat, editor -> Editor, board -> Agent) and falls back to
//     the last-used tab otherwise (terminal/unmatched),
//   - manual tab switches persist to localStorage,
//   - the subagent model picker (Agent tab) renders the toolbar-style
//     grouped model list with an "Inherit" row, follows the server-pushed
//     subagentModel value (including the empty "inherit" state), sends
//     configFields:["subagentModel"] on selection, and hides when the
//     subagent feature is off.
// Run: node scripts/test_settings_tabs.js
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
  // A WebSocket stub that records every sent message (OPEN so the app's
  // ws.readyState guards pass).
  const sent = [];
  window.WebSocket = class {
    constructor() { this.readyState = 1; }
    static OPEN = 1;
    send(data) { sent.push(data); }
    close() {}
  };
  window.HTMLElement.prototype.scrollIntoView = function () {};

  installEditorStubs(window);
  // Real-ish open/close so the overlay's .active class (which gates the
  // picker's re-render and the fetch-on-open) actually toggles.
  window.openModal = (overlay) => overlay.classList.add('active');
  window.closeModal = (overlay) => overlay.classList.remove('active');
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };

  loadAppJs(window);

  const settingsBtn = document.getElementById('settings-btn');
  const settingsOverlay = document.getElementById('settings-overlay');
  const sidebar = document.querySelector('.settings-sidebar');
  const mainTabs = () => Array.from(document.querySelectorAll('.main-tab'));
  const tabBtns = () => Array.from(document.querySelectorAll('.settings-tab-btn'));
  const panels = () => Array.from(document.querySelectorAll('.settings-group[role="tabpanel"]'));
  const visiblePanel = () => panels().find((p) => !p.hidden);
  const activeTabBtn = () => tabBtns().find((b) => b.classList.contains('active'));
  const picker = document.getElementById('subagent-model-picker');
  const pickerList = document.getElementById('subagent-model-list');
  const subagentSelect = document.getElementById('subagent-enabled-select');

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };
  const openFromPane = (pane) => {
    mainTabs().forEach((t) => t.classList.toggle('active', t.dataset.pane === pane));
    settingsBtn.click();
  };

  // ── Sidebar tabs: one visible panel at a time ──
  openFromPane('chat');
  check('opening settings from the chat screen selects the Chat tab', activeTabBtn() && activeTabBtn().dataset.tab === 'chat');
  check('exactly one settings panel is visible', panels().filter((p) => !p.hidden).length === 1);
  check('the visible panel is the Chat section', visiblePanel() && visiblePanel().id === 'settings-tab-chat');
  check('all other panels are hidden', panels().filter((p) => p.hidden).length === panels().length - 1);
  check('selected tab button carries aria-selected', activeTabBtn().getAttribute('aria-selected') === 'true');

  // ── Auto-open follows the active screen ──
  window.closeModal(settingsOverlay);
  openFromPane('editor');
  check('editor screen auto-opens the Editor tab', activeTabBtn() && activeTabBtn().dataset.tab === 'editor');
  check('Editor panel visible', visiblePanel() && visiblePanel().id === 'settings-tab-editor');

  window.closeModal(settingsOverlay);
  openFromPane('board');
  check('board screen auto-opens the Agent tab (board settings live there)', activeTabBtn() && activeTabBtn().dataset.tab === 'agent');
  check('Agent panel visible', visiblePanel() && visiblePanel().id === 'settings-tab-agent');

  // ── Manual tab switch persists to localStorage ──
  const providersBtn = tabBtns().find((b) => b.dataset.tab === 'providers');
  providersBtn.click();
  check('manual switch opens the Providers tab', activeTabBtn() && activeTabBtn().dataset.tab === 'providers');
  check('Providers panel visible', visiblePanel() && visiblePanel().id === 'settings-tab-providers');
  check('tab choice persisted', window.localStorage.getItem('gogen-settings-tab') === 'providers');

  // ── Unmatched screen falls back to last-used ──
  window.closeModal(settingsOverlay);
  openFromPane('terminal');
  check('terminal screen falls back to the last-used tab (providers)', activeTabBtn() && activeTabBtn().dataset.tab === 'providers');

  // ── MCP gets its own tab (moved out of Server) ──
  const mcpBtn = tabBtns().find((b) => b.dataset.tab === 'mcp');
  check('MCP tab button exists', !!mcpBtn);
  mcpBtn.click();
  check('MCP tab opens its own panel', visiblePanel() && visiblePanel().id === 'settings-tab-mcp');
  check('MCP select lives in the MCP panel', !!document.querySelector('#settings-tab-mcp #mcp-select'));
  check('MCP test form lives in the MCP panel', !!document.querySelector('#settings-tab-mcp #mcp-test-btn'));
  check('MCP server list container lives in the MCP panel', !!document.querySelector('#settings-tab-mcp #mcp-server-list'));
  check('Server panel no longer holds the MCP select', !document.querySelector('#settings-tab-server #mcp-select'));
  window.applyServerConfig({
    mcpServers: [{ name: 'fetch', command: 'npx', args: ['-y', '@modelcontextprotocol/server-fetch'], envSet: true }],
    mcp: 'on',
  });
  check('MCP server list renders inside the MCP panel', !!document.querySelector('#settings-tab-mcp .mcp-server-list .provider-row'));
  window.applyServerConfig({
    mcpServers: [
      { name: 'fetch', command: 'npx', args: ['-y', '@modelcontextprotocol/server-fetch'], envSet: true },
      { name: 'vscode', command: 'node', host: true },
    ],
    mcp: 'on',
  });
  const mcpRows = [...document.querySelectorAll('#settings-tab-mcp .mcp-server-list .provider-row')];
  const mcpRow = (name) => mcpRows.find((row) => row.textContent.includes(name));
  check('configured MCP row keeps Test', mcpRow('fetch') && mcpRow('fetch').querySelector('button') && mcpRow('fetch').querySelector('button').textContent === 'Test');
  check('host MCP row has no Test button', mcpRow('vscode') && !mcpRow('vscode').querySelector('button'));

  // ── Subagent model picker ──
  window.closeModal(settingsOverlay);
  openFromPane('board'); // Agent tab
  // Server push: subagents on, no default model yet.
  window.applyServerConfig({ board: 'off', subagent: 'on', subagentMaxDepth: 1, subagentModel: '' });
  check('picker is visible while subagents are enabled', picker && !picker.hidden);
  // Catalog reply (the toolbar shares this flow). The server marks the
  // pane's model with `current: true` — the picker must ignore that flag.
  window.handleModels({
    models: [
      { id: 'gpt-4o', provider: 'default', contextLimit: 128000, current: true },
      { id: 'llama3.1', provider: 'local', contextLimit: 8192 },
    ],
    model: 'gpt-4o',
  });
  const rows = () => Array.from(pickerList.querySelectorAll('.tb-model-row'));
  const inheritRow = () => rows().find((r) => r.textContent.includes('Inherit'));
  check('inherit row rendered', !!inheritRow());
  check('inherit row active when no default is configured', inheritRow().classList.contains('active'));
  check('model rows rendered (toolbar-style)', rows().length === 3); // inherit + 2 models
  check('provider group headers rendered', pickerList.querySelectorAll('.tb-model-group').length === 2);
  // The pane's model carries `current: true`; while inheriting it must NOT
  // be highlighted (only the inherit row is active) and clicking it must
  // select it (regression: the shared row renderer's m.current fallback
  // pinned the parent's model as active and swallowed the click).
  const paneModelRow = rows().find((r) => r.textContent.startsWith('gpt-4o'));
  check('pane model (current flag) not highlighted while inheriting',
    !paneModelRow.classList.contains('active'));
  paneModelRow.click();
  const panePickMsg = sent.find((s) => s.includes('configFields') && s.includes('"subagentModel":"gpt-4o"'));
  check('clicking the pane model selects it (sends subagentModel)', !!panePickMsg);
  check('pane model row highlighted after click', rows().find((r) => r.textContent.startsWith('gpt-4o')).classList.contains('active'));
  // Clear back to inherit so the original flow below starts from a clean slate.
  window.applyServerConfig({ board: 'off', subagent: 'on', subagentMaxDepth: 1, subagentModel: '' });
  check('clear back to inherit before the original flow', inheritRow().classList.contains('active'));

  const gpt4oRow = rows().find((r) => r.textContent.startsWith('gpt-4o'));
  gpt4oRow.click();
  const pickMsg = sent.find((s) => s.includes('configFields') && s.includes('subagentModel'));
  check('selection sends configFields:["subagentModel"] with the model id',
    !!pickMsg && pickMsg.includes('"subagentModel":"gpt-4o"'));
  check('picked row highlights immediately', rows().find((r) => r.textContent.startsWith('gpt-4o')).classList.contains('active'));
  check('inherit row un-highlighted after pick', !inheritRow().classList.contains('active'));

  // Server push confirms the selection (echo from another tab).
  window.applyServerConfig({ board: 'off', subagent: 'on', subagentMaxDepth: 1, subagentModel: 'gpt-4o' });
  check('server push keeps the picked row active', rows().find((r) => r.textContent.startsWith('gpt-4o')).classList.contains('active'));

  // Clearing back to inherit (empty push) re-highlights the inherit row.
  window.applyServerConfig({ board: 'off', subagent: 'on', subagentMaxDepth: 1, subagentModel: '' });
  check('empty push (clear) re-highlights inherit', inheritRow().classList.contains('active'));

  // Feature off hides the picker.
  window.applyServerConfig({ board: 'off', subagent: 'off', subagentMaxDepth: 1, subagentModel: '' });
  check('picker hidden while subagents are disabled', picker.hidden);

  // ── Max concurrent subagents input ──
  // Re-enable subagents (the picker section ended with the feature off).
  window.applyServerConfig({ board: 'off', subagent: 'on', subagentMaxDepth: 1, subagentModel: '', subagentMaxConcurrent: 4 });
  const limitInput = document.getElementById('subagent-limit-input');
  check('concurrent limit input exists', !!limitInput);
  check('server push populates the concurrent limit input', limitInput && limitInput.value === '4');
  limitInput.value = '2';
  limitInput.dispatchEvent(new window.Event('change', { bubbles: true }));
  const limitMsg = sent.find((s) => s.includes('"subagentMaxConcurrent":2'));
  check('changing the limit sends subagentMaxConcurrent', !!limitMsg);
  limitInput.value = '99';
  limitInput.dispatchEvent(new window.Event('change', { bubbles: true }));
  check('out-of-range limit clamped to 32', limitInput.value === '32');
  const clampedMsg = sent.find((s) => s.includes('"subagentMaxConcurrent":32'));
  check('clamped limit sent', !!clampedMsg);
  limitInput.value = '0';
  limitInput.dispatchEvent(new window.Event('change', { bubbles: true }));
  check('below-range limit reset to 1', limitInput.value === '1');

  // ── Schema-driven tab order + grouping ──
  // The sidebar/panels come from components/settings-schema.js (evaluated
  // by the harness as a global), so these invariants fail loudly if the
  // grouping drifts.
  const tabOrder = tabBtns().map((b) => b.dataset.tab);
  const expectedTabs = window.SETTINGS_TABS.map((t) => t.id);
  check('sidebar order matches SETTINGS_TABS', JSON.stringify(tabOrder) === JSON.stringify(expectedTabs));
  check('Appearance tab replaces the old Global tab', tabOrder.includes('appearance') && !tabOrder.includes('global'));
  check('Subagents tab exists (split out of Agent)', tabOrder.includes('subagents'));
  check('Agent tab keeps the board settings', !!document.querySelector('#settings-tab-agent #board-enabled-select'));
  check('subagent model picker lives in the Subagents tab', !!document.querySelector('#settings-tab-subagents #subagent-model-picker'));
  check('subagent model picker no longer in the Agent tab', !document.querySelector('#settings-tab-agent #subagent-model-picker'));
  check('Approval hold moved to Security', !!document.querySelector('#settings-tab-security #approval-hold-input'));
  check('Approval hold no longer in Sessions', !document.querySelector('#settings-tab-sessions #approval-hold-input'));
  check('Automations is its own tab', tabOrder.includes('automations'));
  check('Server is last (restart-only tab after the live tabs)', tabOrder[tabOrder.length - 1] === 'server');
  check('Providers precede MCP precede Server',
    tabOrder.indexOf('providers') < tabOrder.indexOf('mcp') && tabOrder.indexOf('mcp') < tabOrder.indexOf('server'));

  // ── Schema coverage: every entry rendered into its declared tab ──
  // A new schema entry that forgets its DOM (or points at the wrong tab)
  // is caught here rather than by eye.
  const schema = window.SETTINGS_SCHEMA;
  check('SETTINGS_SCHEMA is exposed to the harness', Array.isArray(schema) && schema.length > 0);
  const missing = schema.filter((e) => {
    const panel = document.getElementById('settings-tab-' + e.tab);
    if (!panel) return true;
    if (e.custom) {
      const node = document.getElementById(e.custom);
      return !node || !panel.contains(node);
    }
    const el = document.getElementById(e.id);
    return !el || !panel.contains(el);
  });
  check('every schema entry renders into its declared tab', missing.length === 0);
  if (missing.length > 0) console.warn('  missing:', missing.map((e) => e.id || e.custom).join(', '));

  if (failures > 0) {
    console.error(`test_settings_tabs: ${failures} FAILURE(S)`);
    process.exit(1);
  }
  console.log('test_settings_tabs: all checks passed');
  process.exit(0);
}

main().catch((err) => {
  console.error('test_settings_tabs:', err);
  process.exit(1);
});
