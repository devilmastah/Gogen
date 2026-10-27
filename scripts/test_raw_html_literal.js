'use strict';
// Regression test for the "raw HTML in model/user text" policy: a quoted
// <div id="…"> must display as the literal text the model wrote — escaped in
// the DOM so it can never become an element (and, when unclosed, can never
// nest and compound the 0.9em code font-size in reasoning cards), decoded
// on screen and in copy. The policy is wired in app.js via the marked
// renderer override; this test pins the behavior with a verbatim copy of
// that config (keep in sync) and checks the wiring is still present.
//
// Run: node scripts/test_raw_html_literal.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));

const ROOT = path.join(__dirname, '..');
const VENDOR = path.join(ROOT, 'internal/server/web/vendor');

// ── Verbatim copy from app.js (keep in sync) ──
const escapeHtml = (str) => {
  if (str == null) return '';
  const map = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
  return String(str).replace(/[&<>"']/g, (ch) => map[ch]);
};

async function main() {
  const { marked } = await import(path.join(VENDOR, 'marked.esm.js'));
  marked.use({
    gfm: true,
    breaks: true,
    renderer: {
      html(token) { return escapeHtml(token.text); },
    },
  });

  let failures = 0;
  const check = (desc, ok, detail) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}${!ok && detail ? ' — ' + detail : ''}`);
    if (!ok) failures++;
  };
  const parse = (text) => marked.parse(text, { async: false });

  // 1. Raw inline HTML renders as literal text — no element can exist.
  const inline = parse('The file contains <div id="x"> in it');
  check('raw inline HTML is escaped to literal text',
    inline.includes('&lt;div id=&quot;x&quot;&gt;'), inline);
  check('raw inline HTML produces no <div> element', !/<div/.test(inline), inline);

  // 2. Raw block HTML is escaped too.
  const block = parse('para\n\n<div class="x">block</div>');
  check('raw block HTML is escaped to literal text',
    block.includes('&lt;div class=&quot;x&quot;&gt;block&lt;/div&gt;'), block);

  // 3. Fenced code is untouched (a code token, not an html token): pre>code
  //    survives with a single-escaped '<' — no double escaping.
  const fence = parse('```go\nif a < b {}\n```');
  check('fence keeps pre>code', fence.includes('<pre><code class="language-go">'), fence);
  check('fence "<" single-escaped (no double escape)',
    fence.includes('if a &lt; b {}') && !fence.includes('&amp;lt;'), fence);

  // 4. Backtick inline code: single-escaped, no double escape.
  const codespan = parse('run `a < b` and `x <div>`');
  check('codespan single-escaped',
    codespan.includes('<code>a &lt; b</code>') && codespan.includes('<code>x &lt;div&gt;</code>')
      && !codespan.includes('&amp;lt;'), codespan);

  // 5. Markdown constructs still render.
  const md = parse('# Hi\n\n**bold** and [link](https://x.test) and *em*');
  check('headings render', md.includes('<h1>Hi</h1>'), md);
  check('bold/link/em render',
    md.includes('<strong>bold</strong>') && md.includes('<a href="https://x.test">link</a>')
      && md.includes('<em>em</em>'), md);

  // 6. Display/copy shows the literal source (entities decode in the DOM).
  const dom = new JSDOM('<body></body>');
  dom.window.document.body.innerHTML = inline;
  check('reader sees literal <div id="x"> text',
    dom.window.document.body.textContent.trim() === 'The file contains <div id="x"> in it',
    dom.window.document.body.textContent);
  check('zero elements created', dom.window.document.querySelectorAll('div').length === 0);

  // 7. Wiring pin: app.js still installs the override.
  const appJs = fs.readFileSync(path.join(ROOT, 'internal/server/web/app.js'), 'utf8');
  check('app.js renderer override present',
    appJs.includes('html(token) { return escapeHtml(token.text); }'));

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => {
  console.error('HARNESS ERROR:', err && err.stack || err);
  process.exit(1);
});
