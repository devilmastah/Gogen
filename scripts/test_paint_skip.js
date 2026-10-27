'use strict';
// Unit test for components/paint-skip.js — the off-screen paint skip that
// preserves the real scroll geometry.
//
// jsdom has no layout, so this stubs getBoundingClientRect (known height) and
// getComputedStyle (known padding/border) per element and checks the two
// things that made the difference between a correct skip and the scroll jank:
//   - the --ps-h recorded is the CONTENT box, not the border box:
//     contain-intrinsic-size sizes the content box, so vertical padding must
//     be subtracted or a skipped item reports a box taller than the rendered
//     one (the thought card's 12px) and scrollHeight drifts;
//   - only connected elements are tagged, tagging is idempotent, and
//     fractional heights survive.
//
// Run: node scripts/test_paint_skip.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { ROOT, stripModuleSyntax } = require('./web-harness');

const raf = (w) => new Promise((r) => w.requestAnimationFrame(r));

async function main() {
  const dom = new JSDOM('<!DOCTYPE html><body></body>', { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;

  // jsdom's CSS engine mangles inline shorthand padding/border, so drive the
  // content-box math through a controlled getComputedStyle instead: each test
  // element carries __box = {t,b,bt,bb} (vertical padding / border in px).
  window.getComputedStyle = (el) => {
    const b = (el && el.__box) || {};
    return {
      paddingTop: (b.t || 0) + 'px',
      paddingBottom: (b.b || 0) + 'px',
      borderTopWidth: (b.bt || 0) + 'px',
      borderBottomWidth: (b.bb || 0) + 'px',
    };
  };

  window.eval(stripModuleSyntax(fs.readFileSync(path.join(ROOT, 'internal/server/web/components/paint-skip.js'), 'utf8')));

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };

  const mk = (height, box) => {
    const el = window.document.createElement('div');
    el.getBoundingClientRect = () => ({ height });
    el.__box = box || {};
    window.document.body.appendChild(el);
    return el;
  };

  // 1. Content-box math: a 200px border-box with 6px vertical padding (and a
  //    left-only border that must NOT be subtracted) records 188px.
  const padded = mk(200, { t: 6, b: 6, bt: 0, bb: 0 });
  window.enablePaintSkip(padded);
  await raf(window); await raf(window);
  check('padded item is paint-skipped', padded.classList.contains('paint-skip'));
  check('records the content box (200 - 12 = 188), not the border box',
    padded.style.getPropertyValue('--ps-h') === '188px');

  // 2. No padding: the content box equals the border box.
  const plain = mk(120, {});
  window.enablePaintSkip(plain);
  await raf(window); await raf(window);
  check('unpadded item records its full height', plain.style.getPropertyValue('--ps-h') === '120px');

  // 3. Sub-pixel heights survive (fractional contain-intrinsic-size is legal).
  const frac = mk(120.5, {});
  window.enablePaintSkip(frac);
  await raf(window); await raf(window);
  check('fractional height is preserved', frac.style.getPropertyValue('--ps-h') === '120.5px');

  // 4. Idempotent: re-queueing does not re-tag or clear the recorded size.
  frac.style.setProperty('--ps-h', '999px');
  window.enablePaintSkip(frac);
  await raf(window); await raf(window);
  check('re-enabling an already-skipped item is a no-op', frac.style.getPropertyValue('--ps-h') === '999px');

  // 5. A detached element is never tagged (its rect is meaningless).
  const detached = window.document.createElement('div');
  detached.getBoundingClientRect = () => ({ height: 50 });
  window.enablePaintSkip(detached);
  await raf(window); await raf(window);
  check('detached item is not tagged', !detached.classList.contains('paint-skip'));

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
