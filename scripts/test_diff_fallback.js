// Regression test for the incremental updateDiffFallback (board #73): the
// fallback <pre> inside a monaco-tool-host must be updated APPEND-ONLY while
// a patch diff streams in — existing .diff-row nodes keep their identity
// across deltas (only new rows are added), scroll behavior matches the old
// full-rebuild semantics (pinned-to-bottom stays pinned; a reader scrolled
// up keeps the same distance from the bottom), and a non-append rewrite
// (server re-sent a corrected diff) still triggers a full rebuild.
//
// Loads the REAL editor.js into jsdom (module syntax stripped via
// scripts/web-harness.js) and simulates <pre> layout (scrollHeight grows
// 16px per row, fixed 160px viewport) since jsdom does not lay out.
//
// Run: node scripts/test_diff_fallback.js
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

// ── Load the real editor.js into a jsdom window ──
// runScripts:'dangerously' makes window.eval run in the window context
// (same as the other jsdom harness tests).
const dom = new JSDOM('<!doctype html><html><body></body></html>', { url: 'http://localhost/', runScripts: 'dangerously' });
const { window } = dom;
window.DOMPurify = { sanitize: (h) => h }; // only used by colorize paths, not exercised here
installStorage(window); // editor.js reads persisted prefs at top level
window.eval(stripModuleSyntax(fs.readFileSync(path.join(ROOT, 'internal/server/web/editor.js'), 'utf8')));
const { updateDiffFallback, diffLineNumbers } = window;
check(typeof updateDiffFallback === 'function', 'updateDiffFallback loaded from real editor.js');

// ── Simulated <pre> layout: ROW_H px per row, fixed VIEW_H viewport ──
const ROW_H = 16;
const VIEW_H = 160;
function installLayout(pre) {
  let top = 0;
  Object.defineProperty(pre, 'scrollTop', {
    get: () => top,
    set: (v) => { top = v; },
    configurable: true,
  });
  Object.defineProperty(pre, 'scrollHeight', {
    get: () => pre.children.length * ROW_H,
    configurable: true,
  });
  Object.defineProperty(pre, 'clientHeight', {
    get: () => VIEW_H,
    configurable: true,
  });
  return {
    get top() { return top; },
    set top(v) { top = v; },
    pin() { top = Math.max(0, pre.scrollHeight - VIEW_H); },
  };
}

// Deterministic unified diff (hunk math is illustrative, not real).
function diffLines(n) {
  const L = ['diff --git a/f.go b/f.go', 'index 1234567..89abcde 100644', '--- a/f.go', '+++ b/f.go'];
  let i = 1;
  while (L.length < n) {
    L.push(`@@ -${i},8 +${i},10 @@`);
    for (let k = 0; k < 8 && L.length < n; k++) {
      L.push('  ctx ' + (i + k));
      if (L.length >= n) break;
      L.push('-old ' + (i + k));
      if (L.length >= n) break;
      L.push('+new ' + (i + k));
      i++;
    }
  }
  return L.slice(0, n);
}

function expectedRows(text) {
  const lines = text.split('\n');
  if (lines.length && lines[lines.length - 1] === '') lines.pop();
  return lines.length;
}

// ── 1. Streaming a 250-line diff: rows keep identity, only new rows added ──
console.log('streaming row identity:');
const N = 250;
const full = diffLines(N).join('\n');
const c1 = window.document.createElement('div');
window.document.body.appendChild(c1);
const prefixes = [];
for (let i = 1; i < full.length; i += 13) prefixes.push(full.slice(0, i)); // mid-line chunks exercise in-place last-line growth
prefixes.push(full);
const seen = [];
let identityOk = true;
let countOk = true;
for (const prefix of prefixes) {
  updateDiffFallback(c1, prefix);
  const pre = c1.querySelector('.diff-fallback');
  const want = expectedRows(prefix);
  if (pre.children.length !== want) countOk = false;
  for (let r = 0; r < seen.length; r++) {
    if (pre.children[r] !== seen[r]) { identityOk = false; break; }
  }
  while (seen.length < pre.children.length) seen.push(pre.children[seen.length]);
}
check(countOk, 'row count matches expected line count on every delta');
check(identityOk, 'existing .diff-row nodes keep their identity across all deltas (append-only)');
check(seen.length === N, 'final row count is ' + N);

// ── 2. DOM parity with a one-shot full render of the same text ──
console.log('parity with one-shot full render:');
const c2 = window.document.createElement('div');
window.document.body.appendChild(c2);
updateDiffFallback(c2, full); // fresh pre → full rebuild path
const refPre = c2.querySelector('.diff-fallback');
const incPre = c1.querySelector('.diff-fallback');
let parityOk = true;
let parityMsg = '';
for (let i = 0; i < N && parityOk; i++) {
  const a = incPre.children[i];
  const b = refPre.children[i];
  if (!b || a.innerHTML !== b.innerHTML) {
    parityOk = false;
    parityMsg = 'row ' + i + ': ' + a.outerHTML + ' vs ' + (b ? b.outerHTML : '<missing>');
  }
}
check(parityOk, 'incrementally built rows are byte-identical (gutter + classes) to a full render' + (parityOk ? '' : ' — ' + parityMsg));

// ── 3. Scroll behavior (simulated layout) ──
console.log('scroll behavior:');
const bigFull = diffLines(600).join('\n');
const c3 = window.document.createElement('div');
window.document.body.appendChild(c3);
const pre3 = window.document.createElement('pre');
pre3.className = 'diff-fallback';
c3.appendChild(pre3);
const layout = installLayout(pre3); // install BEFORE the first update so heights are simulated
updateDiffFallback(c3, bigFull.slice(0, 2000));

