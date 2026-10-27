'use strict';
// End-to-end regression test for the streaming markdown renderers in
// web/components/markdown.js, run against the REAL vendored marked +
// DOMPurify in a jsdom window:
//
//   1. List-continuation lookahead (splitStreamBlocks): a list whose items
//      are separated by blank lines stays ONE block while streaming, so the
//      live render is the loose list the one-shot parse produces — never a
//      stack of tight <ul> fragments that the final render would reflow.
//
//   2. Promotion-time reconciliation (renderStreamMarkdown): when cross-block
//      parser state makes the per-block render diverge from the one-shot
//      parse (indented continuations, reference-style link definitions), the
//      frozen prefix is repainted from the joint parse at the next block
//      promotion, so the screen matches the final render BEFORE it happens.
//
// The core assertion for every case: after streaming the full text (without
// any finalize call), the on-screen HTML equals the one-shot render, and
// setMessageMarkdown then changes nothing (normalized) — i.e. no jump at
// stream end.
//
// Run: node scripts/test_stream_reconcile.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { stripModuleSyntax } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
const VENDOR = path.join(ROOT, 'internal/server/web/vendor');
const MARKDOWN_JS = path.join(ROOT, 'internal/server/web/components/markdown.js');

// Same normalization the reconciliation itself uses (boundary whitespace
// between block-level tags is the only legal difference between two
// renderings of the same completed markdown).
const norm = (html) => html.replace(/>\s+</g, '><').trim();

// Streams `fullText` into `el` through renderStreamMarkdown in small chunks,
// like the live token path does.
function stream(el, render, fullText, chunk = 7) {
  for (let i = 0; i < fullText.length; i += chunk) {
    render(el, fullText.slice(0, Math.min(i + chunk, fullText.length)));
  }
  render(el, fullText);
}

// On-screen HTML while streaming: the .md-block children of the .msg-text
// wrapper, in order (per-block nodes, a repainted merged node, and the
// .md-tail). The wrapper divs are render scaffolding; their innerHTML is the
// marked output the user sees.
function onScreen(el) {
  const wrap = el.querySelector('.msg-text');
  return Array.from(wrap.children).map((c) => c.innerHTML).join('');
}

// On-screen HTML after setMessageMarkdown: the one-shot render puts the
// parsed content DIRECTLY into .msg-text (no .md-block scaffolding), so the
// wrapper's innerHTML is the content.
function finalizedScreen(el) {
  return el.querySelector('.msg-text').innerHTML;
}

