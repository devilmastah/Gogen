'use strict';
// Regression test: the status-display settings rows (host / token / row
// name) are meaningful only while the reporter is on, and the token input
// shows the "set" placeholder from the config push. The runtime config
// channel has no generic row gating (only the feature channel does), so
// applyRuntimeSettings carries this — a regression would leave the
// sub-fields visible while the feature is off, or drop the placeholder.
//
// Loads the real index.html + app.js + components into jsdom (imports
// stripped, editor/marked/dompurify stubbed) and drives applyRuntimeSettings
// with config pushes:
//   1. statusDisplay 'off' → host/token/source rows hidden
//   2. statusDisplay 'on'  → rows visible again
//   3. statusDisplayTokenSet true → token placeholder marks the stored secret
//   4. the schema wires all four controls (gate + sub-fields)
//
// Run: node scripts/test_status_display_settings.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');

function main() {
  const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');
  const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;
  const doc = window.document;

  window.matchMedia = window.matchMedia || (() => ({ matches: false, addEventListener() {}, removeEventListener() {} }));
  window.Notification = class {
    static permission = 'denied';
    static requestPermission() { return Promise.resolve('denied'); }
  };
  window.navigator.clipboard = { writeText: async () => {} };
  window.Terminal = class { open() {} write() {} fit() {} onData() {} onResize() { return { dispose() {} }; } dispose() {} reset() {} focus() {} };
  window.FitAddon = class { fit() {} };
  class FakeWS {
    constructor() { this.readyState = 0; this.sent = []; }
    static OPEN = 1;
    static CONNECTING = 0;
    send(msg) { this.sent.push(msg); }
    close() { this.readyState = 3; }
  }
  window.WebSocket = FakeWS;
  installEditorStubs(window);
  window.marked = { use() {}, parse: (t) => String(t == null ? '' : t) };
  window.DOMPurify = { sanitize: (raw) => raw };

  loadAppJs(window);

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };
  const row = (id) => doc.getElementById(id);
  const hidden = (id) => {
    const el = row(id);
    return el ? el.hidden : null;
  };

  // The schema is the only wiring: every control must exist, the gate
  // included.
  check('gate select rendered', !!doc.getElementById('status-display-select'));
  check('host input rendered', !!doc.getElementById('status-display-host-input'));
  check('token input rendered', !!doc.getElementById('status-display-token-input'));
  check('source input rendered', !!doc.getElementById('status-display-source-input'));
  check('host note names the mDNS default',
    (doc.getElementById('status-display-host-row').textContent || '').includes('statusscreen.local'));

  // 1. Feature off: the sub-field rows are hidden.
  window.applyRuntimeSettings({ statusDisplay: 'off' });
  check('off hides the host row', hidden('status-display-host-row') === true);
  check('off hides the token row', hidden('status-display-token-row') === true);
  check('off hides the source row', hidden('status-display-source-row') === true);

  // 2. Auto (the default) and on: visible — auto may probe and enable.
  window.applyRuntimeSettings({ statusDisplay: 'auto' });
  check('auto shows the host row', hidden('status-display-host-row') === false);
  check('auto shows the token row', hidden('status-display-token-row') === false);
  check('auto shows the source row', hidden('status-display-source-row') === false);

  window.applyRuntimeSettings({ statusDisplay: 'on' });
  check('on shows the host row', hidden('status-display-host-row') === false);
  check('on shows the token row', hidden('status-display-token-row') === false);
  check('on shows the source row', hidden('status-display-source-row') === false);

  // 3. A stored token is never echoed; the placeholder says so.
  window.applyRuntimeSettings({ statusDisplay: 'on', statusDisplayTokenSet: true });
  check('stored token placeholder', doc.getElementById('status-display-token-input').placeholder === 'Token set (blank keeps stored)');
  window.applyRuntimeSettings({ statusDisplay: 'on', statusDisplayTokenSet: false });
  check('cleared token placeholder', doc.getElementById('status-display-token-input').placeholder === 'Token');

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main();
