// Standalone verification of the block-level colorize machinery
// (colorizeNode / enqueueColorize / cachedColorize, copied verbatim from
// editor.js) that the markdown block renderer drives. Guards the
// language-resolution path: a fence whose language is a Monaco registry
// ALIAS but not in the static LANG_ALIASES map (e.g. "xhtml" -> html) must
// resolve through the registry before tokenizing — colorize with an
// unregistered id yields no tokens and the result is cached as "done",
// leaving the block permanently uncolored (the reasoning-context regression
// this test pins).
//
// Run: node scripts/test_colorizenode.js
'use strict';

// applyColorized dispatches on window — provide minimal globals (no jsdom).
global.window = { dispatchEvent() {} };
global.CustomEvent = class { constructor(type) { this.type = type; } };

// ── Verbatim copies from editor.js (keep in sync) ──
const LANG_ALIASES = {
  js: 'javascript',
  jsx: 'javascript',
  mjs: 'javascript',
  cjs: 'javascript',
  ts: 'typescript',
  tsx: 'typescript',
  py: 'python',
  rb: 'ruby',
  sh: 'shell',
  bash: 'shell',
  zsh: 'shell',
  yml: 'yaml',
  md: 'markdown',
  golang: 'go',
  rs: 'rust',
  cs: 'csharp',
  csharp: 'csharp',
  kt: 'kotlin',
  plaintext: 'plaintext',
  text: 'plaintext',
  plain: 'plaintext',
  console: 'shell',
};

function normalizeColorizeHTML(html) {
  return html.replace(/<\s*br\s*\/?>\s*$/i, '');
}
const COLORIZE_CACHE_MAX = 300;
const colorizeCache = new Map();
const COLORIZE_SANITIZE_PROFILE = { USE_PROFILES: { html: true } };
// Test stub: mirrors the vendored DOMPurify used by editor.js (identity
// sanitize keeps the fake monaco output unchanged for the checks below).
const DOMPurify = { sanitize: (html) => html };

function cachedColorize(m, source, lang) {
  const key = lang + '\u0000' + source;
  const hit = colorizeCacheGet(key);
  if (hit !== undefined) return Promise.resolve(hit);
  return m.editor.colorize(source, lang, {}).then((raw) => {
    const html = DOMPurify.sanitize(normalizeColorizeHTML(raw), COLORIZE_SANITIZE_PROFILE);
    colorizeCachePut(key, html);
    return html;
  });
}

// LRU accessors (verbatim from editor.js): a hit re-inserts the key so
// re-used entries never age out behind one-shot ones.
function colorizeCacheGet(key) {
  const hit = colorizeCache.get(key);
  if (hit === undefined) return undefined;
  colorizeCache.delete(key);
  colorizeCache.set(key, hit);
  return hit;
}

function colorizeCachePut(key, html) {
  if (colorizeCache.size >= COLORIZE_CACHE_MAX) {
    const oldest = colorizeCache.keys().next().value;
    if (oldest !== undefined) colorizeCache.delete(oldest);
  }
  colorizeCache.set(key, html);
}

// Stand-in for the module-level `monaco` binding in editor.js (assigned by
// initMonaco); resolveMonacoLanguage reads it.
let monaco = null;

function resolveMonacoLanguage(langHint) {
  if (!monaco || !langHint) return null;
  let id = String(langHint).trim().toLowerCase();
  if (!id) return null;
  id = LANG_ALIASES[id] || id;
  const langs = monaco.languages.getLanguages();
  if (langs.some((l) => l.id === id)) return id;
  for (const l of langs) {
    if (l.aliases?.some((a) => String(a).toLowerCase() === id)) return l.id;
    if (l.extensions?.some((ext) => ext === `.${id}` || ext.slice(1).toLowerCase() === id)) {
      return l.id;
    }
  }
  return null;
}

function langIdFromClass(code) {
  const classMatch = /(?:^|\s)language-(\S+)/.exec(code.className || '');
  if (!classMatch) return null;
  const id = String(classMatch[1]).trim().toLowerCase();
  if (!id) return null;
  return LANG_ALIASES[id] || id;
}

