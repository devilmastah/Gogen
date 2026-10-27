// Regression test for the incremental updateDiffEditor (board #89): while a
// patch diff streams into a chat tool card, the Monaco path must splice
// append-only tails into the model (model.applyEdits) instead of running
// model.setValue(fullDiff) per tool_call_delta — the whole-buffer replace,
// re-tokenize and scroll reset per delta was O(n^2) over the stream (the
// fallback <pre> already got this fix in board #73; the Monaco path — the
// default viewer — had not).
//
// Also covers the per-delta O(n) companions on the same path:
//   - unified-diff decorations: append-only pass adds just the new tail's
//     decorations (empty removal list) instead of rebuilding all of them;
//   - the file-line-number cache (ed.__gogenDiffNums) read by the
//     lineNumbers callback: extended incrementally, with the held-back scan
//     state invariant (state advances past COMPLETE lines only — a
//     half-received '@@' hunk header must not poison later numbers).
//
// Loads the REAL editor.js into jsdom (module syntax stripped via
// scripts/web-harness.js) and drives it with a minimal fake monaco model
// that implements real line math, so edit ranges are actually exercised.
//
// Run: node scripts/test_diff_editor_stream.js
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

const dom = new JSDOM('<!doctype html><html><body></body></html>', { url: 'http://localhost/', runScripts: 'dangerously' });
const { window } = dom;

// jsdom (no pretendToBeVisual) has no rAF; queue callbacks so tests can
// flush them explicitly.
const rafQueue = [];
window.requestAnimationFrame = (fn) => rafQueue.push(fn) - 1;
function flushRaf() {
  while (rafQueue.length) rafQueue.shift()();
}

window.DOMPurify = { sanitize: (h) => h }; // only used by colorize paths, not exercised here

// Minimal fake monaco: updateDiffEditor's edit uses a plain IRange object,
// but applyUnifiedDiffDecorations (both passes) construct monaco.Range via
// the module-local `monaco`, which only initMonaco() assigns. Patch the
// single initialization line so the module can be driven headless.
const fakeMonaco = {
  Range: function Range(sl, sc, el, ec) {
    this.startLineNumber = sl;
    this.startColumn = sc;
    this.endLineNumber = el;
    this.endColumn = ec;
  },
};
window.__fakeMonaco = fakeMonaco;
let src = fs.readFileSync(path.join(ROOT, 'internal/server/web/editor.js'), 'utf8');
const MODULE_LINE = 'let monaco = null;';
check(src.includes(MODULE_LINE), 'editor.js still initializes the module-local monaco');
src = src.replace(MODULE_LINE, 'let monaco = window.__fakeMonaco || null;');
installStorage(window); // editor.js reads persisted prefs at top level
window.eval(stripModuleSyntax(src));
const { updateDiffEditor, diffLineNumbers, makeDiffNumsCache } = window;
check(typeof updateDiffEditor === 'function', 'updateDiffEditor loaded from real editor.js');
check(typeof makeDiffNumsCache === 'function', 'makeDiffNumsCache loaded from real editor.js');

// ── Fake model: real line math (lines array), records setValue/applyEdits ──
class FakeModel {
  constructor(text) {
    this._lines = text === '' ? [''] : text.split('\n');
    this.setValueCalls = [];
    this.editOps = [];
  }
  getLineCount() { return this._lines.length; }
  getLineMaxColumn(line) { return (this._lines[line - 1] || '').length + 1; }
  getLineContent(line) { return this._lines[line - 1] || ''; }
  getValue() { return this._lines.join('\n'); }
  setValue(value) {
    this.setValueCalls.push(value);
    this._lines = value === '' ? [''] : value.split('\n');
  }
  applyEdits(ops) {
    for (const op of ops) {
      this.editOps.push(op);
      const r = op.range;
      const startText = this._lines[r.startLineNumber - 1] || '';
      const endText = this._lines[r.endLineNumber - 1] || '';
      const head = startText.slice(0, r.startColumn - 1);
      const tail = endText.slice(r.endColumn - 1);
      const replacement = (head + op.text + tail).split('\n');
      if (r.startLineNumber === r.endLineNumber && replacement.length === 1) {
        this._lines[r.startLineNumber - 1] = replacement[0];
      } else {
        this._lines.splice(r.startLineNumber - 1, r.endLineNumber - r.startLineNumber + 1, ...replacement);
      }
    }
  }
}

