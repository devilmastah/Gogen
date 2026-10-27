'use strict';
// Regression test for the scroll-to-bottom button's dynamic label: the old
// static "New messages" text showed even when the user had simply scrolled up
// with no new content. The button must now read "Jump to latest" while the
// user is only reading scrolled-up history, and switch to "New messages" only
// once content has actually grown below the fold since they left the bottom
// (an appended message / streamed token / late colorize-image-font growth).
//
// Loads the real index.html + app.js + web/components/*.js into jsdom (imports
// stripped, editor/marked/dompurify stubbed). jsdom has no layout, so #messages'
// scroll metrics are faked with writable data properties (as in test_toc.js):
// the module-scope stickToBottom / baseline state is not reachable from the
// harness, so behavior is driven through the exported functions the UI uses
// (unpinFromBottom / disableFollow / smartScroll / pinToBottom /
// updateScrollBottomBtn).
//
// Run: node scripts/test_scroll_button_label.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
const VIEW_PX = 500;

// Data properties on the instance shadow jsdom's read-only prototype
// accessors, so the scroll machinery sees the geometry we set.
function fake(el, props) {
  for (const [k, v] of Object.entries(props)) {
    Object.defineProperty(el, k, { value: v, configurable: true, writable: true });
  }
  return el;
}

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
  window.WebSocket = class {
    constructor() { this.readyState = 0; }
    static OPEN = 1;
    send() {} close() {}
  };

  installEditorStubs(window);
  window.escapeHtml = (s) => String(s == null ? '' : s);
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };

  loadAppJs(window);

  const messagesDiv = document.getElementById('messages');
  const btn = document.getElementById('scroll-bottom-btn');
  const label = document.getElementById('scroll-bottom-btn-label');
  const vis = () => btn.classList.contains('visible');
  const text = () => label.textContent;

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };

  // Start following at the bottom: content 1400, viewport 500 → max scrollTop 900.
  fake(messagesDiv, { scrollHeight: 1400, clientHeight: VIEW_PX, scrollTop: 900 });
  window.enableFollow();
  window.updateScrollBottomBtn();
  check('following at bottom: button hidden', !vis());
  check('following at bottom: neutral label', text() === 'Jump to latest');

  // ── Plain scroll-up with no new content ──
  // User wheels up (eager unpin), landing away from the bottom.
  messagesDiv.scrollTop = 300;
  window.unpinFromBottom();
  check('scrolled up: button shows', vis());
  check('scrolled up without new content: "Jump to latest"', text() === 'Jump to latest');

  // ── New content arrives while detached ──
  // Appends/streamed tokens funnel through smartScroll; content grows below.
  fake(messagesDiv, { scrollHeight: 1900, clientHeight: VIEW_PX, scrollTop: 300 });
  window.smartScroll();
  check('new content below the fold: "New messages"', text() === 'New messages');
  check('new content below the fold: button still shows', vis());

  // ── Scrolling within the history must not clear the "New messages" state ──
  messagesDiv.scrollTop = 1000;
  window.updateScrollBottomBtn();
  check('scrolling while unseen content remains: still "New messages"', text() === 'New messages');

  // ── Re-pinning hides the button; the next detach starts neutral again ──
  window.pinToBottom();
  check('re-pinned: button hidden', !vis());
  messagesDiv.scrollTop = 1000;
  window.unpinFromBottom();
  check('fresh detach after re-pin: "Jump to latest"', text() === 'Jump to latest');

  // ── Pane-restore detach (disableFollow) also reads neutral ──
  window.enableFollow();
  fake(messagesDiv, { scrollHeight: 1900, clientHeight: VIEW_PX, scrollTop: 500 });
  window.disableFollow();
  check('history restore (disableFollow): "Jump to latest"', text() === 'Jump to latest');
  check('history restore: button shows', vis());

  // ── Growth after a restore, via the colorize/image/font funnel ──
  // scheduleRepinIfPinned runs in a rAF; drive one frame and let it latch.
  fake(messagesDiv, { scrollHeight: 2300, clientHeight: VIEW_PX, scrollTop: 500 });
  window.scheduleRepinIfPinned();
  await new Promise((r) => window.requestAnimationFrame(() => window.requestAnimationFrame(r)));
  check('late growth after restore: "New messages"', text() === 'New messages');

  // ── Growth while still following must stay neutral ──
  window.pinToBottom();
  fake(messagesDiv, { scrollHeight: 2600, clientHeight: VIEW_PX, scrollTop: 100 });
  window.updateScrollBottomBtn();
  check('pinned but content grew (d>threshold): button shows', vis());
  check('pinned: label stays "Jump to latest"', text() === 'Jump to latest');

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
