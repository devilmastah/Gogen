'use strict';
// The "Copy" button on a chat code block must copy the SOURCE text.
//
// Monaco's colorize output separates lines with <br/> (the vendored
// standaloneColorizer emits "<br/>"), and textContent of <br/> is empty — so a
// HIGHLIGHTED block's textContent has no newlines at all. The handler used to
// read textContent, which copied the code with every line break stripped.
// __gogenPlain carries the authoritative plain text (renderBlockHtml sets it,
// the streaming tail patcher keeps it in step).
//
// Run: node scripts/test_code_copy_plain.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { stripModuleSyntax } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
const VENDOR = path.join(ROOT, 'internal/server/web/vendor');
const MARKDOWN_JS = path.join(ROOT, 'internal/server/web/components/markdown.js');

async function main() {
  const dom = new JSDOM('<body></body>', { runScripts: 'outside-only' });
  const { window } = dom;
  globalThis.window = window;
  globalThis.document = window.document;

  const { marked } = await import('file://' + path.join(VENDOR, 'marked.esm.js'));
  const DOMPurify = (await import('file://' + path.join(VENDOR, 'dompurify.esm.js'))).default;
  window.marked = marked;
  window.DOMPurify = DOMPurify;
  // Colorization stays out of this test: the DOM is put into the state a
  // colorized block is left in, which is all the copy handler can see.
  window.colorizeNode = () => {};
  window.sealStreamColorize = () => {};
  window.openFileAtLine = async () => {};
  window.eval(stripModuleSyntax(fs.readFileSync(MARKDOWN_JS, 'utf8')));

  let copied = null;
  const rawStore = new WeakMap();
  window.initMarkdown({
    showToast() {},
    copyTextToClipboard: async (text) => { copied = text; return true; },
    getMessageRawStore: () => rawStore,
  });

  let failures = 0;
  const check = (desc, ok, detail) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}${detail ? ' — ' + detail : ''}`);
    if (!ok) failures++;
  };

  const el = window.document.createElement('div');
  el.className = 'message';
  window.setMessageMarkdown(el, '```go\npackage main\n\nfunc main() {}\n```\n');
  const code = el.querySelector('pre code');
  check('code block rendered', !!code && !!code.__gogenPlain, code ? JSON.stringify(code.__gogenPlain) : 'missing');

  // What Monaco leaves behind: one span per line, joined by <br/>.
  code.innerHTML = code.__gogenPlain.split('\n')
    .map((l) => '<span class="mtk1">' + l + '</span>')
    .join('<br/>');
  code.dataset.monacoColorized = '1';
  check('a highlighted block has no newlines in textContent (the bug’s mechanism)',
    !code.textContent.includes('\n'), JSON.stringify(code.textContent));

  const btn = el.querySelector('.code-copy-btn');
  check('copy button rendered', !!btn);
  btn.click();
  await new Promise((r) => setTimeout(r, 0));

  check('Copy takes the source text from __gogenPlain',
    copied === code.__gogenPlain,
    JSON.stringify(copied) + ' vs ' + JSON.stringify(code.__gogenPlain));
  check('the copied text keeps its line breaks', !!copied && copied.includes('\n'));
  check('the copied text is the code, not the span markup',
    !!copied && !copied.includes('<span') && copied.includes('func main() {}'));

  // Regression guard: an UNCOLORED block (no colorization yet) still copies.
  const el2 = window.document.createElement('div');
  el2.className = 'message';
  window.setMessageMarkdown(el2, '```go\nvar x = 1\n```\n');
  copied = null;
  el2.querySelector('.code-copy-btn').click();
  await new Promise((r) => setTimeout(r, 0));
  check('an uncolored block still copies its text', copied === 'var x = 1\n', JSON.stringify(copied));

  console.log(failures === 0 ? '\ntest_code_copy_plain: all checks passed' : `\n${failures} check(s) failed`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((e) => { console.error(e); process.exit(1); });