// ── Fake editor: scroll geometry, reveal/decoration bookkeeping ──
class FakeEditor {
  constructor(model, geo) {
    this._model = model;
    this.revealed = [];
    this.decoCalls = [];
    this._decos = [];
    this._nextDecoId = 1;
    this._geo = geo; // { scrollTop, scrollHeight, height, visibleTop }
  }
  getModel() { return this._model; }
  getLayoutInfo() { return { height: this._geo.height }; }
  getScrollTop() { return this._geo.scrollTop; }
  getScrollHeight() { return this._geo.scrollHeight; }
  getVisibleRanges() { return [{ startLineNumber: this._geo.visibleTop || 1 }]; }
  revealLine(line) { this.revealed.push(line); }
  deltaDecorations(oldIds, newDecos) {
    this.decoCalls.push({ removed: oldIds.length, added: newDecos.length });
    const gone = new Set(oldIds);
    this._decos = this._decos.filter((id) => !gone.has(id));
    const added = newDecos.map(() => 'd' + this._nextDecoId++);
    this._decos.push(...added);
    return added;
  }
  getDomNode() { return null; } // rAF resize fixup: container null → no-op
  decoCount() { return this._decos.length; }
}

// The decoration classification rules (mirrors diffLineClass in editor.js —
// recomputed here so a shared-helper bug cannot hide).
function expectedDecoCount(text) {
  let n = 0;
  for (const line of text.split('\n')) {
    if (/^(\+\+\+|---|diff |index )/.test(line) || line.startsWith('@@') || line.startsWith('+') || line.startsWith('-')) n++;
  }
  return n;
}

// Mount-time seeding exactly as mountDiffEditor does (mount itself needs
// real monaco; the source-level contract is asserted in the last section).
function mountEd(text, geo) {
  const model = new FakeModel(text);
  const ed = new FakeEditor(model, geo);
  ed.__gogenDiffValue = text || '';
  ed.__gogenDiffNums = makeDiffNumsCache(text || '');
  return ed;
}

// Stream `full` into `ed` in slices of `step` chars; after every slice verify
// the invariants shared by all append-only scenarios. Returns the number of
// streaming updates performed.
function streamAndVerify(ed, full, step) {
  let updates = 0;
  let opsChecked = 0;
  // First delta starts one step past whatever the editor already holds (the
  // mount/adoption seed) so every streamed prefix is a pure append.
  for (let end = ed.__gogenDiffValue.length + step; ; end += step) {
    const prefix = full.slice(0, Math.min(end, full.length));
    const model = ed.getModel();
    const setValuesBefore = model.setValueCalls.length;
    const decoCallsBefore = ed.decoCalls.length;
    updateDiffEditor(ed, prefix);
    flushRaf();
    updates++;

    const label = 'delta @' + prefix.length + ' (step ' + step + ')';
    check(model.getValue() === prefix, 'model holds the exact prefix — ' + label);
    check(model.setValueCalls.length === setValuesBefore, 'no setValue on append — ' + label);
    check(ed.__gogenDiffValue === prefix, 'value mirror tracks the prefix — ' + label);

    // Every edit op issued by this update must be a zero-width insertion at
    // the model's (pre-edit) end.
    for (let i = opsChecked; i < model.editOps.length; i++) {
      const r = model.editOps[i].range;
      check(
        r.startLineNumber === r.endLineNumber && r.startColumn === r.endColumn,
        'edit op is zero-width — ' + label
      );
      const line = r.startLineNumber;
      check(
        line === model.getLineCount() || line === model.getLineCount() - model.editOps[i].text.split('\n').length + 1,
        'edit op starts at the model tail — ' + label
      );
    }
    opsChecked = model.editOps.length;

    // Decoration bookkeeping: an append-only pass may replace at most the
    // grown last line's single decoration (its class can cross a prefix
    // threshold as the line grows) — never a mass removal — and the total
    // must match a full independent classification.
    for (let i = decoCallsBefore; i < ed.decoCalls.length; i++) {
      check(ed.decoCalls[i].removed <= 1, 'append removes at most the grown line decoration — ' + label);
    }
    check(
      ed.decoCount() === expectedDecoCount(prefix),
      'decoration count matches independent full classification — ' + label +
        ' (' + ed.decoCount() + ' vs ' + expectedDecoCount(prefix) + ')'
    );

    // Line-number cache: incremental result must equal a fresh full scan,
    // and stay aligned with the text's line count.
    const cache = ed.__gogenDiffNums;
    const want = diffLineNumbers(prefix);
    check(cache.nums.length === prefix.split('\n').length, 'nums aligned with line count — ' + label);
    let numsOk = cache.nums.length === want.length;
    for (let i = 0; i < want.length && numsOk; i++) numsOk = cache.nums[i] === want[i];
    check(numsOk, 'incremental line numbers match full scan — ' + label);

    if (prefix.length >= full.length) break;
  }
  return updates;
}

