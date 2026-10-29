'use strict';
// Regression test for the colorize defects found in the web UI. Everything is
// driven against the REAL editor.js + markdown.js + app.js (only the Monaco
// surface is stubbed) — the older colorize tests run verbatim COPIES of these
// functions, which is why none of this was caught.
//
//  1. THE CACHE KEY SPLIT. A fence the static alias map does not know
//     ("c++" -> cpp, "c#" -> csharp, "xhtml"/"htm" -> html, "es6" ->
//     javascript) was written to the shared LRU under the RESOLVED id but
//     looked up under the static one, so the lookup missed forever and the
//     block was re-tokenized and repainted (plain -> colored) on every
//     re-render. One key function (colorizeStamp) is used by every producer
//     and consumer.
//
//  2. THE PER-FLUSH REWRITE. colorizeNode re-applied the streaming governor's
//     memo through `innerHTML = html` on EVERY flush, even when the element
//     already showed exactly that rendering. Elements stamp what they show
//     (markColorized) and writers skip an identical write.
//
//  3. THE LOST RESTORE. Clearing another pane's transcript clears the SHARED
//     park map and queue; a cached pane's transcript is alive but detached, so
//     its blocks came back plain and never colorized. restorePaneTranscript
//     re-derives the colorization for the restored subtree.
//
//  4. THE FROZEN TAIL. sealStreamColorize published a partially colored
//     element under its FULL display text; the stable path then treated that
//     as final and never re-tokenized — the block stayed half colored. Sealed
//     entries now record their coverage and the stable path finishes the block
//     in the background (without flashing).
//
//  5. THE DEAD 250ms FLOOR. Pacing is a CPU-share budget now: real hardware
//     tokenizes a 2 KB block in ~0.6ms, so ordinary blocks re-color with the
//     text instead of trailing it by a quarter second.
//
// Run: node scripts/test_colorize_reapply.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs, stripModuleSyntax } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
const WEB = path.join(ROOT, 'internal/server/web');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Viewport gate: a fake IntersectionObserver whose intersections the test
// fires (jsdom has none, and the real gate would park every job forever).
function installFakeIO(w) {
  const instances = [];
  class FakeIO {
    constructor(cb) { this.cb = cb; this.targets = new Set(); instances.push(this); }
    observe(el) { this.targets.add(el); }
    unobserve(el) { this.targets.delete(el); }
    disconnect() { this.targets.clear(); }
  }
  w.IntersectionObserver = FakeIO;
  return {
    flushAll() {
      for (const io of instances) {
        const entries = [...io.targets].filter((t) => t.isConnected).map((t) => ({ target: t, isIntersecting: true }));
        if (!entries.length) continue;
        for (const e of entries) io.targets.delete(e.target);
        io.cb(entries, io);
      }
    },
  };
}

