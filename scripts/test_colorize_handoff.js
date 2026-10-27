'use strict';
// Regression test for the streaming→stable colorize HANDOFF in the markdown
// renderer (the user-visible "colored → plain → colored" flash):
//
//   - A code block colored while streaming goes through the governor, which
//     deliberately never writes the shared LRU (its source is still growing,
//     so each prefix key would only evict useful entries).
//   - When the block is recreated — a block promotion mid-stream, or the
//     end-of-stream full render — the recreated element starts plain, so
//     without a handoff it flashed plain until an async tokenize re-ran for
//     text that was already colored.
//   - markdown.js now seals the streaming DOM into the shared LRU before it
//     recreates anything (sealStreamColorize), so the recreated element
//     paints from a synchronous cache hit.
//
// The editor's colorize machinery is stubbed by faithful copies of its cache
// and seal CONTRACT (key shape `lang\0text`, synchronous hit for a cached
// pair, no LRU writes from the streaming path) — not the full alias map or
// Monaco. A stable element whose text is not cached stays plain: that plain
// state IS the flash this test detects.
//
// Run: node scripts/test_colorize_handoff.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { stripModuleSyntax } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
const MARKDOWN_JS = path.join(ROOT, 'internal/server/web/components/markdown.js');

async function main() {
  const dom = new JSDOM('<body></body>', { runScripts: 'outside-only' });
  const { window } = dom;
  globalThis.window = window;
  globalThis.document = window.document;

  const { marked } = await import('file://' + path.join(ROOT, 'internal/server/web/vendor/marked.esm.js'));
  const DOMPurifyMod = await import('file://' + path.join(ROOT, 'internal/server/web/vendor/dompurify.esm.js'));
  const DOMPurify = DOMPurifyMod.default;

  window.marked = marked;
  window.DOMPurify = DOMPurify;
  window.openFileAtLine = async () => {};

  // ── Colorize contract stubs (mirror editor.js; keep in sync) ──
  const colorizeCache = new Map();
  const langOf = (code) => {
    const m = /(?:^|\s)language-(\S+)/.exec(code.className || '');
    return m ? m[1].trim().toLowerCase() : null;
  };
  // editor.js sealStreamColorize: publish the colorized DOM under its text.
  const sealTargets = [];
  window.sealStreamColorize = (root) => {
    sealTargets.push(root);
    if (!root || !root.querySelectorAll) return;
    for (const code of root.querySelectorAll('pre code')) {
      if (!code.dataset || code.dataset.monacoColorized !== '1') continue;
      const display = code.__gogenPlain !== undefined ? code.__gogenPlain : (code.textContent || '');
      if (!display) continue;
      const lang = langOf(code);
      if (!lang || lang === 'plaintext') continue;
      if (colorizeCache.has(lang + '\u0000' + display)) continue;
      colorizeCache.set(lang + '\u0000' + display, code.innerHTML);
    }
  };
  // editor.js colorizeNode: a stable element with a cached (lang, text) pair
  // is colored synchronously; a miss stays plain (the async tokenize the test
  // does not run). The streaming tail is colored in place WITHOUT writing the
  // cache, exactly like the governor.
  window.colorizeNode = (node, opts) => {
    if (!node || !node.querySelectorAll) return;
    for (const code of node.querySelectorAll('pre code')) {
      if (code.__gogenPlain === undefined) code.__gogenPlain = code.textContent || '';
      if (code.dataset.monacoColorized === '1') continue;
      const lang = langOf(code);
      if (!lang || lang === 'plaintext') continue;
      if (opts && opts.streamingTail) {
        code.innerHTML = '<span class="mtk1">' + code.__gogenPlain
          .replace(/&/g, '&amp;').replace(/</g, '&lt;') + '</span>';
        code.dataset.monacoColorized = '1';
        continue;
      }
      const hit = colorizeCache.get(lang + '\u0000' + code.__gogenPlain);
      if (hit === undefined) continue; // async path: stays plain
      code.innerHTML = hit;
      code.dataset.monacoColorized = '1';
    }
  };

  window.eval(stripModuleSyntax(fs.readFileSync(MARKDOWN_JS, 'utf8')));
  const rawStore = new WeakMap();
  window.initMarkdown({
    showToast() {},
    copyTextToClipboard: async () => true,
    getMessageRawStore: () => rawStore,
  });
  const render = window.renderStreamMarkdown;
  const full = window.setMessageMarkdown;
  const mkEl = () => window.document.createElement('div');
  let failures = 0;
  const check = (desc, ok, detail) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}${!ok && detail ? ' — ' + detail : ''}`);
    if (!ok) failures++;
  };

  // 1. Promotion handoff: the fence is colored while streaming; the closer,
  //    the blank line and the next paragraph then arrive in ONE burst, so
  //    promotion happens with no flush that could have re-tokenized the
  //    block. The seal must carry the color into the promoted element
  //    synchronously.
  {
    const el = mkEl();
    render(el, '```js\nconst x = 1;');
    const tailCode = el.querySelector('.md-tail pre code');
    check('streaming: the tail block is colored', !!tailCode && tailCode.dataset.monacoColorized === '1');
    sealTargets.length = 0;
    render(el, '```js\nconst x = 1;\n```\n\nnext paragraph');
    const promoted = el.querySelector('.md-block:not(.md-tail) pre code');
    check('promotion: the promoted block is colored SYNCHRONOUSLY (no flash)',
      !!promoted && promoted.dataset.monacoColorized === '1',
      promoted ? promoted.outerHTML.slice(0, 100) : 'no promoted code element');
    // Perf contract: the promotion seal must target the TAIL node (what is
    // about to be recreated), never the whole message — sealing the message
    // would re-serialize every colored block at every block boundary, which
    // made the stream slow down as the answer grew.
    check('promotion: the seal targets the tail node, not the whole message',
      sealTargets.length > 0 && sealTargets.every((n) => n !== el),
      'targets=' + sealTargets.map((n) => (n === el ? 'message' : (n && n.className) || 'null')).join(','));
    check('promotion: the next paragraph is the in-flight tail',
      !!el.querySelector('.md-tail') && /next paragraph/.test(el.querySelector('.md-tail').textContent));
  }

  // 2. End-of-stream handoff: the last fence is still open when the stream
  //    ends; setMessageMarkdown re-renders the whole message and must keep it
  //    colored synchronously.
  {
    const el = mkEl();
    const open = '```js\nconst y = 2;';
    render(el, open);
    sealTargets.length = 0;
    full(el, open);
    const code = el.querySelector('pre code');
    check('end of stream: the full re-render keeps the block colored SYNCHRONOUSLY',
      !!code && code.dataset.monacoColorized === '1',
      code ? code.outerHTML.slice(0, 100) : 'no code element');
    check('end of stream: the seal targets the message being replaced',
      sealTargets.includes(el));
  }

  // 3. Control: a block never colored during streaming has no sealed entry,
  //    so the stable render stays plain until the async tokenize — the
  //    boundary this fix deliberately does not change.
  {
    const el = mkEl();
    full(el, '```js\nconst z = 3;\n```');
    const code = el.querySelector('pre code');
    check('control: an uncolored block stays plain (async path unchanged)',
      !!code && code.dataset.monacoColorized !== '1');
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