// Deterministic two-hunk unified diff (hunk math illustrative, not real).
function twoHunkDiff() {
  return [
    'diff --git a/f.go b/f.go',
    'index 1234567..89abcde 100644',
    '--- a/f.go',
    '+++ b/f.go',
    '@@ -1,3 +1,4 @@',
    ' context',
    '-old one',
    '+new one',
    '+new two',
    ' more context',
    '@@ -10,3 +11,3 @@',
    ' later context',
    '-later old',
    '+later new',
    '',
  ].join('\n');
}

console.log('append-only stream, reader pinned to bottom');
{
  const full = twoHunkDiff();
  // 300 scroll height, 100 viewport, scrollTop 200 → within 16px of bottom.
  const ed = mountEd('', { scrollTop: 200, scrollHeight: 300, height: 100 });
  const revealsBefore = ed.revealed.length;
  const updates = streamAndVerify(ed, full, 7);
  check(ed.getModel().setValueCalls.length === 0, 'never setValue across the whole stream');
  check(
    ed.revealed.length === revealsBefore + updates && updates > 0,
    'pinned-to-bottom reader follows the tail (revealLine per delta)'
  );
  check(ed.revealed[ed.revealed.length - 1] === full.split('\n').length, 'reveal targets the new last line');
}

console.log('append-only stream, reader scrolled up');
{
  const full = twoHunkDiff();
  // scrollTop 0, 300/100 → 200px from the bottom: not at bottom.
  const ed = mountEd('', { scrollTop: 0, scrollHeight: 300, height: 100 });
  const revealsBefore = ed.revealed.length;
  streamAndVerify(ed, full, 11);
  check(ed.revealed.length === revealsBefore, 'scrolled-up reader is not re-revealed (viewport untouched)');
}

console.log('unchanged value is a full no-op');
{
  const full = twoHunkDiff();
  const ed = mountEd('', { scrollTop: 0, scrollHeight: 300, height: 100 });
  updateDiffEditor(ed, full);
  flushRaf();
  const m = ed.getModel();
  const before = {
    setValue: m.setValueCalls.length,
    ops: m.editOps.length,
    reveal: ed.revealed.length,
    deco: ed.decoCalls.length,
  };
  updateDiffEditor(ed, full); // same value the mirror already holds
  flushRaf();
  check(m.setValueCalls.length === before.setValue, 'no setValue on unchanged value');
  check(m.editOps.length === before.ops, 'no edit ops on unchanged value');
  check(ed.revealed.length === before.reveal, 'no reveal on unchanged value');
  check(ed.decoCalls.length === before.deco, 'no decoration churn on unchanged value');
}

console.log('non-append rewrite (server re-sent a corrected diff) takes the full path');
{
  const first = twoHunkDiff();
  const ed = mountEd('', { scrollTop: 0, scrollHeight: 300, height: 100, visibleTop: 5 });
  updateDiffEditor(ed, first);
  flushRaf();
  const corrected = twoHunkDiff().replace('-old one', '-OLD ONE');
  const m = ed.getModel();
  const decosBefore = ed.decoCount();
  ed.revealed.length = 0;
  updateDiffEditor(ed, corrected);
  flushRaf();
  check(m.setValueCalls.length === 1 && m.setValueCalls[0] === corrected, 'rewrite uses setValue with the corrected text');
  check(m.getValue() === corrected, 'model holds the corrected text');
  check(ed.__gogenDiffValue === corrected, 'value mirror tracks the rewrite');
  check(ed.__gogenDiffNums.text === corrected, 'line-number cache rebuilt for the rewrite');
  const want = diffLineNumbers(corrected);
  check(JSON.stringify(ed.__gogenDiffNums.nums) === JSON.stringify(want), 'line numbers match a fresh full scan');
  const lastCall = ed.decoCalls[ed.decoCalls.length - 1];
  check(lastCall.removed === decosBefore, 'rewrite removes the old decorations (full pass)');
  check(ed.decoCount() === expectedDecoCount(corrected), 'rewrite decoration count matches full classification');
  check(ed.revealed.length === 1 && ed.revealed[0] === 5, 'rewrite keeps the top visible line in place (reveal 5)');
}