// Boots the real web UI (index.html + components + app.js) with the real
// editor.js colorize machinery and one fake Monaco surface whose tokenize
// takes `colorizeMs` and emits one span per line, like Monaco does.
async function bootApp({ colorizeMs = 0 } = {}) {
  const html = fs.readFileSync(path.join(WEB, 'index.html'), 'utf8');
  const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
  const w = dom.window;
  const doc = w.document;

  w.matchMedia = w.matchMedia || (() => ({ matches: false, addEventListener() {}, removeEventListener() {} }));
  w.Notification = { permission: 'denied', requestPermission: () => Promise.resolve('denied') };
  w.navigator.clipboard = { writeText: async () => {} };
  w.HTMLElement.prototype.scrollIntoView = function () {};
  w.Terminal = class {
    constructor() { this._cbs = {}; }
    open() {} write() {} fit() {}
    onData(cb) { this._cbs.data = cb; }
    onResize() { return { dispose() {} }; }
    dispose() {} reset() {} focus() {}
  };
  w.FitAddon = class { fit() {} };
  w.WebSocket = class { constructor() { this.readyState = 1; } static OPEN = 1; send() {} close() {} };
  // Identity sanitize: the tokenizer stub emits escaped text, and the real
  // DOMPurify profile is only about class/span survival anyway.
  w.DOMPurify = { sanitize: (h) => h };
  const io = installFakeIO(w);

  // Real editor.js FIRST: the harness's editor stubs keep whatever already
  // exists, so the real colorize machinery wins and only the unrelated editor
  // surface (setupEditorUI, connectEditorSocket, …) is stubbed.
  w.eval(stripModuleSyntax(fs.readFileSync(path.join(WEB, 'components/storage.js'), 'utf8')));
  w.eval(stripModuleSyntax(fs.readFileSync(path.join(WEB, 'editor.js'), 'utf8')));
  installEditorStubs(w);
  w.openModal = (o) => o.classList.add('active');
  w.closeModal = (o) => o.classList.remove('active');
  // REAL marked: markdown.js must actually produce <pre><code> for these
  // paths (a stub would render plain text and the test would prove nothing).
  const markedMod = await import('file://' + path.join(WEB, 'vendor/marked.esm.js'));
  w.marked = markedMod.marked;
  loadAppJs(w);

  const stats = { colorize: 0, langs: [] };
  w.initMonaco = async () => ({
    editor: {
      colorize: async (source, lang) => {
        stats.colorize++;
        stats.langs.push(lang);
        if (colorizeMs) await sleep(colorizeMs);
        return String(source).split('\n')
          .map((l) => '<span class="mtk1">' + l.replace(/&/g, '&amp;').replace(/</g, '&lt;') + '</span>')
          .join('<br>');
      },
    },
    languages: { getLanguages: () => [] },
  });
  // Alias-gap resolver: "c++" is a Monaco alias of cpp (the static map in
  // editor.js has no entry for it). Everything else resolves to itself.
  w.resolveMonacoLanguage = (hint) => {
    const id = String(hint || '').trim().toLowerCase();
    if (!id) return null;
    if (id === 'c++') return 'cpp';
    if (id === 'plaintext' || id === 'text') return 'plaintext';
    return id;
  };
  w.disposeChatEditors();
  dom.window.__stats = stats;
  return { w, doc, io, stats };
}

// A .md-tail block whose code element is ready to stream (the patcher keeps
// __gogenPlain / __gogenRaw in step with the DOM).
function makeTail(w, lang, source) {
  const block = w.document.createElement('div');
  block.className = 'md-block md-tail';
  const pre = w.document.createElement('pre');
  const code = w.document.createElement('code');
  code.className = 'language-' + lang;
  code.textContent = source;
  pre.appendChild(code);
  block.appendChild(pre);
  w.document.getElementById('messages').appendChild(block);
  code.__gogenPlain = source;
  code.__gogenRaw = source;
  return { block, code };
}

function makeBlock(w, lang, source) {
  const block = w.document.createElement('div');
  block.className = 'md-block';
  const pre = w.document.createElement('pre');
  const code = w.document.createElement('code');
  code.className = 'language-' + lang;
  code.textContent = source;
  pre.appendChild(code);
  block.appendChild(pre);
  w.document.getElementById('messages').appendChild(block);
  return { block, code };
}

const isColored = (code) => !!code && code.dataset.monacoColorized === '1';

