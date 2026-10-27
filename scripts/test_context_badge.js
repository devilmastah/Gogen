'use strict';
// Regression test for the toolbar context badge (item 1c):
//   - the badge must NOT set a native `title` (it would pop a second,
//     overlapping bubble ~1s after the custom #context-tooltip),
//   - the value exposed to assistive tech lives in `aria-label`,
//   - the badge must NOT be a live region (role="status" / aria-live):
//     its label is rewritten on every streaming context frame, which made a
//     screen reader announce the percentage repeatedly,
//   - the badge is focusable (tabindex="0") and the custom tooltip shows on
//     focus/blur as well as hover, so keyboard/touch users reach it.
//
// Loads the REAL index.html + app.js into jsdom (imports stripped, editor/
// marked/dompurify stubbed), adopts a session over a fake WebSocket, then
// drives real "context" frames through ws.onmessage.
// Run: node scripts/test_context_badge.js
// APPJS env var selects the app.js copy to test (defaults to the real one).
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

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };

  const ws = sockets[0];
  if (!ws) throw new Error('app.js did not construct a WebSocket');
  const recv = (obj) => ws.onmessage({ data: JSON.stringify(obj) });

  // Connect and adopt session A as the active pane.
  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  recv({
    type: 'config', sessionId: 'A', sessionLabel: 'session',
    mode: 'act', thinkingLevel: 'off', reasoningEfforts: [], model: 'm1',
    modelDescription: '', workingDir: '/tmp', globalMode: false,
  });

  const badge = document.getElementById('tb-context-badge');
  const tooltip = document.getElementById('context-tooltip');
  if (!badge || !tooltip) throw new Error('badge/tooltip not found');

  // ── Static markup: focusable, not a live region ──
  check('badge is focusable (tabindex="0")', badge.getAttribute('tabindex') === '0');
  check('badge is not role="status"', badge.getAttribute('role') !== 'status');
  check('badge is not an aria-live region', badge.getAttribute('aria-live') !== 'polite' && badge.getAttribute('aria-live') !== 'assertive');

  // Count aria-label attribute writes; a per-frame rewrite is what the fix
  // is meant to stop churning on.
  let ariaWrites = 0;
  const mo = new window.MutationObserver((records) => {
    for (const r of records) if (r.attributeName === 'aria-label') ariaWrites++;
  });
  mo.observe(badge, { attributes: true, attributeFilter: ['aria-label'] });

  // ── A real context frame drives the badge ──
  recv({ type: 'context', sessionId: 'A', usedTokens: 21000, contextLimit: 128000, messageCount: 3 });

  check('badge does NOT set a native title', !badge.hasAttribute('title') && !badge.title);
  const aria = badge.getAttribute('aria-label') || '';
  check('aria-label carries the percentage', aria.includes('16%'));
  check('aria-label carries the token counts', aria.includes('128,000'));
  check('visible label still shows the percentage', badge.textContent.includes('16%'));

  // Two more identical frames: the aria-label must not be rewritten.
  // MutationObserver callbacks are microtask-scheduled.
  mo.takeRecords();
  ariaWrites = 0;
  recv({ type: 'context', sessionId: 'A', usedTokens: 21000, contextLimit: 128000, messageCount: 3 });
  recv({ type: 'context', sessionId: 'A', usedTokens: 21000, contextLimit: 128000, messageCount: 3 });
  await Promise.resolve();
  await Promise.resolve();
  check('identical frames do not rewrite aria-label', ariaWrites === 0);

  // A changed value does update it.
  recv({ type: 'context', sessionId: 'A', usedTokens: 64000, contextLimit: 128000, messageCount: 4 });
  await Promise.resolve();
  await Promise.resolve();
  check('a changed frame updates aria-label', (badge.getAttribute('aria-label') || '').includes('50%'));

  // ── Tooltip: hover ──
  badge.dispatchEvent(new window.Event('mouseenter'));
  check('hover shows the custom tooltip', tooltip.style.display === 'block');
  check('tooltip has the detailed breakdown', tooltip.textContent.includes('Used:') && tooltip.textContent.includes('Limit:'));
  check('showing the tooltip does not create a native title', !badge.hasAttribute('title'));
  badge.dispatchEvent(new window.Event('mouseleave'));
  check('mouseleave hides the tooltip', tooltip.style.display === 'none');

  // ── Tooltip: focus (keyboard / touch) ──
  badge.dispatchEvent(new window.Event('focus'));
  check('focus shows the custom tooltip', tooltip.style.display === 'block');
  badge.dispatchEvent(new window.Event('blur'));
  check('blur hides the tooltip', tooltip.style.display === 'none');

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