function applyColorized(code, html) {
  code.innerHTML = html;
  code.dataset.monacoColorized = '1';
  code.classList.add('monaco-colorized');
  // Notify scroll system that DOM height may have changed.
  window.dispatchEvent(new CustomEvent('gogen-colorized', { bubbles: false }));
}

function sealStreamColorize(root) {
  if (!root || !root.querySelectorAll) return;
  for (const code of root.querySelectorAll('pre code')) {
    if (!code.dataset || code.dataset.monacoColorized !== '1') continue;
    const display = code.__gogenPlain !== undefined ? code.__gogenPlain : (code.textContent || '');
    if (!display) continue;
    const lang = langIdFromClass(code);
    if (!lang || lang === 'plaintext') continue;
    if (colorizeCache.has(lang + '\u0000' + display)) continue;
    colorizeCachePut(lang + '\u0000' + display, code.innerHTML);
  }
}

function enqueueColorize(code, source, lang) {
  return (async () => {
    try {
      const m = await initMonaco();
      if (!code.isConnected) return;
      const resolved = resolveMonacoLanguage(lang);
      if (!resolved || resolved === 'plaintext') return;
      const raw = await m.editor.colorize(source, resolved, {});
      if (!code.isConnected) return;
      // The element's plain text must still be the tokenized source (an
      // in-place patch/colorize that changed it invalidates the result).
      if (code.__gogenPlain !== undefined && code.__gogenPlain !== source) return;
      const html = DOMPurify.sanitize(normalizeColorizeHTML(raw), COLORIZE_SANITIZE_PROFILE);
      colorizeCachePut(resolved + '\u0000' + source, html);
      applyColorized(code, html);
    } catch (_) {
      // Unknown / unloaded language or init failure — leave plain text.
    }
  })();
}

// ── Colorize scheduler (verbatim from editor.js; keep in sync) ──
// In this standalone test IntersectionObserver is undefined, so the
// fallback path applies: jobs queue and drain immediately (the
// pre-viewport-gate behavior). The deferral/viewport behavior itself is
// covered by scripts/test_colorize_schedule.js.
const COLORIZE_MARGIN_PX = 800;
const COLORIZE_SLICE_MS = 12;
const COLORIZE_PARK_MAX = 400;

let colorizeDeferralTokens = [];
let colorizeDeferralSeq = 0;
let colorizeDeferralWaiters = [];
let colorizeJobs = [];
let colorizeDrainScheduled = false;
const colorizeParked = new Map();
let colorizeObserver = null;

function scheduleColorizeIdle(fn, timeout) {
  if (typeof requestIdleCallback === 'function') requestIdleCallback(fn, { timeout });
  else setTimeout(fn, 0);
}

function colorizeDeferralGate() {
  if (!colorizeDeferralTokens.length) return null;
  return new Promise((resolve) => colorizeDeferralWaiters.push(resolve));
}

function beginColorizeDeferral() {
  const token = ++colorizeDeferralSeq;
  colorizeDeferralTokens.push(token);
  return token;
}

function endColorizeDeferral(tokenEnd) {
  if (!colorizeDeferralTokens.length) return;
  if (tokenEnd === undefined) {
    colorizeDeferralTokens.pop();
  } else {
    const i = colorizeDeferralTokens.lastIndexOf(tokenEnd);
    if (i < 0) return;
    colorizeDeferralTokens.splice(i, 1);
  }
  if (colorizeDeferralTokens.length) return;
  const waiters = colorizeDeferralWaiters;
  colorizeDeferralWaiters = [];
  for (const resolve of waiters) resolve();
  drainColorizeJobs();
}

function releaseColorizeTargets() {
  colorizeParked.clear();
  colorizeJobs = [];
  if (colorizeObserver) {
    colorizeObserver.disconnect();
    colorizeObserver = null;
  }
}