console.log('CRLF text takes the safe full path (models EOL-normalize)');
{
  const ed = mountEd('', { scrollTop: 0, scrollHeight: 300, height: 100 });
  const crlf = 'diff --git a/a b/a\r\n@@ -1,1 +1,1 @@\r\n-old\r\n+new\r\n';
  updateDiffEditor(ed, crlf);
  flushRaf();
  check(ed.getModel().setValueCalls.length === 1, 'CRLF value uses setValue');
  const grown = crlf + '+one more\r\n';
  updateDiffEditor(ed, grown);
  flushRaf();
  check(ed.getModel().setValueCalls.length === 2, 'append refused while the value contains CR');
  check(ed.__gogenDiffValue === grown, 'mirror still tracks the value');
}

console.log('untracked editor adopts tracking, then streams append-only');
{
  const full = twoHunkDiff();
  const model = new FakeModel(full.slice(0, 10)); // "mounted" by someone else, no seeds
  const ed = new FakeEditor(model, { scrollTop: 0, scrollHeight: 300, height: 100 });
  updateDiffEditor(ed, full.slice(0, 10)); // model already holds this text
  flushRaf();
  check(model.setValueCalls.length === 0, 'no-op detected for untracked editor holding the same text');
  check(ed.__gogenDiffValue === full.slice(0, 10), 'tracking adopted');
  check(ed.__gogenDiffNums.text === full.slice(0, 10), 'line-number cache adopted');
  streamAndVerify(ed, full, 5);
  check(model.setValueCalls.length === 0, 'subsequent stream is append-only after adoption');
}

console.log('fuzz: char-by-char and coarse streaming of multi-hunk diffs (incl. hunk headers split mid-line)');
{
  const texts = [
    twoHunkDiff(),
    '@@ -1,1 +1,1 @@\n-a\n+b',
    'diff --git a/a b/a\n@@ -1,2 +1,2 @@\n x\n-y\n+z\n\\ No newline at end of file',
    'diff --git a/a b/a\n@@ -1,1 +1,1 @@\n+only line',
    twoHunkDiff() + '\n@@ -99,1 +99,1 @@\n-tail\n+tail new\n',
  ];
  let ok = true;
  let msg = '';
  outer: for (const text of texts) {
    for (const step of [1, 2, 3, 5, 13]) {
      const ed = mountEd('', { scrollTop: 0, scrollHeight: 10000, height: 100 });
      try {
        for (let end = step; end < text.length; end += step) {
          updateDiffEditor(ed, text.slice(0, end));
          const cache = ed.__gogenDiffNums;
          const want = diffLineNumbers(text.slice(0, end));
          if (cache.nums.length !== want.length ||
              cache.nums.some((n, i) => n !== want[i]) ||
              ed.getModel().getValue() !== text.slice(0, end) ||
              ed.__gogenDiffValue !== text.slice(0, end) ||
              ed.getModel().setValueCalls.length !== 0 ||
              ed.decoCount() !== expectedDecoCount(text.slice(0, end))) {
            ok = false;
            msg = 'text ' + JSON.stringify(text.slice(0, 30)) + '… step ' + step + ' at ' + end;
            break outer;
          }
        }
        updateDiffEditor(ed, text);
        if (ed.getModel().getValue() !== text || ed.getModel().setValueCalls.length !== 0) {
          ok = false; msg = 'final full value mismatch, step ' + step; break outer;
        }
      } catch (err) {
        ok = false; msg = 'threw: ' + err.message + ' (step ' + step + ')'; break outer;
      }
    }
  }
  check(ok, 'fuzz streaming matches full-scan semantics at every delta' + (ok ? '' : ' — ' + msg));
}