let failures = 0;
const check = (desc, ok, detail) => {
  console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}${detail ? ' — ' + detail : ''}`);
  if (!ok) failures++;
};

async function main() {
  // ── 1. One cache key for producers and consumers (alias-gap fence) ──
  {
    const { w, io, stats } = await bootApp();
    check('the REAL colorizeNode is loaded (not the harness stub)', w.colorizeNode.length >= 2,
      'arity ' + w.colorizeNode.length);
    const src = 'int main() { return 0; }\n';
    const a = makeBlock(w, 'c++', src);
    w.colorizeNode(a.block);
    io.flushAll();
    await sleep(60);
    const first = stats.colorize;
    check('alias-gap fence colorizes on its first render', first === 1 && isColored(a.code), String(first));
    a.block.remove();
    const b = makeBlock(w, 'c++', src);
    w.colorizeNode(b.block);
    io.flushAll();
    await sleep(60);
    check('re-render of the same alias-gap fence is a synchronous cache hit (no second tokenize)',
      stats.colorize === first, `${first} -> ${stats.colorize}`);
    check('the re-rendered alias-gap block is painted', isColored(b.code));
    b.block.remove();

    // Control: a fence the static map already covers behaves the same.
    const goSrc = 'package main\n';
    const c = makeBlock(w, 'go', goSrc);
    w.colorizeNode(c.block);
    io.flushAll();
    await sleep(60);
    const before = stats.colorize;
    c.block.remove();
    const d = makeBlock(w, 'go', goSrc);
    w.colorizeNode(d.block);
    io.flushAll();
    await sleep(60);
    check('control: a covered fence also hits the cache on re-render', stats.colorize === before,
      `${before} -> ${stats.colorize}`);
    d.block.remove();

    // ── 2. An unchanged streaming flush must not rebuild the colored DOM ──
    const streamSrc = 'let x = 1;\n';
    const { block, code } = makeTail(w, 'go', streamSrc);
    w.colorizeNode(block, { streamingTail: true });
    await sleep(80);
    check('streaming block colorizes once', isColored(code));
    const firstChild = code.firstChild;
    const tokenizes = stats.colorize;
    w.colorizeNode(block, { streamingTail: true }); // a stall / visibility flush
    check('a same-text flush does not rewrite the colored subtree', code.firstChild === firstChild);
    check('a same-text flush does not tokenize again', stats.colorize === tokenizes,
      `${tokenizes} -> ${stats.colorize}`);
    const grown = streamSrc + 'let y = 2;\n';
    code.__gogenPlain = grown;
    code.__gogenRaw = grown;
    code.appendChild(w.document.createTextNode('let y = 2;\n'));
    w.colorizeNode(block, { streamingTail: true }); // the tail patcher's append
    check('a grown flush keeps the colored prefix (no subtree rebuild)', code.firstChild === firstChild);
    // NOTE: Monaco-shaped output separates lines with <br>, whose textContent
    // is empty — compare the text with line breaks removed (this is also why
    // the chat Copy button, which reads code.textContent, needs __gogenPlain).
    check('a grown flush preserves the element text',
      code.textContent.replace(/\n/g, '') === grown.replace(/\n/g, ''),
      JSON.stringify(code.textContent.slice(-24)));
    block.remove();

    // ── 3. A repeat render of the same STABLE element writes nothing ──
    const stable = makeBlock(w, 'go', 'package again\n');
    w.colorizeNode(stable.block);
    io.flushAll();
    await sleep(60);
    check('stable block colorized on its first render', isColored(stable.code));
    const child = stable.code.firstChild;
    const stableTokenizes = stats.colorize;
    w.colorizeNode(stable.block);
    io.flushAll();
    await sleep(40);
    check('a repeat render of the same stable element does not rewrite it', stable.code.firstChild === child);
    check('a repeat render of the same stable element does not tokenize',
      stats.colorize === stableTokenizes, `${stableTokenizes} -> ${stats.colorize}`);
    stable.block.remove();

    // ── 4. A restore re-derives colorization the park map lost ──
    const container = w.document.createElement('div');
    const block2 = w.document.createElement('div');
    block2.className = 'md-block';
    const pre2 = w.document.createElement('pre');
    const code2 = w.document.createElement('code');
    code2.className = 'language-go';
    code2.textContent = 'package other\n';
    pre2.appendChild(code2);
    block2.appendChild(pre2);
    container.appendChild(block2);
    w.disposeChatEditors(); // what clearChat does for a different pane
    check('the cached block starts plain', !isColored(code2));
    const restored = w.restorePaneTranscript({
      id: 'sess-restored',
      domCache: {
        container,
        sessionId: 'sess-restored',
        scrollTop: 0,
        pinned: true,
        historyToolCallArgs: {},
      },
    });
    check('restorePaneTranscript restores the cache', restored === true);
    io.flushAll();
    await sleep(80);
    check('a restored pane re-colorizes its blocks', isColored(code2));
  }

  // ── 5. Pacing: a cheap tokenize colors with the text, an expensive one backs off ──
  {
    const { w, stats } = await bootApp({ colorizeMs: 1 });
    const { block, code } = makeTail(w, 'go', 'const a = 0;\n');
    const flushes = 16;
    let src = 'const a = 0;\n';
    for (let i = 0; i < flushes; i++) {
      // Each flush brings new text, the way the stream does.
      const delta = 'const a = ' + (i + 1) + ';\n';
      src += delta;
      code.__gogenPlain = src;
      code.__gogenRaw = src;
      code.appendChild(w.document.createTextNode(delta));
      w.colorizeNode(block, { streamingTail: true });
      await sleep(32);
    }
    // Old policy was one start per 250ms ⇒ ~2 in this window.
    check('a cheap block re-colors with the text (not on the old 250ms floor)',
      stats.colorize >= 4, `tokenizes in ~${flushes * 32}ms: ${stats.colorize}`);
    block.remove();
  }
  {
    const { w, stats } = await bootApp({ colorizeMs: 100 });
    const { block, code } = makeTail(w, 'go', 'const a = 0;\n');
    let src = 'const a = 0;\n';
    for (let i = 0; i < 20; i++) {
      const delta = 'const a = ' + (i + 1) + ';\n';
      src += delta;
      code.__gogenPlain = src;
      code.__gogenRaw = src;
      code.appendChild(w.document.createTextNode(delta));
      w.colorizeNode(block, { streamingTail: true });
      await sleep(32);
    }
    // 100ms of work earns a ~667ms gap: ~2 starts in 640ms, never 20.
    check('an expensive tokenize backs off instead of saturating the thread',
      stats.colorize <= 3, 'tokenizes in ~640ms: ' + stats.colorize);
    block.remove();
  }

  // ── 6. Stream end FINISHES the block instead of freezing a partial ──
  {
    const { w, io, stats } = await bootApp({ colorizeMs: 60 });
    const el = w.document.createElement('div');
    el.className = 'message';
    w.document.body.appendChild(el);
    const line = (i) => 'const v' + i + ' = ' + i + ';\n';
    let text = '```go\n';
    for (let i = 0; i < 6; i++) text += line(i);
    w.renderStreamMarkdown(el, text);
    await sleep(120); // first tokenize lands; the memo covers 6 lines
    for (let i = 6; i < 30; i++) {
      text += line(i);
      w.renderStreamMarkdown(el, text);
      await sleep(8); // faster than the budget gap, so the memo lags behind
    }
    const beforeEnd = stats.colorize;
    w.setMessageMarkdown(el, text + '```\n'); // stream end
    const codeAtEnd = el.querySelector('pre code');
    check('the stream-end render paints immediately (no blank block)', isColored(codeAtEnd));
    io.flushAll(); // the completion job is parked on the viewport gate
    await sleep(600);
    const code = el.querySelector('pre code');
    const spans = code.querySelectorAll('span.mtk1').length;
    const display = code.__gogenPlain || '';
    const lines = display.split('\n').filter((l) => l.length).length;
    // "Frozen" leaves the un-tokenized remainder as a trailing plain text node
    // (that is what applyStreamColored appends); a finished block ends on a
    // colored span.
    const last = code.lastChild;
    const plainTail = !!(last && last.nodeType === 3 && last.data.trim());
    check('a tokenize runs after the stream ends to finish the block',
      stats.colorize > beforeEnd, `${beforeEnd} -> ${stats.colorize}`);
    check('the finished block is colored to the end (no frozen prefix)',
      !plainTail && spans >= lines + 1 && lines >= 30,
      `${spans} spans for ${lines} lines, plain tail: ${plainTail}`);
    el.remove();
  }

  console.log(failures === 0 ? '\ntest_colorize_reapply: all checks passed' : `\n${failures} check(s) failed`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((e) => { console.error(e); process.exit(1); });