function ensureColorizeObserver() {
  if (!colorizeObserver) {
    colorizeObserver = new IntersectionObserver((entries) => {
      let queued = false;
      for (const entry of entries) {
        if (!entry.isIntersecting) continue;
        const jobs = colorizeParked.get(entry.target);
        if (!jobs) continue;
        colorizeParked.delete(entry.target);
        colorizeObserver.unobserve(entry.target);
        for (const job of jobs) colorizeJobs.push({ el: entry.target, job });
        queued = true;
      }
      if (queued) drainColorizeJobs();
    }, { rootMargin: COLORIZE_MARGIN_PX + 'px 0px' });
  }
  return colorizeObserver;
}

function parkColorizeJob(el, job) {
  let jobs = colorizeParked.get(el);
  if (!jobs) {
    jobs = [];
    colorizeParked.set(el, jobs);
    ensureColorizeObserver().observe(el);
  }
  jobs.push(job);
}

function scheduleColorizeJob(el, job) {
  if (el && typeof IntersectionObserver === 'function') {
    if (colorizeParked.size >= COLORIZE_PARK_MAX && !colorizeParked.has(el)) {
      colorizeJobs.push({ el, job });
      drainColorizeJobs();
      return;
    }
    parkColorizeJob(el, job);
    return;
  }
  colorizeJobs.push({ el: el || null, job });
  drainColorizeJobs();
}

function drainColorizeJobs() {
  if (colorizeDrainScheduled || colorizeDeferralTokens.length || !colorizeJobs.length) return;
  colorizeDrainScheduled = true;
  const runSlice = async () => {
    colorizeDrainScheduled = false;
    const deadline = performance.now() + COLORIZE_SLICE_MS;
    while (colorizeJobs.length && performance.now() < deadline) {
      const { el, job } = colorizeJobs.shift();
      if (el && !el.isConnected) {
        if (typeof IntersectionObserver === 'function') parkColorizeJob(el, job);
        continue;
      }
      try {
        await job();
      } catch (_) {
      }
    }
    if (colorizeJobs.length) {
      colorizeDrainScheduled = true;
      scheduleColorizeIdle(runSlice, 100);
    }
  };
  scheduleColorizeIdle(runSlice, 400);
}

const STREAM_COLORIZE_INTERVAL_MS = 250;
const STREAM_COLORIZE_SLOWDOWN = 4;
const STREAM_COLORIZE_MAX_BYTES = 32 * 1024;

const streamColorize = new WeakMap(); // host node -> governor state

function streamGovernor(node) {
  let st = streamColorize.get(node);
  if (!st) {
    st = { inFlight: false, pending: false, lastAt: 0, lastCost: 0, timer: 0, memo: null };
    streamColorize.set(node, st);
  }
  return st;
}

function streamColorizeSource(code) {
  if (code.__gogenRaw !== undefined) return code.__gogenRaw;
  if (code.__gogenPlain !== undefined) return code.__gogenPlain;
  return code.textContent || '';
}

function applyStreamColored(code, html, source) {
  const display = code.__gogenPlain !== undefined ? code.__gogenPlain : (code.textContent || '');
  if (display !== source && !display.startsWith(source)) return false;
  applyColorized(code, html);
  const tail = display.slice(source.length);
  if (tail) code.appendChild(document.createTextNode(tail));
  return true;
}

function armStreamColorizeRetry(node, gov, wait) {
  if (gov.timer) return;
  gov.timer = setTimeout(() => {
    gov.timer = 0;
    colorizeNode(node, { streamingTail: true });
  }, wait);
}

function startStreamColorize(node, gov, code, lang, source) {
  gov.inFlight = true;
  gov.pending = false;
  gov.lastAt = Date.now();
  const started = gov.lastAt;
  (async () => {
    try {
      const m = await initMonaco();
      if (!code.isConnected) return;
      const resolved = resolveMonacoLanguage(lang);
      if (!resolved || resolved === 'plaintext') return;
      const raw = await m.editor.colorize(source, resolved, {});
      if (!code.isConnected) return;
      const html = DOMPurify.sanitize(normalizeColorizeHTML(raw), COLORIZE_SANITIZE_PROFILE);
      if (applyStreamColored(code, html, source)) {
        gov.memo = { lang, source, html };
      }
    } catch (_) {
      // Unknown / unloaded language or init failure — leave plain text.
    } finally {
      gov.lastCost = Date.now() - started;
      gov.inFlight = false;
      if (gov.pending) {
        gov.pending = false;
        colorizeNode(node, { streamingTail: true });
      }
    }
  })();
}