async function main() {
  // 'outside-only' enables window.eval (used to load markdown.js below)
  // without running any page scripts.
  const dom = new JSDOM('<body></body>', { runScripts: 'outside-only' });
  const { window } = dom;
  // DOMPurify's browser bundle binds to the global window at import time —
  // expose the jsdom window/document before importing the vendor modules.
  globalThis.window = window;
  globalThis.document = window.document;

  const { marked } = await import('file://' + path.join(VENDOR, 'marked.esm.js'));
  const DOMPurifyMod = await import('file://' + path.join(VENDOR, 'dompurify.esm.js'));
  const DOMPurify = DOMPurifyMod.default;

  // The renderer's parse pipeline (mirrors renderMarkdownHTML) — the ground
  // truth the live render is compared against.
  const oneShot = (text) =>
    DOMPurify.sanitize(marked.parse(text, { async: false }), { USE_PROFILES: { html: true } });

  window.marked = marked;
  window.DOMPurify = DOMPurify;
  window.colorizeNode = () => {};
  window.sealStreamColorize = () => {};
  window.openFileAtLine = async () => {};
  window.eval(stripModuleSyntax(fs.readFileSync(MARKDOWN_JS, 'utf8')));

  const rawStore = new WeakMap();
  window.initMarkdown({
    showToast() {},
    copyTextToClipboard: async () => true,
    getMessageRawStore: () => rawStore,
  });
  const render = window.renderStreamMarkdown;
  const setMessageMarkdown = window.setMessageMarkdown;

  let failures = 0;
  const check = (desc, ok, detail) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}${!ok && detail ? ' — ' + detail : ''}`);
    if (!ok) failures++;
  };

  // Case 1: loose bullet list (the lookahead's targeted case). Use a plain
  // div — the streaming path is also used for non-.message elements.
  {
    const el = window.document.createElement('div');
    const text = 'Plan:\n\n- inspect the batcher\n\n- check positions\n\n- verify scroll\n\nsummary';
    stream(el, render, text);
    const before = onScreen(el);
    check('loose list: live render equals one-shot before finalize',
      norm(before) === norm(oneShot(text)),
      norm(before) + '\n    vs: ' + norm(oneShot(text)));
    check('loose list: streamed as ONE block (no tight <ul> fragments)',
      el._gogenBlocks.done.length === 2 && el._gogenBlocks.mergedNode === null,
      'done=' + el._gogenBlocks.done.length + ' merged=' + String(el._gogenBlocks.mergedNode !== null));
    setMessageMarkdown(el, text);
    check('loose list: finalize changes nothing (no end-of-stream reflow)',
      norm(finalizedScreen(el)) === norm(before));
  }

  // Case 2: indented continuation (beyond the lookahead — reconciliation
  // must repaint). Use a .message element to cover the msgBody() path.
  {
    const el = window.document.createElement('div');
    el.className = 'message assistant';
    const text = '- item with detail\n\n  and the detail continues here\n\n- second item\n\nmore';
    stream(el, render, text);
    const before = onScreen(el);
    check('indented continuation: reconciled live render equals one-shot before finalize',
      norm(before) === norm(oneShot(text)),
      norm(before) + '\n    vs: ' + norm(oneShot(text)));
    check('indented continuation: repaint fired (frozen prefix merged)',
      el._gogenBlocks.mergedNode !== null,
      'merged=' + String(el._gogenBlocks.mergedNode !== null));
    check('indented continuation: continuation paragraph is INSIDE the list item',
      before.includes('<li>') && /<li>[^]*<p>and the detail continues here<\/p>[^]*<\/li>/.test(before.replace(/>\s+</g, '><')),
      before);
    setMessageMarkdown(el, text);
    check('indented continuation: finalize changes nothing',
      norm(finalizedScreen(el)) === norm(before));
  }

  // Case 3: reference-style link whose definition arrives in a LATER block —
  // the usage block was frozen as literal text; the next promotion repaints
  // it as the resolved link.
  {
    const el = window.document.createElement('div');
    const text = 'See [the docs][md] first.\n\n[md]: https://example.test/x\n\nDone above.';
    stream(el, render, text);
    const before = onScreen(el);
    check('reference link: resolved live (not literal [md]) before finalize',
      norm(before) === norm(oneShot(text)),
      norm(before) + '\n    vs: ' + norm(oneShot(text)));
    setMessageMarkdown(el, text);
    check('reference link: finalize changes nothing',
      norm(finalizedScreen(el)) === norm(before));
  }

  // Case 4: plain paragraphs — the fast path must stay per-block (no
  // repaint), and still match the one-shot render.
  {
    const el = window.document.createElement('div');
    const text = 'para one\n\npara two\n\npara three';
    stream(el, render, text);
    check('plain paragraphs: no repaint (per-block freeze)',
      el._gogenBlocks.mergedNode === null && el._gogenBlocks.done.length === 2,
      'merged=' + String(el._gogenBlocks.mergedNode !== null) + ' done=' + el._gogenBlocks.done.length);
    check('plain paragraphs: live render equals one-shot before finalize',
      norm(onScreen(el)) === norm(oneShot(text)), onScreen(el));
  }

  // Case 6: sanitize budget. The promotion compare must run on RAW marked
  // output — DOMPurify sanitization is the dominant cost of the reconcile
  // pass (an order of magnitude above the parse), and sanitizing the joint
  // prefix at every promotion made long code-heavy streams slow down as they
  // grew. Sanitize must only ever see ONE BLOCK (the HTML that lands in the
  // DOM), never the accumulated prefix.
  {
    const el = window.document.createElement('div');
    const origSanitize = DOMPurify.sanitize;
    let sanitizeCalls = 0;
    let maxInput = 0;
    DOMPurify.sanitize = function (raw, ...rest) {
      sanitizeCalls++;
      maxInput = Math.max(maxInput, String(raw).length);
      return origSanitize.call(DOMPurify, raw, ...rest);
    };
    const lines = (i) => Array.from({ length: 50 }, (_, j) => 'const v' + i + '_' + j + ' = compute(' + j + ');').join('\n');
    let text = '';
    try {
      for (let i = 0; i < 4; i++) {
        text += 'Paragraph ' + i + '.\n\n```js\n' + lines(i) + '\n```\n\n';
        render(el, text);
      }
      // The tail render (a plain paragraph) is inside the budget too.
      render(el, text + 'tail');
    } finally {
      DOMPurify.sanitize = origSanitize;
    }
    // Each block's HTML is ~2.3KB; the accumulated prefix is ~9KB by the
    // last promotion. The old code sanitized the joint prefix on EVERY
    // promotion, so maxInput would be ≈ prefix length.
    check('sanitize budget: never sanitizes the joint prefix (max input = one block)',
      maxInput < 3000, 'maxInput=' + maxInput + ' calls=' + sanitizeCalls);
  }

  // Case 5: a rewind (text shrink) resets the state, merged node included,
  // and re-renders correctly from scratch.
  {
    const el = window.document.createElement('div');
    const text = 'alpha\n\nbeta\n\ngamma';
    stream(el, render, text);
    render(el, 'alpha'); // rewind: much shorter text
    check('rewind: state reset and re-render equals one-shot',
      norm(onScreen(el)) === norm(oneShot('alpha')), onScreen(el));
  }

  globalThis.window = undefined;
  globalThis.document = undefined;

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => {
  console.error('HARNESS ERROR:', err && err.stack || err);
  process.exit(1);
});