// Pinned to the bottom: new lines keep the view at the bottom.
layout.pin();
updateDiffFallback(c3, bigFull.slice(0, 4000));
check(pre3.scrollTop === Math.max(0, pre3.scrollHeight - VIEW_H),
  'pinned-to-bottom stays pinned after append (' + pre3.scrollHeight + 'px content)');

// Scrolled up into the diff: distance from the bottom is preserved (the old
// full-rebuild semantics), i.e. the view shifts by exactly the growth — no yank.
layout.top = 32;
const hBefore = pre3.scrollHeight;
updateDiffFallback(c3, bigFull);
const growth = pre3.scrollHeight - hBefore;
check(pre3.scrollTop === 32 + growth,
  'scrolled-up reader keeps distance from bottom (scrollTop 32 -> ' + pre3.scrollTop + ', growth ' + growth + ')');

// Identical text: no-op (no rebuild, no scroll change).
const tBefore = pre3.scrollTop;
const nBefore = pre3.children.length;
updateDiffFallback(c3, bigFull);
check(pre3.children.length === nBefore && pre3.scrollTop === tBefore, 'identical text is a no-op');

// ── 4. Non-append rewrite: full rebuild still works ──
console.log('non-append rewrite:');
const oldFirst = pre3.children[0];
const corrected = 'diff --git a/z.go b/z.go\nindex 1..2 100644\n--- a/z.go\n+++ b/z.go\n@@ -1 +1 @@\n-old\n+new\n';
updateDiffFallback(c3, corrected);
check(pre3.children.length === expectedRows(corrected), 'rebuilt row count matches corrected diff');
check(pre3.children[0] !== oldFirst, 'full rebuild replaced the existing rows');
// And streaming can resume incrementally from the rebuilt state.
updateDiffFallback(c3, corrected + ' context line\n');
check(pre3.children.length === expectedRows(corrected + ' context line\n'), 'streaming resumes incrementally after a rebuild');

// ── 5. In-place last-line growth: row count stable, class re-derived ──
console.log('in-place last-line growth:');
const c4 = window.document.createElement('div');
window.document.body.appendChild(c4);
updateDiffFallback(c4, 'diff --git a/x b/x\nindex 1..2 100644\nind');
const pre4 = c4.querySelector('.diff-fallback');
const lastRow = pre4.lastElementChild;
const lastCode = lastRow.lastElementChild;
check(pre4.children.length === 3, 'partial last line renders as one row');
check(!lastCode.classList.contains('gogen-diff-meta'), "'ind' has no diff class yet");
updateDiffFallback(c4, 'diff --git a/x b/x\nindex 1..2 100644\nindex 1234');
check(pre4.lastElementChild === lastRow, 'grown last line updates the same row in place');
check(pre4.children.length === 3, 'no extra rows for in-place growth');
check(pre4.lastElementChild.lastElementChild.classList.contains('gogen-diff-meta'),
  'class re-derived for the grown line (index ... -> gogen-diff-meta)');

// ── 6. diffLineNumbers: incremental scan matches the exported full scan ──
console.log('diffLineNumbers:');
const numsFull = diffLineNumbers(full);
let numsOk = true;
for (let i = 0; i < N && numsOk; i++) {
  if (incPre.children[i].firstElementChild.textContent !== (numsFull[i] || '')) numsOk = false;
}
check(numsOk, 'gutter numbers from incremental updates match diffLineNumbers() on the full text');

// ── 7. Fuzz: many diff shapes × chunk sizes, incremental vs one-shot ──
console.log('fuzz parity:');
const fuzzTexts = [
  '',
  'no newline at end',
  'diff --git a/a b/a\n@@ -0,0 +1 @@\n+only line',
  'diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -1,2 +1,2 @@\n old\n-old2\n+new2\n\\ No newline at end of file',
  diffLines(40).join('\n') + '\n',
  diffLines(120).join('\n') + '\n@@ -50,2 +50,2 @@\n second\n-hunk\n+hunk\n',
];
let fuzzOk = true;
let fuzzMsg = '';
for (const text of fuzzTexts) {
  for (const step of [1, 2, 5, 13]) {
    const ci = window.document.createElement('div');
    window.document.body.appendChild(ci);
    for (let i = 1; i < text.length; i += step) updateDiffFallback(ci, text.slice(0, i));
    updateDiffFallback(ci, text);
    const cf = window.document.createElement('div');
    window.document.body.appendChild(cf);
    updateDiffFallback(cf, text);
    const pi = ci.querySelector('.diff-fallback');
    const pf = cf.querySelector('.diff-fallback');
    if (pi.children.length !== pf.children.length) {
      fuzzOk = false; fuzzMsg = 'row count ' + pi.children.length + ' vs ' + pf.children.length + ' (step ' + step + ')';
      break;
    }
    for (let i = 0; i < pi.children.length && fuzzOk; i++) {
      if (pi.children[i].innerHTML !== pf.children[i].innerHTML) {
        fuzzOk = false;
        fuzzMsg = 'row ' + i + ' (step ' + step + '): ' + pi.children[i].outerHTML + ' vs ' + pf.children[i].outerHTML;
      }
    }
    ci.remove();
    cf.remove();
  }
}
check(fuzzOk, 'incremental streaming matches one-shot render for all fuzz cases' + (fuzzOk ? '' : ' — ' + fuzzMsg));

if (failures) {
  console.error('\n' + failures + ' check(s) FAILED');
  process.exit(1);
}
console.log('\nall diff-fallback checks passed');