function colorizeNode(node, opts) {
  if (!node || !node.querySelectorAll) return;
  const codes = node.querySelectorAll('pre code');
  if (!codes.length) return;
  const streamingTail = !!(opts && opts.streamingTail);
  const gov = streamingTail ? streamGovernor(node) : null;

  for (const code of codes) {
    if (code.__gogenPlain === undefined) code.__gogenPlain = code.textContent || '';
    const lang = langIdFromClass(code);
    if (!lang || lang === 'plaintext') continue;

    if (!streamingTail) {
      const source = code.__gogenPlain;
      if (!source.trim()) continue;
      const key = lang + '\u0000' + source;
      const hit = colorizeCacheGet(key);
      if (hit !== undefined) {
        applyColorized(code, hit);
      } else {
        scheduleColorizeJob(code, () => enqueueColorize(code, source, lang));
      }
      continue;
    }

    const source = streamColorizeSource(code);
    if (!source.trim()) continue;
    // A closed fence's text is FINAL: publish the element's current colored
    // content under its displayed text, so the streaming→stable handoff
    // (promotion, end-of-stream full render) paints from a synchronous cache
    // hit instead of flashing plain while an async tokenize re-runs.
    if (code.dataset.monacoColorized === '1' && code.__gogenRaw !== undefined
        && code.__gogenRaw === code.__gogenPlain) {
      colorizeCachePut(lang + '\u0000' + code.__gogenPlain, code.innerHTML);
    }
    // Re-apply the governor's last result while its source still covers the
    // element's text (equal or a PREFIX of it); only an unchanged source
    // skips the tokenize entirely. See editor.js.
    const memo = gov.memo;
    let applied = false;
    if (memo && memo.lang === lang && (memo.source === source || source.startsWith(memo.source))) {
      if (applyStreamColored(code, memo.html, memo.source)) {
        applied = true;
        if (memo.source === source) continue;
      }
    }
    if (!applied) {
      const hit = colorizeCacheGet(lang + '\u0000' + source);
      if (hit !== undefined) {
        applyStreamColored(code, hit, source);
        continue;
      }
    }
    if (source.length > STREAM_COLORIZE_MAX_BYTES) continue;
    if (gov.inFlight) {
      gov.pending = true;
      continue;
    }
    const gap = Math.max(STREAM_COLORIZE_INTERVAL_MS, gov.lastCost * STREAM_COLORIZE_SLOWDOWN);
    const wait = gap - (Date.now() - gov.lastAt);
    if (wait > 0) {
      armStreamColorizeRetry(node, gov, wait);
      continue;
    }
    startStreamColorize(node, gov, code, lang, source);
  }
}
// ── End verbatim copies ──

// Test-controlled initMonaco: enqueueColorize (module scope) calls this
// name; main() re-points it at the fake monaco before running the checks.
let initMonaco = async () => { throw new Error('initMonaco not configured'); };

let failures = 0;
function check(name, cond, detail) {
  if (!cond) { failures++; console.log('FAIL:', name, detail || ''); }
  else console.log('ok  :', name);
}