console.log('incomplete trailing line then completion (held-back scan state)');
{
  // The last hunk header arrives split across deltas: a partial '@@ -10,3'
  // must NOT advance the real scan state (it would corrupt later numbers).
  const head = twoHunkDiff(); // ends with a trailing newline
  const tail1 = '\n@@ -20,2 +2';  // incomplete second hunk header
  const tail2 = '0,2 @@\n-xx\n+yy\n';
  const ed = mountEd('', { scrollTop: 0, scrollHeight: 10000, height: 100 });
  updateDiffEditor(ed, head + tail1);
  flushRaf();
  let t = head + tail1;
  let want = diffLineNumbers(t);
  check(JSON.stringify(ed.__gogenDiffNums.nums) === JSON.stringify(want), 'partial hunk header previews without advancing state');
  check(ed.__gogenDiffNums.lastLine === '@@ -20,2 +2', 'incomplete trailing line is tracked for held-back state');
  updateDiffEditor(ed, t + '0,2 @@\n');
  flushRaf();
  t += '0,2 @@\n';
  want = diffLineNumbers(t);
  check(JSON.stringify(ed.__gogenDiffNums.nums) === JSON.stringify(want), 'completed header numbers match full scan');
  updateDiffEditor(ed, head + tail1 + tail2);
  flushRaf();
  t = head + tail1 + tail2;
  want = diffLineNumbers(t);
  check(JSON.stringify(ed.__gogenDiffNums.nums) === JSON.stringify(want), 'lines after the split header number correctly');
  check(ed.getModel().setValueCalls.length === 0, 'whole sequence stayed append-only');
}

console.log('scanDiffValueIncremental matches extractDiffValue at every prefix');
{
  // The streaming card updater decodes the diff value incrementally (only the
  // new tail per frame — re-decoding the whole value was O(args²) over a
  // streamed patch). It must agree with the batch extractor at EVERY prefix,
  // including a backslash escape split across two deltas.
  const { scanDiffValueIncremental, extractDiffValue } = window;
  const cases = [
    JSON.stringify({ path: 'a.go', diff: '--- a\n+++ b\n@@ -1 +1 @@\n-old\n+new' }),
    JSON.stringify({ diff: 'line1\nline2\t"quoted"\\backslash\r\nend' }),
    JSON.stringify({ diff: '', path: 'x' }),
  ];
  for (const args of cases) {
    const scan = { keyed: false, complete: false, pos: 0, valueStart: -1, out: '' };
    let mismatch = null;
    for (let n = 1; n <= args.length; n++) {
      const prefix = args.slice(0, n);
      const inc = scanDiffValueIncremental(scan, prefix);
      const batch = extractDiffValue(prefix);
      // One allowed difference: when the prefix ends on a lone backslash (an
      // escape split across deltas), the batch extractor emits the raw '\'
      // for that frame while the incremental scanner holds it back until the
      // escaped character arrives — it must never surface a half-decoded
      // escape.
      const heldBackEscape = !batch.complete && batch.value.endsWith('\\')
        && inc.value === batch.value.slice(0, -1);
      if (inc.ok !== batch.ok || inc.complete !== batch.complete
        || (inc.value !== batch.value && !heldBackEscape)) {
        mismatch = { n, inc, batch };
        break;
      }
    }
    check(!mismatch, 'incremental scan matches at every prefix (' + JSON.stringify(args.slice(0, 24)) + '\u2026)'
      + (mismatch ? ' — first mismatch at ' + mismatch.n + ': inc=' + JSON.stringify(mismatch.inc) + ' batch=' + JSON.stringify(mismatch.batch) : ''));
  }
  // Once complete, later calls keep returning the finished value without
  // rescanning.
  const full = JSON.stringify({ diff: 'abc' });
  const scan = { keyed: false, complete: false, pos: 0, valueStart: -1, out: '' };
  scanDiffValueIncremental(scan, full.slice(0, 9));
  let out = null;
  for (let n = 10; n <= full.length; n++) out = scanDiffValueIncremental(scan, full.slice(0, n));
  check(out && out.complete && out.value === 'abc', 'value completed across deltas decodes exactly');
  check(scanDiffValueIncremental(scan, full) === out || (out.complete && scan.complete), 'repeat call after completion stays complete');
}

console.log('mountDiffEditor seeds the streaming state (source contract — mount needs real monaco)');
{
  check(src.includes('ed.__gogenDiffValue = value || \'\';'), 'mountDiffEditor seeds __gogenDiffValue');
  check(src.includes('ed.__gogenDiffNums = makeDiffNumsCache(value || \'\');'), 'mountDiffEditor seeds __gogenDiffNums');
  check(src.includes('extendDiffNumsCache(ed.__gogenDiffNums'), 'append path extends the line-number cache');
  check(src.includes('appendUnifiedDiffDecorations(ed,'), 'append path uses the incremental decoration pass');
}

flushRaf();
if (failures) {
  console.error('\n' + failures + ' check(s) FAILED');
  process.exit(1);
}
console.log('\nall diff-editor-stream checks passed');
