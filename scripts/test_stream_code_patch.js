'use strict';
// Regression tests for the streaming fenced-code-block PATCH renderer in
// web/components/markdown.js (the C1 fix):
//
//   - A tail that is exactly one fenced code block is patched in place:
//     only the grown text is appended to the existing <code> element, so the
//     colorized spans survive and each flush costs O(delta) instead of a
//     full marked + DOMPurify re-parse and DOM swap of the whole block.
//   - The patched text/HTML must stay identical (modulo marked's entity
//     spelling for quotes) to the one-shot render of the same text, at every
//     prefix — the same invariant test_stream_reconcile.js pins for other
//     block shapes.
//   - A closing fence, a fence whose last line becomes a closer, a fence
//     with content after it, a fence that does not start the block, or any
//     other shape falls back to the full renderer and stays correct.
//
// Run: node scripts/test_stream_code_patch.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('/tmp/gogen-jsdom/node_modules/jsdom');
const { stripModuleSyntax } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
const VENDOR = path.join(ROOT, 'internal/server/web/vendor');
const MARKDOWN_JS = path.join(ROOT, 'internal/server/web/components/markdown.js');

const norm = (html) => html.replace(/>\s+</g, '><').trim();

async function main() {
  const dom = new JSDOM('<body></body>', { runScripts: 'outside-only' });
  const { window } = dom;
  globalThis.window = window;
  globalThis.document = window.document;

  const { marked } = await import('file://' + path.join(VENDOR, 'marked.esm.js'));
  const DOMPurifyMod = await import('file://' + path.join(VENDOR, 'dompurify.esm.js'));
  const DOMPurify = DOMPurifyMod.default;

  const oneShotHtml = (text) =>
    DOMPurify.sanitize(marked.parse(text, { async: false }), { USE_PROFILES: { html: true } });
  // The decoded code text a one-shot render would show (entities resolved,
  // exactly like the live DOM's textContent).
  const oneShotCodeText = (text) => {
    const d = window.document.createElement('div');
    d.innerHTML = oneShotHtml(text);
    const code = d.querySelector('pre code');
    return code ? code.textContent : null;
  };
  // The <pre> outerHTML of a scope (live) vs of a one-shot render: the
  // comparison target for code rendering — the copy-button wrapper
  // (code-block-wrap + head + button) is renderer scaffolding that marked's
  // one-shot HTML does not contain.
  const livePre = (scope) => {
    const p = scope && scope.querySelector('pre');
    return p ? p.outerHTML : '';
  };
  const oneShotPre = (text) => {
    const d = window.document.createElement('div');
    d.innerHTML = oneShotHtml(text);
    const p = d.querySelector('pre');
    return p ? p.outerHTML : '';
  };

  window.marked = marked;
  window.DOMPurify = DOMPurify;
  // The colorize governor is exercised in test_colorizenode.js; here it is a
  // no-op so the DOM stays plain text and the assertions read the real
  // patched content. markdown.js seals before recreating elements — also a
  // no-op here (the handoff contract has its own test,
  // test_colorize_handoff.js).
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

  // Renders growing prefixes of `fullText` from empty (a fresh stream).
  function stream(el, fullText, chunk = 5) {
    for (let n = chunk; n < fullText.length; n += chunk) render(el, fullText.slice(0, n));
    render(el, fullText);
  }
  // Continues a stream that already rendered `prevText`: only GROWING
  // prefixes (rewinds reset the renderer state, which is not this test's
  // subject) so the patch path stays engaged.
  function streamFrom(el, prevText, fullText, chunk = 5) {
    for (let n = prevText.length + chunk; n < fullText.length; n += chunk) {
      render(el, fullText.slice(0, n));
    }
    render(el, fullText);
  }

  let failures = 0;
  const check = (desc, ok, detail) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}${!ok && detail ? ' — ' + detail : ''}`);
    if (!ok) failures++;
  };
  const mkEl = () => window.document.createElement('div');

  // ── 1. Patch path: a growing fence keeps its code element ──
  {
    const el = mkEl();
    const part1 = '```js\nconst x = 1;';
    stream(el, part1, 4);
    const code1 = el.querySelector('.md-tail pre code');
    check('patch: tail code element rendered', !!code1 && /language-js/.test(code1.className));
    check('patch: first render is one-shot-equal',
      norm(livePre(el.querySelector('.md-tail'))) === norm(oneShotPre(part1)),
      norm(livePre(el.querySelector('.md-tail'))) + ' vs ' + norm(oneShotPre(part1)));

    const part2 = part1 + '\nconst y = 2;';
    streamFrom(el, part1, part2, 3);
    const code2 = el.querySelector('.md-tail pre code');
    check('patch: the code element is patched in place (same node)', code1 === code2);
    check('patch: patched text equals the one-shot render',
      code2.textContent === oneShotCodeText(part2),
      JSON.stringify(code2.textContent) + ' vs ' + JSON.stringify(oneShotCodeText(part2)));
    check('patch: live code HTML equals the one-shot render',
      norm(livePre(el.querySelector('.md-tail'))) === norm(oneShotPre(part2)),
      norm(livePre(el.querySelector('.md-tail'))) + '\n    vs ' + norm(oneShotPre(part2)));
    check('patch: __gogenRaw tracks the raw code text (no normalized newline)',
      code2.__gogenRaw === 'const x = 1;\nconst y = 2;',
      JSON.stringify(code2.__gogenRaw));
    check('patch: __gogenPlain tracks the displayed text',
      code2.__gogenPlain === code2.textContent,
      JSON.stringify(code2.__gogenPlain) + ' vs ' + JSON.stringify(code2.textContent));
    check('patch: copy button preserved through patches',
      !!el.querySelector('.md-tail .code-block-wrap > .code-block-head .code-copy-btn'));
    check('patch: the pre stays inside the copy-button wrap',
      el.querySelector('.md-tail .code-block-wrap > pre > code') === code2);

    // ── 2. Closing fence in ONE step: no DOM rebuild (the closer only ends
    //      the block; the code content is unchanged), parity preserved ──
    const part3 = part2 + '\n```';
    render(el, part3);
    const code3 = el.querySelector('.md-tail pre code');
    check('closer: an unsplit closing fence does not recreate the code element', code3 === code2);
    check('closer: closed-fence text equals the one-shot render',
      code3.textContent === oneShotCodeText(part3),
      JSON.stringify(code3.textContent) + ' vs ' + JSON.stringify(oneShotCodeText(part3)));
    check('closer: live code HTML equals the one-shot render',
      norm(livePre(el.querySelector('.md-tail'))) === norm(oneShotPre(part3)));

    // ── 3. Promotion: the closed fence freezes, the tail moves on ──
    const part4 = part3 + '\n\nafter';
    streamFrom(el, part3, part4, 3);
    const stable = el.querySelector('.md-block:not(.md-tail)');
    check('promotion: closed fence promoted to a stable block with its code',
      !!stable && !!stable.querySelector('pre code'));
    check('promotion: promoted code text equals the one-shot render',
      !!stable && stable.querySelector('pre code').textContent === 'const x = 1;\nconst y = 2;\n',
      stable && JSON.stringify(stable.querySelector('pre code').textContent));
    check('promotion: the tail now renders the following paragraph',
      !!el.querySelector('.md-tail') && el.querySelector('.md-tail').textContent.includes('after'));
    check('promotion: promoted code HTML equals the one-shot render',
      norm(livePre(stable)) === norm(oneShotPre(part4)),
      norm(livePre(stable)) + '\n    vs ' + norm(oneShotPre(part4)));
  }

  // ── 4. Boundary: a partial closer line becoming a real closer ──
  // (" `` " is code content until the third tick arrives; the fallback must
  // drop it from the code element.)
  {
    const el = mkEl();
    const a = '```js\ncode\n``';
    stream(el, a, 3);
    const codeA = el.querySelector('.md-tail pre code');
    check('boundary: a partial closer is code content (one-shot parity)',
      codeA.textContent === oneShotCodeText(a),
      JSON.stringify(codeA.textContent) + ' vs ' + JSON.stringify(oneShotCodeText(a)));
    const b = a + '`';
    streamFrom(el, a, b, 3);
    const codeB = el.querySelector('.md-tail pre code');
    check('boundary: completing the closer falls back to a full render', codeB !== codeA);
    check('boundary: the closer is no longer inside the code text',
      codeB.textContent === oneShotCodeText(b),
      JSON.stringify(codeB.textContent) + ' vs ' + JSON.stringify(oneShotCodeText(b)));
    check('boundary: live code HTML equals the one-shot render',
      norm(livePre(el.querySelector('.md-tail'))) === norm(oneShotPre(b)),
      norm(livePre(el.querySelector('.md-tail'))) + '\n    vs ' + norm(oneShotPre(b)));
  }

  // ── 5. Content after the closing fence (not patchable) stays correct ──
  {
    const el = mkEl();
    const a = '```js\nx = 1\n```';
    stream(el, a, 3);
    const b = a + '\ntail text';
    streamFrom(el, a, b, 3);
    check('after-closer content: live code HTML equals the one-shot render',
      norm(livePre(el.querySelector('.md-tail'))) === norm(oneShotPre(b)),
      norm(livePre(el.querySelector('.md-tail'))) + '\n    vs ' + norm(oneShotPre(b)));
    check('after-closer content: code text stays the fence content',
      el.querySelector('.md-tail pre code').textContent === 'x = 1\n');
    check('after-closer content: the trailing text renders as a paragraph',
      /tail text/.test(el.querySelector('.md-tail').textContent));
  }

  // ── 6. A fence that does not start the block is never patched ──
  {
    const el = mkEl();
    const a = 'intro\n```js\nx = 1';
    stream(el, a, 4);
    const codeA = el.querySelector('.md-tail pre code');
    const b = a + ' + 2';
    streamFrom(el, a, b, 3);
    const codeB = el.querySelector('.md-tail pre code');
    check('fence after text: full renders (code element recreated)', codeA !== codeB);
    check('fence after text: rendering still equals the one-shot render',
      norm(livePre(el.querySelector('.md-tail'))) === norm(oneShotPre(b)),
      norm(livePre(el.querySelector('.md-tail'))) + '\n    vs ' + norm(oneShotPre(b)));
  }

  // ── 6b. Perf contract: a long continuously-streamed fence materializes
  //        its code element ONCE (after the fence opener) — every later
  //        flush patches it. This is the C1 fix's core guarantee: no
  //        per-flush marked/DOMPurify re-parse and DOM swap of the block.
  {
    const el = mkEl();
    let long = '```js\n';
    for (let i = 0; i < 40; i++) long += 'const v' + i + ' = ' + i + ';\n';
    long += 'const last = v39';
    stream(el, long.slice(0, 12), 3); // opener + a little content
    let creations = 0;
    let last = null;
    for (let n = 12 + 3; n < long.length; n += 6) {
      render(el, long.slice(0, n));
      const c = el.querySelector('.md-tail pre code');
      if (c !== last) { creations++; last = c; }
    }
    render(el, long);
    const finalCode = el.querySelector('.md-tail pre code');
    if (finalCode !== last) creations++;
    check('perf: a long streamed fence materializes its code element exactly once',
      creations === 1, 'creations=' + creations);
    check('perf: the patched long fence equals the one-shot render',
      finalCode.textContent === oneShotCodeText(long),
      JSON.stringify(finalCode.textContent && finalCode.textContent.slice(-20)) + ' vs ' +
      JSON.stringify(oneShotCodeText(long) && oneShotCodeText(long).slice(-20)));
    check('perf: the long fence finished with a completed line',
      finalCode.textContent.endsWith('const last = v39\n'));
  }

  // ── 7. Quotes in the code: textContent parity (marked entity spellings
  //      serialize differently in a patched text node; the rendered text is
  //      identical, which is what users see and what the final render pins).
  {
    const el = mkEl();
    const a = '```js\nconst s = "a" + \'b\';';
    stream(el, a, 3);
    const codeA = el.querySelector('.md-tail pre code');
    const b = a + '\nconst t = 2;';
    streamFrom(el, a, b, 3);
    const codeB = el.querySelector('.md-tail pre code');
    check('quotes: code element patched in place', codeA === codeB);
    check('quotes: patched textContent equals the one-shot render',
      codeB.textContent === oneShotCodeText(b),
      JSON.stringify(codeB.textContent) + ' vs ' + JSON.stringify(oneShotCodeText(b)));
  }

  // ── 8. CRLF: patching is disabled for '\r' (the patch appends bytes
  //      verbatim while marked normalizes CRLF to LF on a full render), so a
  //      CRLF fence keeps one-shot parity at every prefix instead of briefly
  //      showing raw carriage returns. ──
  {
    const el = mkEl();
    const a = '```js\r\nconst x = 1;';
    stream(el, a, 4);
    check('crlf: a streamed prefix matches the one-shot render',
      el.querySelector('.md-tail pre code').textContent === oneShotCodeText(a),
      JSON.stringify(el.querySelector('.md-tail pre code').textContent) + ' vs ' + JSON.stringify(oneShotCodeText(a)));
    const b = a + '\r\nconst y = 2;\r\n```';
    streamFrom(el, a, b, 4);
    check('crlf: the closed fence matches the one-shot render',
      norm(livePre(el.querySelector('.md-tail'))) === norm(oneShotPre(b)),
      norm(livePre(el.querySelector('.md-tail'))) + '\n    vs ' + norm(oneShotPre(b)));
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