// Minimal fake code element (colorizeNode touches className, textContent,
// innerHTML, dataset, classList, isConnected; the streaming apply path also
// appends a plain tail via appendChild + document.createTextNode).
function fakeCode(className, text) {
  const el = {
    className,
    textContent: text,
    dataset: {},
    isConnected: true,
    appended: [],
    _classes: new Set(),
    classList: {
      add: (c) => el._classes.add(c),
      contains: (c) => el._classes.has(c),
    },
    appendChild(node) { el.appended.push(node.data); return node; },
  };
  // innerHTML assignment REPLACES the element's children (real DOM
  // semantics), so the append log resets with it — otherwise a second
  // colorize apply (the coalesced retry) looks like it appended the tail
  // twice.
  let html = '';
  Object.defineProperty(el, 'innerHTML', {
    get: () => html,
    set: (v) => { html = v; el.appended.length = 0; },
  });
  return el;
}
const fakeNode = (codes) => ({ querySelectorAll: () => codes });
const flush = () => new Promise((r) => setTimeout(r, 5));
const wait = (ms) => new Promise((r) => setTimeout(r, ms));
// The append path needs document.createTextNode; this test is standalone
// (no jsdom), so provide the one call it makes.
global.document = { createTextNode: (data) => ({ nodeType: 3, data }) };

