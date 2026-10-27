'use strict';
// Regression test for the toast stack UX: a close affordance, de-duplication
// of repeated identical toasts, a bounded stack, and hover/focus PAUSE of the
// auto-dismiss countdown (plus the slide-out exit class) — so an error toast
// cannot vanish mid-read or mid-tab.
//
// Timers are stubbed AFTER app.js loads (init uses real timers), so the
// toast scheduler's setTimeout/clearTimeout are captured and driven
// deterministically without waiting real seconds.
// Run: node scripts/test_toast.js
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
  window.WebSocket = class {
    constructor() { this.readyState = 0; }
    static OPEN = 1;
    send() {} close() {}
  };

  installEditorStubs(window);
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };

  loadAppJs(window);

  // Deterministic timer scheduler installed after load.
  const timers = new Map();
  let seq = 0;
  window.setTimeout = (fn, ms) => { const id = ++seq; timers.set(id, { fn, ms: Number(ms) || 0 }); return id; };
  window.clearTimeout = (id) => { timers.delete(id); };
  const drain = (ms) => { for (const [id, t] of [...timers]) if (t.ms === ms) { timers.delete(id); t.fn(); } };

  const host = document.getElementById('toast-host');
  const clear = () => { while (host.firstChild) host.removeChild(host.firstChild); };

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };

  // ── Append + close affordance ──
  window.showToast('Hello', 'info');
  check('toast appended', host.children.length === 1);
  const t1 = host.children[0];
  check('toast shows the message', t1.textContent.includes('Hello'));
  const closer = t1.querySelector('.toast-close');
  check('toast has a close button', !!closer);
  check('close button is labelled', closer && closer.getAttribute('aria-label') === 'Dismiss');
  check('info toast schedules a 3000ms dismiss', timers.has(t1._toastTimer) && timers.get(t1._toastTimer).ms === 3000);

  // ── De-dupe: an identical toast refreshes instead of stacking ──
  window.showToast('Hello', 'info');
  check('identical toast de-dupes', host.children.length === 1);
  // A distinct message stacks.
  window.showToast('Other', 'success');
  check('distinct toast stacks', host.children.length === 2);

  // ── Pause on hover: clears the timer; leave reschedules ──
  t1.dispatchEvent(new window.Event('mouseenter'));
  check('hover clears the auto-dismiss timer', !timers.has(t1._toastTimer));
  t1.dispatchEvent(new window.Event('mouseleave'));
  check('leave reschedules the countdown', timers.has(t1._toastTimer) && timers.get(t1._toastTimer).ms > 0);

  // ── Pause on focus (keyboard users tabbing into the close button) ──
  // focusin/focusout bubble, so a real focus on the close button reaches the
  // toast's listener; jsdom Events default to non-bubbling, so opt in.
  closer.dispatchEvent(new window.Event('focusin', { bubbles: true }));
  check('focusin clears the timer', !timers.has(t1._toastTimer));
  closer.dispatchEvent(new window.Event('focusout', { bubbles: true }));
  check('focusout reschedules', timers.has(t1._toastTimer));

  // ── Close button adds the exit class; fallback timer removes the node ──
  const t2 = host.children[1];
  t2.querySelector('.toast-close').dispatchEvent(new window.Event('click'));
  check('close adds the exit class', t2.classList.contains('toast-out'));
  drain(400);
  check('exit fallback removes the node', host.children.length === 1);

  // ── Clicking the toast body also dismisses ──
  const t1b = host.children[0];
  t1b.querySelector('.toast-text').dispatchEvent(new window.Event('click', { bubbles: true }));
  check('body click dismisses', t1b.classList.contains('toast-out'));

  // ── Bounded stack ──
  clear();
  for (let i = 0; i < 9; i++) window.showToast('m' + i, 'info');
  check('stack is bounded at 8', host.children.length === 8);
  check('oldest toast evicted', ![...host.children].some((el) => el.dataset.message === 'm0'));

  // ── Eviction must shed a node that is mid-exit ──
  // A dismissing toast is still in the DOM (awaiting its exit animation);
  // eviction must remove it anyway, or the bounded-stack loop can never
  // reduce the child count.
  clear();
  for (let i = 0; i < 8; i++) window.showToast('e' + i, 'info');
  const exiting = host.children[0];
  exiting.querySelector('.toast-close').dispatchEvent(new window.Event('click'));
  check('dismissing toast stays until its exit completes', host.children.length === 8 && exiting.classList.contains('toast-out'));
  window.showToast('e8', 'info');
  check('eviction sheds a mid-exit toast', host.children.length === 8 && !host.contains(exiting));

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
