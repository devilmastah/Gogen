'use strict';
// Regression test for the model picker's current-model check when two
// registered providers list the SAME model ID (deepseek + opencode both
// list deepseek-flash): selecting the deepseek copy must mark ONLY the
// deepseek row as current. The config push carries the owning profile
// (modelProvider) and the client derives m.current per (provider, id).
//
// The bug: applyServerConfig re-derived m.current from the BARE model id
// (m.id === data.model), so every config push (the set_model echo, the
// attach handshake, feature-flag broadcasts) checked BOTH rows — "the same
// model selected from 2 providers".
//
// Loads the real index.html + app.js into jsdom (imports stripped,
// editor/marked/dompurify stubbed) and asserts:
//   1. the list_models reply marks only the owning copy,
//   2. a config push (the set_model echo) keeps ONLY the owning copy
//      checked — the regression,
//   3. switching to the other provider's copy moves the check,
//   4. a default-profile model (no modelProvider field) still matches,
//   5. a cleared model (empty) checks no row.
//
// Run: node scripts/test_model_picker_current_flag.js
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

  // The collision catalog: deepseek-flash listed by BOTH the "deepseek"
  // and the "opencode" profile, plus a default-profile model.
  const catalog = [
    { id: 'gpt-4o', provider: 'default' },
    { id: 'deepseek-flash', provider: 'deepseek' },
    { id: 'deepseek-flash', provider: 'opencode' },
  ];

  // rows() maps each rendered row to { id, provider, active, check } by
  // walking the list in order and tracking the current group header.
  const rows = () => {
    const out = [];
    let group = 'default';
    for (const el of document.querySelectorAll('#tb-model-list > *')) {
      if (el.classList.contains('tb-model-group')) {
        group = el.textContent;
        continue;
      }
      const label = el.querySelector('span');
      out.push({
        id: label ? label.textContent : el.textContent,
        provider: group,
        active: el.classList.contains('active'),
        check: !!el.querySelector('.tb-check'),
      });
    }
    return out;
  };
  const activeRows = () => rows().filter((r) => r.active);

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };

  // 1. list_models reply: the server's per-(provider, id) flag marks only
  //    the owning copy.
  window.handleModels({ models: catalog, model: 'deepseek-flash', modelProvider: 'deepseek' });
  let active = activeRows();
  check('list_models marks exactly one row current', active.length === 1);
  check('list_models marks the DEEPSEEK copy',
    active.length === 1 && active[0].provider === 'deepseek' && active[0].id === 'deepseek-flash');

  // 2. THE regression: a config push (the set_model echo) re-derives the
  //    flags client-side. With the owning profile it must keep ONLY the
  //    deepseek copy checked — the old bare-id match checked both.
  window.applyServerConfig({ model: 'deepseek-flash', modelProvider: 'deepseek' });
  active = activeRows();
  check('config push keeps exactly one row current', active.length === 1);
  check('config push keeps the DEEPSEEK copy (not the opencode twin)',
    active.length === 1 && active[0].provider === 'deepseek');
  check('exactly one row carries the check icon', rows().filter((r) => r.check).length === 1);

  // 3. Switching to the opencode copy moves the check.
  window.applyServerConfig({ model: 'deepseek-flash', modelProvider: 'opencode' });
  active = activeRows();
  check('switching providers moves the check to the OPENCODE copy',
    active.length === 1 && active[0].provider === 'opencode');

  // 4. A default-profile model (no modelProvider field) still matches.
  window.applyServerConfig({ model: 'gpt-4o' });
  active = activeRows();
  check('default-profile model (no modelProvider) marks its row',
    active.length === 1 && active[0].provider === 'default' && active[0].id === 'gpt-4o');

  // 5. A cleared model (empty — validation dropped a stale restored model)
  //    checks no row: no stale checkmark on either twin.
  window.applyServerConfig({ model: '' });
  check('a cleared model checks no row', activeRows().length === 0);

  if (failures > 0) {
    console.error(`test_model_picker_current_flag: ${failures} FAILURE(S)`);
    process.exit(1);
  }
  console.log('test_model_picker_current_flag: all checks passed');
  process.exit(0);
}

main().catch((err) => {
  console.error('test_model_picker_current_flag:', err);
  process.exit(1);
});