(async () => {
  // Fake Monaco: registered languages with their aliases; colorize with an
  // UNREGISTERED id yields plain output with no token spans (mimics the real
  // standalone colorize's empty-tokenization fallback). Records the lang id
  // each colorize call receives.
  const registered = {
    html: { aliases: ['HTML', 'htm', 'html', 'xhtml'] },
    python: { aliases: ['py', 'gyp', 'ipython'] },
    javascript: { aliases: ['js', 'es', 'jsx', 'mjs', 'cjs'] },
    go: { aliases: ['golang'] },
  };
  const tokenizeLangs = [];
  monaco = {
    languages: {
      getLanguages: () => Object.keys(registered).map((id) => ({ id, aliases: registered[id].aliases })),
    },
    editor: {
      colorize(source, lang) {
        tokenizeLangs.push(lang);
        if (!(lang in registered)) {
          return Promise.resolve(`<div>${source}</div><br/>`); // unknown lang: no tokens
        }
        return Promise.resolve(`<span class="mtk1">${lang}:${source}</span><br/>`);
      },
    },
  };
  initMonaco = async () => monaco;

  // 1. Alias-gap fence ("xhtml" is a Monaco alias for html, not in the
  //    static map): must resolve to html and produce token spans. This is
  //    the regression: without the registry resolution, colorize receives
  //    the unregistered "xhtml" and the block stays plain forever.
  const code1 = fakeCode('language-xhtml', '<div>hi</div>');
  colorizeNode(fakeNode([code1]));
  await flush();
  check('alias-gap fence (xhtml) resolves to html for colorize', tokenizeLangs[tokenizeLangs.length - 1] === 'html',
    'got: ' + tokenizeLangs[tokenizeLangs.length - 1]);
  check('alias-gap fence block colored (mtk spans)', code1.innerHTML.includes('mtk1') && code1.classList.contains('monaco-colorized'),
    code1.innerHTML.slice(0, 60));
  check('cache stored under resolved id (html\\0source)', colorizeCache.has('html\u0000<div>hi</div>'));
  check('cache NOT stored under static id (xhtml\\0source)', !colorizeCache.has('xhtml\u0000<div>hi</div>'));

  // 2. Unknown fence language: skipped (no tokenize, no class, no cache).
  const tokenizeBefore = tokenizeLangs.length;
  const code2 = fakeCode('language-foo', 'bar');
  colorizeNode(fakeNode([code2]));
  await flush();
  check('unknown fence skipped (no tokenize)', tokenizeLangs.length === tokenizeBefore);
  check('unknown fence left plain (no class)', !code2.classList.contains('monaco-colorized'));
  check('unknown fence not cached', !colorizeCache.has('foo\u0000bar'));

  // 3. Static-map-covered alias ("py" -> python) still resolves.
  const code3 = fakeCode('language-py', 'print(1)');
  colorizeNode(fakeNode([code3]));
  await flush();
  check('covered alias (py) resolves to python', tokenizeLangs[tokenizeLangs.length - 1] === 'python');
  check('covered alias block colored', code3.innerHTML.includes('mtk1'));

  // 4. Sync inline hit: same (lang, source) on a fresh element applies
  //    synchronously from the cache — no extra tokenize call.
  const tokenizeBefore2 = tokenizeLangs.length;
  const code4 = fakeCode('language-py', 'print(1)');
  colorizeNode(fakeNode([code4]));
  check('sync cache hit inlines immediately (no await needed)', code4.classList.contains('monaco-colorized'));
  check('sync cache hit does not re-tokenize', tokenizeLangs.length === tokenizeBefore2);

  // 5. Plaintext fences are skipped by design.
  const tokenizeBefore3 = tokenizeLangs.length;
  const code5 = fakeCode('language-plaintext', 'whatever');
  colorizeNode(fakeNode([code5]));
  await flush();
  check('plaintext fence skipped', tokenizeLangs.length === tokenizeBefore3 && !code5.classList.contains('monaco-colorized'));

  // 6. streamingTail (streaming-tail renders): a cache MISS must still
  //    tokenize in the background — the per-flush streaming colorization
  //    the pre-optimization code provided — but must NOT write the shared
  //    cache (the tail source is still growing, so the key can never be hit
  //    again and would only evict a useful entry). A cache HIT still
  //    applies synchronously.
  const tokenizeBefore4 = tokenizeLangs.length;
  const code6 = fakeCode('language-go', 'func main() {}');
  colorizeNode(fakeNode([code6]), { streamingTail: true });
  await flush();
  check('streamingTail miss tokenizes', tokenizeLangs.length === tokenizeBefore4 + 1);
  check('streamingTail result applied', code6.classList.contains('monaco-colorized') && code6.innerHTML.includes('mtk1'));
  check('streamingTail miss not cached', !colorizeCache.has('go\u0000func main() {}'));

  // 6c. Closed-fence publish: a fence whose code text is FINAL (raw ===
  //     plain) publishes the element's current colorization under its
  //     displayed text, so the streaming→stable handoff (block promotion,
  //     end-of-stream full render) paints from a synchronous hit instead of
  //     flashing plain. An open fence is still growing and must NOT publish
  //     (each prefix would only evict useful entries).
  const closedCode = fakeCode('language-go', '');
  closedCode.__gogenRaw = 'func main() {}\n';
  closedCode.__gogenPlain = 'func main() {}\n';
  closedCode.innerHTML = '<span class="mtk1">colored</span>';
  closedCode.dataset.monacoColorized = '1';
  colorizeNode(fakeNode([closedCode]), { streamingTail: true });
  check('closed fence publishes its colorization under the displayed text',
    colorizeCache.get('go\u0000func main() {}\n') === '<span class="mtk1">colored</span>');

  const openCode = fakeCode('language-go', '');
  openCode.__gogenRaw = 'func f() {';
  openCode.__gogenPlain = 'func f() {\n';
  openCode.innerHTML = '<span class="mtk1">func f() {</span>';
  openCode.dataset.monacoColorized = '1';
  colorizeNode(fakeNode([openCode]), { streamingTail: true });
  await flush();
  check('open fence does not publish a growing prefix',
    !colorizeCache.has('go\u0000func f() {\n'));

  // 6d. sealStreamColorize (the handoff hook markdown.js calls before it
  //     recreates elements): every colorized block is published under the
  //     text it displays, so the recreated element hits synchronously.
  const sealCode = fakeCode('language-python', 'print(9)');
  sealCode.__gogenPlain = 'print(9)';
  sealCode.innerHTML = '<span class="mtk1">sealed</span>';
  sealCode.dataset.monacoColorized = '1';
  sealStreamColorize(fakeNode([sealCode]));
  check('seal publishes a colorized element under its displayed text',
    colorizeCache.get('python\u0000print(9)') === '<span class="mtk1">sealed</span>');

  // 6b. A flush that re-renders the tail detaches the captured element
  //     while the tokenize is in flight: the stale result must be dropped.
  const colorizeFn = monaco.editor.colorize;
  let finishColorize = null;
  monaco.editor.colorize = (source, lang) => new Promise((resolve) => {
    finishColorize = () => resolve(`<span class="mtk1">${lang}:${source}</span><br/>`);
  });
  const code6b = fakeCode('language-go', 'func main() {}');
  colorizeNode(fakeNode([code6b]), { streamingTail: true });
  await flush(); // macrotask tick: the tokenize reaches its in-flight await
  code6b.isConnected = false; // next flush re-rendered: old element detached
  finishColorize();
  await flush();
  check('stale tail result dropped (element detached)', !code6b.classList.contains('monaco-colorized'));
  monaco.editor.colorize = colorizeFn;

  const code7 = fakeCode('language-go', 'func main() {}');
  colorizeNode(fakeNode([code7]));
  await flush();
  check('normal render warms the cache', colorizeCache.has('go\u0000func main() {}'));

  const code8 = fakeCode('language-go', 'func main() {}');
  colorizeNode(fakeNode([code8]), { streamingTail: true });
  check('streamingTail cache hit still applies synchronously', code8.classList.contains('monaco-colorized'));

  // ── 7. Governor: pacing ──
  // The first streamingTail call tokenizes immediately; a second call from
  // the next flush (well inside the interval) must NOT start another job —
  // it is paced and only arms a retry.
  const node7 = fakeNode([]);
  const code7a = fakeCode('language-go', 'paced');
  node7.querySelectorAll = () => [code7a];
  const tokenizeBefore7 = tokenizeLangs.length;
  colorizeNode(node7, { streamingTail: true });
  await flush();
  check('governor: first streamingTail call tokenizes', tokenizeLangs.length === tokenizeBefore7 + 1);
  check('governor: first result applied', code7a.classList.contains('monaco-colorized'));
  const code7b = fakeCode('language-go', 'paced more');
  node7.querySelectorAll = () => [code7b];
  colorizeNode(node7, { streamingTail: true });
  await flush();
  check('governor: a second flush within the interval does not tokenize (paced)',
    tokenizeLangs.length === tokenizeBefore7 + 1, 'tokenizes=' + (tokenizeLangs.length - tokenizeBefore7));
  // Detach node7 from the fake DOM: its armed retry must not add tokenizes
  // to the later checks' counts.
  node7.querySelectorAll = () => [];

  // ── 8. Governor: size cap ──
  const node8 = fakeNode([]);
  const bigCode = fakeCode('language-go', 'x'.repeat(STREAM_COLORIZE_MAX_BYTES + 1));
  node8.querySelectorAll = () => [bigCode];
  const tokenizeBefore8 = tokenizeLangs.length;
  colorizeNode(node8, { streamingTail: true });
  await flush();
  check('governor: sources past the cap stay plain (no tokenize)',
    tokenizeLangs.length === tokenizeBefore8 && !bigCode.classList.contains('monaco-colorized'));

  // ── 9. Governor: memo ──
  // After a result is applied, a NEW element carrying the same source (a
  // full re-render of an unchanged block) must repaint synchronously from
  // the memo, with the display tail re-appended and no tokenize.
  const node9 = fakeNode([]);
  const code9a = fakeCode('language-go', 'memo source');
  node9.querySelectorAll = () => [code9a];
  colorizeNode(node9, { streamingTail: true });
  await flush();
  check('governor: memo test setup tokenized', code9a.classList.contains('monaco-colorized'));
  const code9b = fakeCode('language-go', 'memo source');
  code9b.__gogenRaw = 'memo source';
  code9b.__gogenPlain = 'memo source\n'; // display form (marked's normalized newline)
  node9.querySelectorAll = () => [code9b];
  const tokenizeBefore9 = tokenizeLangs.length;
  colorizeNode(node9, { streamingTail: true });
  check('governor: memo hit applies synchronously (no tokenize)',
    code9b.classList.contains('monaco-colorized') && tokenizeLangs.length === tokenizeBefore9);
  check('governor: memo hit re-appends the display tail exactly',
    code9b.appended.join('') === '\n', JSON.stringify(code9b.appended.join('')));

  // ── 9b. Governor: memo PREFIX re-apply ──
  // A fenced shape the patcher cannot handle (a fence preceded by text, an
  // indented fence, CRLF) is fully re-rendered on every flush, so the fresh
  // element starts plain. The governor's last result must then be re-applied
  // SYNCHRONOUSLY whenever its source is still a prefix of the new text —
  // otherwise the block flashed plain until the next paced tokenize
  // (>=250ms) re-colored it: the reported "colored → plain → colored".
  {
    const node9b = fakeNode([]);
    const gov9b = streamGovernor(node9b);
    gov9b.memo = { lang: 'go', source: 'const a = 1;', html: '<span class="mtk1">const a = 1;</span>' };
    const code9c = fakeCode('language-go', '');
    code9c.__gogenPlain = 'const a = 1;\nconst b = 2;'; // grew since that tokenize
    node9b.querySelectorAll = () => [code9c];
    // Emulate a tokenize already in flight for this tail, so the check does
    // not queue one of its own (it would land inside later sections).
    gov9b.inFlight = true;
    colorizeNode(node9b, { streamingTail: true });
    check('memo prefix: colors re-applied synchronously (no plain gap)',
      code9c.classList.contains('monaco-colorized'), code9c.innerHTML.slice(0, 40));
    check('memo prefix: grown remainder re-appended as plain text',
      code9c.appended.join('') === '\nconst b = 2;', JSON.stringify(code9c.appended.join('')));
  }

  // ── 10. Governor: coalescing + append-and-reapply ──
  // While a tokenize is in flight, the element grows (a streaming patch).
  // The finished result must be applied as a colored prefix with the grown
  // remainder re-appended (never dropped), and the flush that arrived
  // during the job must coalesce into ONE trailing retry for the newest
  // text (paced, so it lands after the interval).
  const colorizeImpl = monaco.editor.colorize;
  const tokenizeSources = [];
  let blockFirst10 = true;
  let finish10 = null;
  monaco.editor.colorize = (source) => {
    tokenizeSources.push(source);
    if (blockFirst10) {
      blockFirst10 = false;
      return new Promise((resolve) => {
        finish10 = () => resolve('<span class="mtk1">colored</span><br/>');
      });
    }
    return Promise.resolve('<span class="mtk1">colored</span><br/>');
  };
  const node10 = fakeNode([]);
  const code10 = fakeCode('language-go', 'func a(');
  code10.__gogenRaw = 'func a(';
  code10.__gogenPlain = 'func a(\n';
  node10.querySelectorAll = () => [code10];
  colorizeNode(node10, { streamingTail: true }); // starts the (blocked) job
  await flush();
  check('governor: blocked job started',
    tokenizeSources.length === 1 && tokenizeSources[0] === 'func a(',
    JSON.stringify(tokenizeSources));
  // The stream grew while the tokenize ran (patch appended) and another
  // flush arrived.
  code10.__gogenRaw = 'func a(1)';
  code10.__gogenPlain = 'func a(1)\n';
  colorizeNode(node10, { streamingTail: true });
  finish10();
  await flush();
  check('governor: grown result applied as prefix + re-appended tail',
    code10.classList.contains('monaco-colorized') && code10.appended.join('') === '1)\n',
    'appended=' + JSON.stringify(code10.appended.join('')));
  await wait(300); // let the coalesced, paced retry land
  check('governor: coalesced retry tokenized the newest text',
    tokenizeSources[tokenizeSources.length - 1] === 'func a(1)',
    'last=' + JSON.stringify(tokenizeSources[tokenizeSources.length - 1]));
  monaco.editor.colorize = colorizeImpl;

  // ── 11. Governor: a rewritten element is never painted with stale tokens ──
  const node11 = fakeNode([]);
  const code11 = fakeCode('language-go', 'original text');
  code11.__gogenRaw = 'original text';   // tokenized source at job start
  code11.__gogenPlain = 'rewritten text'; // display no longer extends it
  node11.querySelectorAll = () => [code11];
  colorizeNode(node11, { streamingTail: true });
  await flush();
  check('governor: rewritten text never gets stale tokens applied',
    !code11.classList.contains('monaco-colorized'));

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
})().catch((err) => { console.error('TEST ERROR:', err); process.exit(2); });
