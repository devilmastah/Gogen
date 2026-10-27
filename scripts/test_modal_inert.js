// Regression test for modal accessibility (board #90): openModal must mark
// the overlay aria-modal and inert the app surface behind it (#top-tabs,
// #main-panes), and closeModal must undo both. The inert flag is tracked by
// a Set of open overlays, so STACKED modals only restore the background when
// the LAST one closes, and re-opening an already-open overlay does not
// double-count (a single close must still clear it).
//
// Loads the REAL editor.js into jsdom (module syntax stripped via
// scripts/web-harness.js) and drives openModal/closeModal directly.
//
// Run: node scripts/test_modal_inert.js
'use strict';

const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { ROOT, stripModuleSyntax, installStorage } = require('./web-harness');

let failures = 0;
function check(cond, msg) {
  if (cond) {
    console.log('  ok   ' + msg);
  } else {
    failures++;
    console.error('  FAIL ' + msg);
  }
}

// #top-tabs and #main-panes are the inert targets; #toast-host is a sibling
// that must stay live (aria-live announcements); the two overlays are
// siblings too, so they stay interactive while a modal is open.
const html = `<!doctype html><html><body>
  <div id="top-tabs"><button id="tabbtn">tab</button></div>
  <div id="main-panes"><button id="mainbtn">main</button></div>
  <div id="toast-host"></div>
  <div id="overlay-a" aria-hidden="true"><button id="a-ok">ok</button></div>
  <div id="overlay-b" aria-hidden="true"><button id="b-ok">ok</button></div>
</body></html>`;

const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously' });
const { window } = dom;
const doc = window.document;
window.DOMPurify = { sanitize: (h) => h }; // only used by the colorize paths, not exercised here
installStorage(window); // editor.js reads persisted prefs at top level
window.eval(stripModuleSyntax(fs.readFileSync(path.join(ROOT, 'internal/server/web/editor.js'), 'utf8')));

const { openModal, closeModal } = window;
check(typeof openModal === 'function' && typeof closeModal === 'function', 'openModal/closeModal loaded from real editor.js');
if (typeof openModal !== 'function') { process.exit(1); }

const A = doc.getElementById('overlay-a');
const B = doc.getElementById('overlay-b');
const topTabs = doc.getElementById('top-tabs');
const mainPanes = doc.getElementById('main-panes');
const toastHost = doc.getElementById('toast-host');

const inert = (el) => el.hasAttribute('inert');

// ── 1. Baseline: nothing is inert before any modal opens ──
console.log('baseline:');
check(!inert(topTabs) && !inert(mainPanes), 'no inert before any modal opens');

// ── 2. Opening a modal marks it and inerts the background ──
console.log('single modal:');
openModal(A);
check(A.getAttribute('aria-hidden') === 'false', 'open sets aria-hidden="false"');
check(A.getAttribute('aria-modal') === 'true', 'open sets aria-modal="true"');
check(A.classList.contains('active'), 'open adds the active class');
check(inert(topTabs) && inert(mainPanes), 'background (#top-tabs, #main-panes) becomes inert');
check(!inert(toastHost), 'toast host is not inert (stays a live region)');
check(!inert(A), 'the overlay itself is never inert');

// ── 3. Closing restores the background ──
console.log('close restores:');
closeModal(A);
check(A.getAttribute('aria-hidden') === 'true', 'close sets aria-hidden="true"');
check(!A.hasAttribute('aria-modal'), 'close removes aria-modal');
check(!A.classList.contains('active'), 'close removes the active class');
check(!inert(topTabs) && !inert(mainPanes), 'background inert is cleared after the last modal closes');

// ── 4. Stacked modals keep the background inert until the LAST closes ──
console.log('stacked modals:');
openModal(A);
openModal(B);
check(inert(topTabs) && inert(mainPanes), 'background stays inert with two modals open');
closeModal(B);
check(inert(topTabs) && inert(mainPanes), 'background stays inert after closing only the outer modal');
check(!B.hasAttribute('aria-modal'), 'closed inner modal loses aria-modal');
closeModal(A);
check(!inert(topTabs) && !inert(mainPanes), 'background restored once the last modal closes');

// ── 5. Re-opening an already-open overlay must not double-count ──
console.log('idempotent open:');
openModal(A);
openModal(A); // same overlay again — must not increment
closeModal(A);
check(!inert(topTabs) && !inert(mainPanes), 'open(A) twice + close(A) once clears the background');

// ── 6. closeModal on a never-opened overlay is a harmless no-op ──
console.log('close without open:');
closeModal(A);
check(!inert(topTabs) && !inert(mainPanes), 'closeModal on a closed overlay leaves the background live');

if (failures) {
  console.error('\n' + failures + ' check(s) FAILED');
  process.exit(1);
}
console.log('\nall modal-inert checks passed');
