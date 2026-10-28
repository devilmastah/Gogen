'use strict';
// A config push must never discard what the user is typing.
//
// publishConfig sends the WHOLE runtime-config block to every attached client
// (another tab, another pane, a background agent, a feature toggle), and
// applyRuntimeSettings used to assign every control from it — so a push that
// landed while a draft was in the System prompt textarea (or any server-backed
// text/number field) silently replaced the draft with the stored value.
//
// The fix skips a control that has focus AND a value the server does not have
// yet; the edit is still sent on `change` (blur/Enter) and the echo after that
// is what syncs the field, so nothing is lost. This test pins both halves:
// drafts survive, and everything else still syncs.
//
// Note: jsdom refuses to focus an element inside a `hidden` subtree, so each
// field is focused after its settings tab is opened (as in a real browser).
//
// Run: node scripts/test_settings_live_push.js
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

  loadAppJs(window);
  doc.getElementById('settings-btn').click();

  let failures = 0;
  const check = (desc, ok, detail) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}${detail ? ' — ' + detail : ''}`);
    if (!ok) failures++;
  };
  // Open a settings tab (a hidden panel cannot hold focus in jsdom) and focus
  // a control in it.
  const focusIn = (tab, id) => {
    const btn = doc.querySelector('.settings-tab-btn[data-tab="' + tab + '"]');
    if (btn) btn.click();
    const el = doc.getElementById(id);
    el.focus();
    check('#' + id + ' is focused', doc.activeElement === el);
    return el;
  };
  // Type into a control the way a browser does: set the value AND fire
  // `input` (that event, not the programmatic assignment, is what marks the
  // control as holding an uncommitted edit).
  const type = (el, value) => {
    el.value = value;
    el.dispatchEvent(new window.Event('input', { bubbles: true }));
  };
  // Commit an edit like a browser does on blur/Enter.
  const commit = (el) => {
    el.blur();
    el.dispatchEvent(new window.Event('change', { bubbles: true }));
  };

  // ── 1. A focused textarea keeps its draft ──
  const sys = focusIn('agent', 'system-prompt-input');
  type(sys, 'MY UNSAVED DRAFT');
  window.applyServerConfig({ systemPrompt: 'server value' });
  check('a focused textarea keeps the draft across a config push',
    sys.value === 'MY UNSAVED DRAFT', JSON.stringify(sys.value));

  // ── 2. The same field syncs once it is no longer being edited ──
  commit(sys);
  window.applyServerConfig({ systemPrompt: 'server value' });
  check('the same field syncs after blur (no permanent desync)',
    sys.value === 'server value', JSON.stringify(sys.value));

  // ── 3. A focused text input keeps its draft too ──
  const testCmd = focusIn('project', 'test-command-input');
  type(testCmd, 'make quick-test');
  window.applyServerConfig({ testCommand: 'make test' });
  check('a focused text input keeps the draft', testCmd.value === 'make quick-test', JSON.stringify(testCmd.value));
  commit(testCmd);

  // ── 4. A focused NUMBER field keeps its draft (same class) ──
  const depth = focusIn('subagents', 'subagent-depth-input');
  type(depth, '7');
  window.applyServerConfig({ subagentMaxDepth: 2 });
  check('a focused number field keeps the draft', depth.value === '7', depth.value);
  commit(depth);

  // ── 5. Focused but UNMODIFIED fields still sync (no stale value) ──
  const lint = focusIn('project', 'lint-command-input');
  window.applyServerConfig({ lintCommand: 'make vet' });
  check('a focused, unmodified field still syncs', lint.value === 'make vet', JSON.stringify(lint.value));
  lint.blur();

  // ── 6. Selects cannot hold a draft: they always follow the push ──
  const skills = focusIn('tools', 'skills-enabled-select');
  skills.value = 'off';
  window.applyServerConfig({ skills: 'on' });
  check('a focused select still follows the push', skills.value === 'on', skills.value);
  skills.blur();

  // ── 7. The secret placeholders still refresh while a draft is kept ──
  const keyInput = focusIn('tools', 'web-search-api-key-input');
  keyInput.value = 'sk-draft';
  window.applyServerConfig({ webSearchApiKeySet: true, systemPrompt: 'x' });
  check('the draft secret value is kept', keyInput.value === 'sk-draft', keyInput.value);
  check('the secret placeholder still refreshes',
    keyInput.placeholder.includes('blank keeps stored'), keyInput.placeholder);
  keyInput.blur();

  console.log(failures === 0 ? '\ntest_settings_live_push: all checks passed' : `\n${failures} check(s) failed`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((e) => { console.error(e); process.exit(1); });
