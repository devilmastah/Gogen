'use strict';
// Config frames must not do work the frame did not ask for.
//
// A `config` message arrives on every attach, model switch, mode/thinking
// change, feature-toggle broadcast and provider save. Two things used to run
// on EVERY one of them regardless of whether anything changed:
//
//   1. refreshExplorer() while the editor pane was active — a directory walk
//      plus a git-status round trip (the comment said "when working dir
//      changes"; the code had no comparison), and
//   2. a full rebuild of the toolbar model list (hundreds of rows with fresh
//      listeners) even when the selected model did not move.
//
// This test pins the change-detection on both, and pins that the list still
// renders whenever it must (open, filter, catalog, selection move), so a
// stale list can never be shown.
//
// Run: node scripts/test_config_frame_side_effects.js
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
  window.WebSocket = class { constructor() { this.readyState = 1; } static OPEN = 1; send() {} close() {} };

  installEditorStubs(window);
  window.openModal = (o) => o.classList.add('active');
  window.closeModal = (o) => o.classList.remove('active');
  window.marked = { use() {}, parse(t) { return String(t == null ? '' : t); } };
  window.DOMPurify = { sanitize: (raw) => raw };

  // Count explorer refreshes and toolbar-list renders. Both are app.js
  // globals (the harness evals app.js as a classic script), and both are
  // referenced by name at call time, so wrapping them here is what the app
  // actually calls.
  let explorerRefreshes = 0;
  window.refreshExplorer = () => { explorerRefreshes++; return Promise.resolve(); };
  let listRenders = 0;
  const realRender = window.renderToolbarModelList;
  window.renderToolbarModelList = function (...args) { listRenders++; return realRender.apply(this, args); };

  loadAppJs(window);
  // loadAppJs re-evals app.js, which redefines the two globals: wrap again.
  explorerRefreshes = 0;
  window.refreshExplorer = () => { explorerRefreshes++; return Promise.resolve(); };
  const renderAfterLoad = window.renderToolbarModelList;
  listRenders = 0;
  window.renderToolbarModelList = function (...args) { listRenders++; return renderAfterLoad.apply(this, args); };

  let failures = 0;
  const check = (desc, ok, detail) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}${detail ? ' — ' + detail : ''}`);
    if (!ok) failures++;
  };

  const editorPane = doc.getElementById('editor-pane');
  editorPane.classList.add('active');

  // ── 1. Explorer refresh follows the working dir, not the frame count ──
  window.applyServerConfig({ workingDir: '/home/x/proj', model: 'm1', modelProvider: 'default' });
  check('first working dir populates the explorer once', explorerRefreshes === 1, String(explorerRefreshes));
  for (let i = 0; i < 4; i++) {
    window.applyServerConfig({ workingDir: '/home/x/proj', model: 'm1', modelProvider: 'default', mode: 'plan' });
  }
  check('unchanged working dir does not refresh the explorer again',
    explorerRefreshes === 1, String(explorerRefreshes));
  window.applyServerConfig({ workingDir: '/home/x/other', model: 'm1', modelProvider: 'default' });
  check('a changed working dir refreshes the explorer', explorerRefreshes === 2, String(explorerRefreshes));
  // Inactive editor pane: nothing to refresh.
  editorPane.classList.remove('active');
  window.applyServerConfig({ workingDir: '/home/x/third', model: 'm1', modelProvider: 'default' });
  check('no refresh while the editor pane is inactive', explorerRefreshes === 2, String(explorerRefreshes));
  editorPane.classList.add('active');

  // ── 2. The toolbar list is rebuilt only when the selection moves ──
  window.handleModels({
    models: [
      { id: 'm1', provider: 'default' },
      { id: 'm2', provider: 'default' },
      { id: 'm3', provider: 'acme' },
    ],
    model: 'm1',
    modelProvider: 'default',
  });
  const rendersAfterCatalog = listRenders;
  check('the catalog reply renders the list once', rendersAfterCatalog >= 1, String(rendersAfterCatalog));

  for (let i = 0; i < 5; i++) {
    window.applyServerConfig({ workingDir: '/home/x/third', model: 'm1', modelProvider: 'default', mode: 'plan' });
  }
  check('config frames with an unchanged model do not rebuild the list',
    listRenders === rendersAfterCatalog, `${rendersAfterCatalog} → ${listRenders}`);

  const before = listRenders;
  window.applyServerConfig({ model: 'm2', modelProvider: 'default' });
  check('a model switch rebuilds the list', listRenders === before + 1, `${before} → ${listRenders}`);
  const activeRow = () => Array.from(doc.querySelectorAll('#tb-model-list .tb-model-row')).find((r) => r.classList.contains('active'));
  check('the rebuilt list checks the new model', !!activeRow() && activeRow().textContent.startsWith('m2'));

  const beforeClear = listRenders;
  window.applyServerConfig({ model: '' });
  check('clearing the model rebuilds the list once', listRenders === beforeClear + 1, `${beforeClear} → ${listRenders}`);
  check('a cleared model checks no row',
    !Array.from(doc.querySelectorAll('#tb-model-list .tb-model-row')).some((r) => r.classList.contains('active')));
  window.applyServerConfig({ model: '' });
  check('repeating the cleared push does not rebuild', listRenders === beforeClear + 1, String(listRenders));

  // ── 3. The list still renders whenever it must ──
  const beforeOpen = listRenders;
  doc.getElementById('tb-model-btn').click();
  check('opening the popover re-renders the list', listRenders === beforeOpen + 1, `${beforeOpen} → ${listRenders}`);
  const filter = doc.getElementById('tb-model-filter');
  filter.value = 'm3';
  filter.dispatchEvent(new window.Event('input', { bubbles: true }));
  check('typing in the filter re-renders the list', listRenders === beforeOpen + 2, String(listRenders));
  check('the filtered list shows only the match',
    Array.from(doc.querySelectorAll('#tb-model-list .tb-model-row')).every((r) => r.textContent.includes('m3')));

  console.log(failures === 0 ? '\ntest_config_frame_side_effects: all checks passed' : `\n${failures} check(s) failed`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((e) => { console.error(e); process.exit(1); });
