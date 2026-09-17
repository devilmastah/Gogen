// Monaco editor workspace for GoGen web UI.
// DOMPurify here is the SAME module instance components/markdown.js imports:
// both use this identical root-absolute specifier, and the ESM module map is
// keyed by resolved URL, so the editor path adds no second fetch/evaluation
// of the 28.8 KB (≈11 KB gzip) vendor file. It is needed for the sanitized
// Monaco colorize output (cachedColorize / enqueueColorize), and the chat
// path needs it anyway (markdown.js sanitizes every message body), so it
// stays a static import rather than a lazy one. See index.html's
// modulepreload hint and scripts/test_modulepreload.js (board #101).
import DOMPurify from '/vendor/dompurify.esm.js';
// Shared decision-dialog plumbing (components/dialog.js). It imports this
// module's openModal/closeModal — a safe ESM cycle, see dialog.js's header.
import { openDialog } from '/components/dialog.js';
// Safe localStorage access (see storage.js): reads of persisted prefs below
// run at module top level, where an unguarded access would throw in a
// storage-blocked browser and abort the import graph.
import { storageGet, storageSet } from '/components/storage.js';

let monaco = null;

// Must be set before any editor/worker is created.
self.MonacoEnvironment = {
  getWorker(_workerId, label) {
    const map = {
      json: '/monaco/json.worker.js',
      css: '/monaco/css.worker.js',
      scss: '/monaco/css.worker.js',
      less: '/monaco/css.worker.js',
      html: '/monaco/html.worker.js',
      handlebars: '/monaco/html.worker.js',
      razor: '/monaco/html.worker.js',
      typescript: '/monaco/ts.worker.js',
      javascript: '/monaco/ts.worker.js',
    };
    const url = map[label] || '/monaco/editor.worker.js';
    return new Worker(url, { type: 'module' });
  },
};

export const GOGEN_UI = {
  // Flip to false for inline (unified-style) DiffEditor rendering. Applies
  // to the EDITOR pane's git-diff view only — chat tool-card diffs are
  // always unified and use the separate "Chat diff viewer" setting.
  // Persisted so the choice survives reloads.
  diffRenderSideBySide: storageGet('gogen_diff_layout') !== 'inline',
  maxOpenTabs: 20,
};

const buffers = new Map(); // path -> { model, viewState, savedVersionId, lastUsed }
let openOrder = []; // paths in tab order
let activePath = null;
let mode = 'edit'; // 'edit' | 'diff'
let editor = null;
let diffEditor = null;
let monacoReady = false;
let monacoInitPromise = null;
let wsRef = null;
let reqCounter = 0;
const pendingReqs = new Map();
const chatEditors = new Set(); // disposable Monaco editors in chat tool cards
let toastFn = null;
let searchDebounceTimer = null;
let searchGen = 0;
// Find-in-files options (persisted): findCaseSensitive is the "Match case"
// toggle (default ON, matching the server's default case-sensitive search);
// findGlob is the include-files filter sent as the fs_search glob.
let findCaseSensitive = storageGet('gogen_find_case') !== '0';
let findGlob = storageGet('gogen_find_glob') || '';
let diffStatText = ''; // "+N −M" summary for the unstaged-diff pane
let diffChangeCount = 0; // number of hunks in the unstaged-diff pane (nav buttons)
let refDecorationIds = []; // range-highlight decorations from chat references
const markerCounts = new Map(); // path -> { errors, warnings }
// Branch/upstream/ahead/behind from the last git_status reply, shown in the
// status bar. refreshGitStatus fills it; null when there is no repo or the
// last status call failed.
let gitBranch = null;
// The path the currently-open tab context menu was opened on (null while the
// menu is closed). Set by openTabMenu; consumed by runTabMenuAction.
let tabMenuPath = null;
// Paths of folders currently expanded in the editor's file tree. The tree is
// rebuilt from scratch by refreshExplorer (socket reconnect, pane switch,
// refresh button); only this set survives a rebuild — loadTree restores
// expansion from it so a refresh never collapses folders the user opened.
const expandedDirs = new Set();

function $(id) {
  return document.getElementById(id);
}

export function setToastHandler(fn) {
  toastFn = typeof fn === 'function' ? fn : null;
}

function toast(message, kind = 'info') {
  if (toastFn) toastFn(message, kind);
}

// ── Modal focus management (shared by chat + editor overlays) ──
// openModal shows an overlay, moves focus inside it, traps Tab within it and
// remembers the trigger element so closeModal can hand focus back. Each modal
// adds its own Esc handling on top (dismiss = non-destructive action).
let modalLastFocus = null;

// While any modal is open the app surface behind it (the tab bar and the
// panes) is made inert, so Tab and assistive tech cannot reach content a
// modal is logically covering. The overlays are siblings of these two
// containers, so they stay interactive. A Set (not a count/boolean) so
// stacked modals — a delete approval popping over the settings overlay —
// only restore the background when the LAST one closes, and re-opening an
// already-open overlay never double-counts.
const openModals = new Set();
const MODAL_BACKGROUND_SELECTORS = ['#top-tabs', '#main-panes'];

function setBackgroundInert(inert) {
  for (const sel of MODAL_BACKGROUND_SELECTORS) {
    const el = document.querySelector(sel);
    if (!el) continue;
    if (inert) el.setAttribute('inert', '');
    else el.removeAttribute('inert');
  }
}

function markModalOpen(overlay) {
  if (openModals.has(overlay)) return;
  openModals.add(overlay);
  if (openModals.size === 1) setBackgroundInert(true);
}

function markModalClosed(overlay) {
  if (!openModals.delete(overlay)) return;
  if (openModals.size === 0) setBackgroundInert(false);
}

function focusablesOf(container) {
  if (!container) return [];
  const sel = 'button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [href], [tabindex]:not([tabindex="-1"])';
  return [...container.querySelectorAll(sel)].filter((n) => n.offsetParent !== null);
}

function trapFocusIn(overlay) {
  const onKeydown = (e) => {
    if (e.key !== 'Tab') return;
    const focusables = focusablesOf(overlay);
    if (!focusables.length) return;
    const first = focusables[0];
    const last = focusables[focusables.length - 1];
    if (e.shiftKey && (document.activeElement === first || !overlay.contains(document.activeElement))) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  };
  overlay.addEventListener('keydown', onKeydown);
  return () => overlay.removeEventListener('keydown', onKeydown);
}

export function openModal(overlay, opts = {}) {
  if (!overlay) return;
  if (modalLastFocus === null) modalLastFocus = document.activeElement;
  overlay.classList.add(opts.className || 'active');
  overlay.setAttribute('aria-hidden', 'false');
  // Mark the overlay as a modal and inert the app surface behind it (see
  // openModals above). aria-modal tells assistive tech the rest of the page
  // is off-limits; inert enforces the same for the keyboard.
  overlay.setAttribute('aria-modal', 'true');
  markModalOpen(overlay);
  if (overlay.__gogenTrap) overlay.__gogenTrap();
  overlay.__gogenTrap = trapFocusIn(overlay);
  const target = opts.focusSelector ? overlay.querySelector(opts.focusSelector) : null;
  const focusables = focusablesOf(overlay);
  const el = target || (focusables.length ? focusables[0] : null);
  if (el && el.focus) el.focus();
}

export function closeModal(overlay, opts = {}) {
  if (!overlay) return;
  overlay.classList.remove(opts.className || 'active');
  overlay.setAttribute('aria-hidden', 'true');
  overlay.removeAttribute('aria-modal');
  markModalClosed(overlay);
  if (overlay.__gogenTrap) {
    overlay.__gogenTrap();
    overlay.__gogenTrap = null;
  }
  const target = modalLastFocus;
  modalLastFocus = null;
  if (target && target.focus && document.contains(target)) {
    try { target.focus(); } catch (_) {}
  }
}

export function handleServerMessage(data) {
  if (!data || !data.requestId || !pendingReqs.has(data.requestId)) return false;
  const p = pendingReqs.get(data.requestId);
  pendingReqs.delete(data.requestId);
  if (data.error) p.reject(new Error(data.error));
  else p.resolve(data);
  return true;
}

// ── Editor WebSocket (/ws/editor) ──
// The editor runs on its own socket, separate from the chat socket, so
// editor requests (fs/git) never queue behind chat turns and a streaming
// session cannot stall editor saves. The socket reconnects itself and
// repaints the explorer on (re)connect.

let editorReconnectTimer = null;

function editorSocketURL() {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${protocol}//${window.location.host}/ws/editor`;
}

/**
 * Open (or reopen) the editor WebSocket. Safe to call repeatedly; each call
 * replaces the previous socket's handlers. On (re)connect the file tree and
 * git status are refreshed so a dropped socket never leaves a stale explorer.
 */
export function connectEditorSocket() {
  clearTimeout(editorReconnectTimer);
  editorReconnectTimer = null;
  const ws = new WebSocket(editorSocketURL());
  wsRef = ws;

  ws.onopen = () => {
    // Repaint the explorer so a reconnect (or first load) reflects the
    // server's current tree/git state. Best-effort: Monaco may not be
    // initialized yet, but the tree is plain DOM and does not need it.
    refreshExplorer().catch(() => {});
  };

  ws.onmessage = (event) => {
    let data;
    try {
      data = JSON.parse(event.data);
    } catch (err) {
      console.error('Failed to parse editor message:', err);
      return;
    }
    handleServerMessage(data);
  };

  ws.onclose = () => {
    wsRef = null;
    // Reject any in-flight requests so callers fail fast instead of hanging.
    for (const [, p] of pendingReqs) {
      p.reject(new Error('editor connection lost'));
    }
    pendingReqs.clear();
    editorReconnectTimer = setTimeout(connectEditorSocket, 3000);
  };

  ws.onerror = () => {
    // onclose follows; nothing else to do here.
  };
}

// Per-request timeout so a request can never hang forever when the server
// never replies (stuck fs/git op). Slow operations (git, search, replace,
// write) pass the longer window. A timed-out request may still complete
// server-side; callers just stop waiting and surface the error.
const WS_REQUEST_TIMEOUT_MS = 15000;
const WS_REQUEST_SLOW_TIMEOUT_MS = 30000;

function wsRequest(type, payload = {}, timeoutMs = WS_REQUEST_TIMEOUT_MS) {
  return new Promise((resolve, reject) => {
    if (!wsRef || wsRef.readyState !== WebSocket.OPEN) {
      reject(new Error('not connected'));
      return;
    }
    const requestId = `ed-${++reqCounter}`;
    const timer = setTimeout(() => {
      pendingReqs.delete(requestId);
      reject(new Error(`editor request timed out (${type})`));
    }, timeoutMs);
    // Wrap resolve/reject so the timer is cleared whichever way the request
    // settles (reply dispatch or onclose); handleServerMessage/onclose stay
    // unchanged.
    pendingReqs.set(requestId, {
      resolve: (v) => { clearTimeout(timer); resolve(v); },
      reject: (e) => { clearTimeout(timer); reject(e); },
    });
    wsRef.send(JSON.stringify({ type, requestId, ...payload }));
  });
}

export async function initMonaco() {
  if (monacoReady) return monaco;
  // Never start the (multi-MB) import in the middle of a history replay: the
  // transcript paints first, colors follow (see the colorize scheduler).
  // Gating initMonaco itself — not just the colorize jobs — means no caller
  // (a diff viewer, the editor pane, a colorize job) can jump the queue.
  const gate = colorizeDeferralGate();
  if (gate) await gate;
  if (monacoReady) return monaco;
  if (monacoInitPromise) return monacoInitPromise;

  // Dynamic import: avoids blocking the WebSocket connection on a 3.8 MB download.
  monacoInitPromise = (async () => {
    // Load the editor stylesheet alongside the JS bundle so the Chat-only
    // first paint never pays for 128KB of Monaco CSS. Once-only guard.
    // Must wait for the CSS to finish loading before creating any editor,
    // otherwise the diff viewer renders with wrong sizing and scrollbars.
    let cssPromise = Promise.resolve();
    if (!document.getElementById('monaco-editor-css')) {
      const link = document.createElement('link');
      link.id = 'monaco-editor-css';
      link.rel = 'stylesheet';
      link.href = '/monaco/editor.main.css';
      cssPromise = new Promise((resolve) => {
        link.onload = () => resolve();
        link.onerror = () => resolve(); // proceed anyway on error
        // Safety timeout: never block more than 5s waiting for CSS.
        setTimeout(() => resolve(), 5000);
      });
      document.head.appendChild(link);
    }
    // Start the JS download in parallel but don't proceed until CSS is ready.
    const jsPromise = import('/monaco/editor.bundle.js');
    await cssPromise;
    const mod = await jsPromise;
    monaco = mod.default;
    // Monaco 0.52+ no longer ships a built-in unified-diff highlighter.
    if (!monaco.languages.getLanguages().some((l) => l.id === 'diff')) {
      monaco.languages.register({ id: 'diff' });
    }
    monaco.languages.setMonarchTokensProvider('diff', {
      tokenizer: {
        root: [
          [/^\+\+\+.*$/, 'meta.diff.header'],
          [/^---.*$/, 'meta.diff.header'],
          [/^diff .*$/, 'meta.diff'],
          [/^index .*$/, 'comment'],
          [/^@@.*@@.*$/, 'meta.diff.hunk'],
          [/^\+.*$/, 'markup.inserted'],
          [/^-.*$/, 'markup.deleted'],
        ],
      },
    });

    // Strong diff colors for unified patches (language tokens + decorations).
    monaco.editor.defineTheme('gogen-dark', {
      base: 'vs-dark',
      inherit: true,
      rules: [
        { token: 'comment', foreground: '6A9955' },
        { token: 'meta.diff', foreground: '569CD6' },
        { token: 'meta.diff.header', foreground: '569CD6', fontStyle: 'bold' },
        { token: 'meta.diff.hunk', foreground: 'C586C0' },
        { token: 'markup.inserted', foreground: '4EC9B0' },
        { token: 'markup.deleted', foreground: 'F14C4C' },
      ],
      colors: {
        // Match the app chrome (--bg in styles.css :root) so editor and
        // diff surfaces blend with the page instead of showing the
        // vs-dark default #1e1e1e.
        'editor.background': '#131418',
        'diffEditor.insertedTextBackground': '#2ea04340',
        'diffEditor.removedTextBackground': '#f8514940',
        'diffEditor.insertedLineBackground': '#2ea04326',
        'diffEditor.removedLineBackground': '#f8514926',
        'editorGutter.addedBackground': '#2ea043',
        'editorGutter.deletedBackground': '#f85149',
        'editorGutter.modifiedBackground': '#d29922',
      },
    });
    monaco.editor.defineTheme('gogen-light', {
      base: 'vs',
      inherit: true,
      rules: [
        { token: 'comment', foreground: '008000' },
        { token: 'meta.diff', foreground: '0066CC' },
        { token: 'meta.diff.header', foreground: '0066CC', fontStyle: 'bold' },
        { token: 'meta.diff.hunk', foreground: 'A626A4' },
        { token: 'markup.inserted', foreground: '1A7F37' },
        { token: 'markup.deleted', foreground: 'CF222E' },
      ],
      colors: {
        // Match the app chrome (--bg in styles.css :root.light).
        'editor.background': '#f7f8fa',
        'diffEditor.insertedTextBackground': '#dafbe180',
        'diffEditor.removedTextBackground': '#ffebe980',
        'diffEditor.insertedLineBackground': '#dafbe140',
        'diffEditor.removedLineBackground': '#ffebe940',
        'editorGutter.addedBackground': '#1a7f37',
        'editorGutter.deletedBackground': '#cf222e',
        'editorGutter.modifiedBackground': '#9a6700',
      },
    });
    monaco.editor.setTheme('gogen-dark');
    // Language services push diagnostics asynchronously; refresh tab badges
    // and the status bar whenever markers change.
    monaco.editor.onDidChangeMarkers(() => refreshTabBadges());
    monacoReady = true;
    return monaco;
  })();

  try {
    return await monacoInitPromise;
  } catch (err) {
    monacoInitPromise = null;
    throw err;
  }
}

/**
 * Switch the Monaco theme between dark and light.
 * Safe to call before initMonaco has completed — the call is a no-op until monaco is ready.
 */
export function setMonacoTheme(useLight) {
  if (!monaco) return;
  monaco.editor.setTheme(useLight ? 'gogen-light' : 'gogen-dark');
}

// Common fence aliases → Monaco language ids.
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

/** Map a file path to a Monaco language id (mirrors server languageFromPath). */
export function languageFromPath(path) {
  if (!path) return 'plaintext';
  const base = String(path).split(/[/\\]/).pop() || '';
  const dot = base.lastIndexOf('.');
  const ext = dot >= 0 ? base.slice(dot).toLowerCase() : '';
  switch (ext) {
    case '.go':
    case '.mod':
      return 'go';
    case '.js':
    case '.mjs':
    case '.cjs':
    case '.jsx':
      return 'javascript';
    case '.ts':
    case '.tsx':
      return 'typescript';
    case '.json':
      return 'json';
    case '.md':
    case '.markdown':
      return 'markdown';
    case '.html':
    case '.htm':
      return 'html';
    case '.css':
      return 'css';
    case '.scss':
      return 'scss';
    case '.less':
      return 'less';
    case '.yaml':
    case '.yml':
      return 'yaml';
    case '.toml':
      return 'ini';
    case '.xml':
      return 'xml';
    case '.sh':
    case '.bash':
    case '.zsh':
      return 'shell';
    case '.py':
      return 'python';
    case '.rs':
      return 'rust';
    case '.java':
      return 'java';
    case '.c':
    case '.h':
      return 'c';
    case '.cpp':
    case '.cc':
    case '.cxx':
    case '.hpp':
      return 'cpp';
    case '.cs':
      return 'csharp';
    case '.sql':
      return 'sql';
    case '.rb':
      return 'ruby';
    case '.php':
      return 'php';
    case '.swift':
      return 'swift';
    case '.kt':
      return 'kotlin';
    case '.lua':
      return 'lua';
    case '.r':
      return 'r';
    case '.diff':
    case '.patch':
      return 'diff';
    default:
      return 'plaintext';
  }
}

/**
 * Monaco's colorize appends a <br/> after every line, including the last, so
 * its output is always one (empty) line taller than the plain <pre> render.
 * Drop that trailing break so the colorized and plain states have identical
 * line structure — otherwise swapping between them mid-stream changes the
 * block's height and makes pinned auto-scroll bounce.
 */
function normalizeColorizeHTML(html) {
  return html.replace(/<\s*br\s*\/?>\s*$/i, '');
}

// Cache of Monaco colorize output keyed by `lang\0source`. During streaming
// the message DOM is re-rendered every flush, which recreates every code
// element — without this cache each flush would re-tokenize every completed
// code block. Tokenization is deterministic for a given (language, source)
// pair, so a cached result is always valid for the same text.
const COLORIZE_CACHE_MAX = 300;
const colorizeCache = new Map();
// Sanitize profile applied once at cache-fill, so every consumer (inline
// fast path, async applies, tool results) gets purified HTML from this
// single choke point. Monaco's token spans are class-based, and `class`/
// `span` survive the html profile, so the painted output is unchanged.
const COLORIZE_SANITIZE_PROFILE = { USE_PROFILES: { html: true } };

// LRU accessors for colorizeCache. Map iteration order is insertion order,
// so a cache HIT re-inserts the key (delete + set) to refresh its recency,
// and eviction removes the first key — the least recently used. Under plain
// FIFO, entries that are re-used on every flush (the same block rendered
// repeatedly) could age out behind one-shot entries that are never read
// again.
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

/**
 * Tokenize `source` for `lang` via Monaco, returning normalized HTML.
 * Completed blocks (unchanged text between flushes) hit the cache and skip
 * tokenization entirely; only growing in-flight blocks re-tokenize. Results
 * are DOMPurify-sanitized once before caching.
 */
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

// ── Colorize scheduling: replay deferral + viewport gate ──
// Tokenizing pulls in the ~4.5MB Monaco bundle, so WHEN colorize work runs
// matters as much as how fast it is:
//
//   - beginColorizeDeferral()/endColorizeDeferral() bracket a history replay
//     (app.js). While a deferral is active, jobs queue and initMonaco itself
//     waits, so no caller can start the import mid-replay: a restored
//     session paints first, colors follow.
//   - a shared IntersectionObserver parks each job until its element is near
//     the viewport (COLORIZE_MARGIN_PX), so a restored session tokenizes only
//     what the user can see or is about to scroll to. A detached subtree — a
//     cached pane's transcript — stays parked until it is restored to the
//     document; releaseColorizeTargets drops the parked work when the chat
//     DOM is discarded (clearChat).
//   - queued jobs drain in ~12ms idle slices, the same discipline the
//     tool-result colorize queue uses.
const COLORIZE_MARGIN_PX = 800;
const COLORIZE_SLICE_MS = 12;
// Safety valve for the parked set: past this many distinct elements, jobs run
// eagerly (drain-queued) instead of parking, so a very large session cannot
// leave code permanently plain while the observer's retained set stays
// bounded. Far beyond any ordinary transcript's uncolorized code-block count.
// A job whose element is DETACHED is parked regardless of the cap (it cannot
// run now, and only the observer can wake it when its subtree is restored);
// the map shrinks again as soon as those elements reconnect.
const COLORIZE_PARK_MAX = 400;

// Active deferrals, oldest first. They are TOKENS, not a counter: a replay
// that overran the watchdog is ended by the watchdog (newest token popped),
// and its own finally must not end a NEWER replay's deferral when it
// eventually runs — endColorizeDeferral ignores a token that is no longer
// active.
let colorizeDeferralTokens = [];
let colorizeDeferralSeq = 0;
let colorizeDeferralWaiters = [];
let colorizeJobs = [];            // ready to run: [{ el, job }]
let colorizeDrainScheduled = false;
const colorizeParked = new Map(); // Element -> [job, …] waiting on the observer
let colorizeObserver = null;

function scheduleColorizeIdle(fn, timeout) {
  if (typeof requestIdleCallback === 'function') requestIdleCallback(fn, { timeout });
  else setTimeout(fn, 0);
}

// Promise that resolves when the active deferral ends (null when none is
// active). initMonaco awaits it; the colorize queue refuses to drain.
function colorizeDeferralGate() {
  if (!colorizeDeferralTokens.length) return null;
  return new Promise((resolve) => colorizeDeferralWaiters.push(resolve));
}

/** Bracket a batch of transcript rendering (a history replay) so no Monaco
 * import or tokenization starts while it builds the DOM. Returns the token to
 * pass back to endColorizeDeferral, so a replay that overran the watchdog
 * cannot end a newer replay's deferral when its finally eventually runs. */
export function beginColorizeDeferral() {
  const token = ++colorizeDeferralSeq;
  colorizeDeferralTokens.push(token);
  return token;
}

/** End a deferral; the last one releases initMonaco's waiters and drains the
 * queued jobs in idle slices. tokenEnd identifies the caller's own deferral (a
 * token that is no longer active — the watchdog already ended it — is
 * ignored); no token ends the newest active deferral (the watchdog path).
 * Idempotent when no deferral is active. */
export function endColorizeDeferral(tokenEnd) {
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

/** Drop parked jobs and stop observing their elements: the transcript DOM
 * they belong to is going away (clearChat), so they can never become
 * visible work. */
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

// Queue one colorize job. Without IntersectionObserver support (older
// browsers, the standalone node tests) the queue runs immediately — the
// pre-viewport-gate behavior.
// Park one job on the observer, ignoring the cap. Used for work that has
// nowhere else to go: the cap exists to stop READY jobs from accumulating,
// not to drop a job whose element is detached — and re-entering
// scheduleColorizeJob from the drain (park → overflow → queue → drain →
// detached → park) loops forever once the cap is reached.
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
      // Overflow: run it through the queue (the pre-viewport-gate behavior)
      // rather than leaving the block permanently plain.
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
        // Its subtree was detached mid-drain (a pane cache): park it again so
        // it colorizes when the transcript is restored. Unconditional parking
        // (not scheduleColorizeJob) is required: at the park cap that path
        // would re-queue the job and loop drain → detached → queue forever.
        if (typeof IntersectionObserver === 'function') parkColorizeJob(el, job);
        continue;
      }
      try {
        await job();
      } catch (_) {
        // Keep going: one failed tokenize must not strand the rest.
      }
    }
    if (colorizeJobs.length) {
      colorizeDrainScheduled = true;
      scheduleColorizeIdle(runSlice, 100);
    }
  };
  scheduleColorizeIdle(runSlice, 400);
}

/**
 * Colorize a single element's textContent with Monaco.
 * langHint may be a language id or a file path.
 *
 * Routed through the scheduler: deferred during a history replay and parked
 * until the element is near the viewport.
 */
export async function colorizeElement(el, langHint) {
  if (!el) return;
  scheduleColorizeJob(el, () => runColorizeElement(el, langHint));
}

// The colorize body proper: generation-stamped and applied only while the
// element is still current and connected.
async function runColorizeElement(el, langHint) {
  const gen = (el._gogenHlGen = (el._gogenHlGen || 0) + 1);
  let m;
  try {
    m = await initMonaco();
  } catch (err) {
    console.warn('monaco colorize init failed', err);
    return;
  }
  if (el._gogenHlGen !== gen || !el.isConnected) return;

  let lang = resolveMonacoLanguage(langHint);
  if (!lang && langHint && String(langHint).includes('.')) {
    lang = resolveMonacoLanguage(languageFromPath(langHint));
  }
  if (!lang || lang === 'plaintext') return;

  const source = el.textContent || '';
  if (!source.trim()) return;

  try {
    // Shared token cache: repeated renders of the same source skip
    // tokenization, and results are purified at cache-fill.
    const html = await cachedColorize(m, source, lang);
    if (el._gogenHlGen !== gen || !el.isConnected) return;
    el.innerHTML = html;
    el.dataset.monacoColorized = '1';
    el.classList.add('monaco-colorized');
    // Notify scroll system that DOM height may have changed.
    window.dispatchEvent(new CustomEvent('gogen-colorized', { bubbles: false }));
  } catch (_) {
    // Unknown / unloaded language — leave plain text.
  }
}

/**
 * Language id for a fenced `<code class="language-X">` element, resolved via
 * the static alias map only (no Monaco needed, so the synchronous cache
 * fast path works before the bundle loads). The async path re-resolves via
 * Monaco's full registry (resolveMonacoLanguage), which covers alias-gap
 * fences (e.g. "xhtml" → html, "c++" → cpp) the static map lacks. For
 * languages the static map covers, the id here equals the resolved id, so a
 * sync hit is always valid; for alias-gap languages the sync lookup simply
 * misses and the async path stores under the canonical id.
 */
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

/**
 * Publish every colorized code block under `root` to the shared LRU, keyed by
 * the text it currently displays. The streaming governor deliberately never
 * writes the LRU (its source grows, so every write would be a one-shot key
 * evicting useful entries) — but an element's current content IS a valid
 * rendering of its displayed text, so sealing it lets the render that
 * REPLACES the streaming DOM (a block promotion, the end-of-stream full
 * render, thinking finalize, edit restore) paint from a synchronous cache hit
 * instead of flashing plain while a tokenize re-runs for text that was
 * already colored.
 *
 * Callers: markdown.js seals the message before promoting completed blocks
 * and before setMessageMarkdown wipes it.
 */
export function sealStreamColorize(root) {
  if (!root || !root.querySelectorAll) return;
  for (const code of root.querySelectorAll('pre code')) {
    if (!code.dataset || code.dataset.monacoColorized !== '1') continue;
    const display = code.__gogenPlain !== undefined ? code.__gogenPlain : (code.textContent || '');
    if (!display) continue;
    const lang = langIdFromClass(code);
    if (!lang || lang === 'plaintext') continue;
    // Already published (an earlier promotion or full render): the recreated
    // element will hit that entry, so re-serializing this element's
    // span-heavy HTML would be pure cost. Map.has, not colorizeCacheGet —
    // this must not disturb LRU order.
    if (colorizeCache.has(lang + '\u0000' + display)) continue;
    colorizeCachePut(lang + '\u0000' + display, code.innerHTML);
  }
}

// Background tokenize for one code element whose content is FINAL (a
// completed block or a single-shot render). The result is applied only if
// the element is still in the document — a re-rendered block replaces its
// code elements, so element identity is the staleness signal (no
// generation counter needed). The sanitized result goes into the shared
// LRU (the key is the exact source, so only an identical completed block
// can ever hit it).
function enqueueColorize(code, source, lang) {
  return (async () => {
    try {
      const m = await initMonaco();
      if (!code.isConnected) return;
      // Resolve through Monaco's full registry (alias scan included) before
      // tokenizing: the static map in langIdFromClass covers only the common
      // fences, and colorize with an unregistered id yields no tokens (plain
      // output, cached as "done" — permanently uncolored). Alias-gap fences
      // like "xhtml" (→ html), "c++" (→ cpp), "es" (→ javascript) must be
      // resolved here; unknown ids resolve to null and stay plain, matching
      // the pre-merge colorizeCodeBlocks behavior.
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

// ── Streaming-tail colorize governor ──
// A streaming code block is re-rendered or patched every ~32ms flush, and
// tokenizing the whole (growing) block per flush is quadratic in its
// length — with most old results thrown away, because the flush replaced
// the very element the tokenize was for. The governor bounds that work:
//
//   - ONE tokenize per tail node in flight; a request that arrives while
//     one runs is coalesced into a single trailing retry that reads the
//     element's text at retry time (always the newest content),
//   - at most one START per interval, stretched to 4x the previous
//     tokenize's duration, so colorize can never saturate the main thread,
//   - sources past STREAM_COLORIZE_MAX_BYTES stay plain until the block is
//     completed or finalized (those paths colorize once, cached),
//   - the last applied (lang, source) result is remembered per node, so a
//     block re-materialized by a full render with unchanged text is
//     repainted synchronously without tokenizing again.
//
// The result is applied as a colored PREFIX plus the untouched plain
// suffix: the tokenize may finish after more text arrived, so the element's
// current text is preserved verbatim (re-appended as a text node) instead
// of discarding the result — every paid tokenize becomes visible.
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

// The text a streaming code element should be tokenized from: the markdown
// source in `__gogenRaw` when the streaming patch renderer maintains it
// (the raw fence content, without marked's normalized trailing newline),
// else the element's displayed plain text.
function streamColorizeSource(code) {
  if (code.__gogenRaw !== undefined) return code.__gogenRaw;
  if (code.__gogenPlain !== undefined) return code.__gogenPlain;
  return code.textContent || '';
}

// Apply a streaming colorize result: the colored HTML covers `source` (a
// prefix of the element's current plain text), and any grown remainder is
// re-appended as plain text, so the element's text is preserved exactly —
// never a gap, never a duplicate. Returns false (nothing applied) when the
// element's text no longer extends `source` (a rewrite/replacement).
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
        // A flush arrived while this tokenize ran: re-gate immediately for
        // the newest text (the pacing gate still applies, so this can only
        // arm a retry, never start back-to-back tokenizes).
        gov.pending = false;
        colorizeNode(node, { streamingTail: true });
      }
    }
  })();
}

/**
 * Syntax-highlight fenced code blocks under `node` (a completed .md-block,
 * the streaming tail, or a full .msg-text render). Runs synchronously:
 * already-cached (lang, source) pairs are inlined immediately — a single
 * DOM write, no async — and only uncached blocks tokenize in the
 * background.
 *
 * For stable nodes (completed blocks, single-shot renders) the content is
 * final and the shared LRU cache applies as before.
 *
 * opts.streamingTail (the in-flight tail, re-rendered or patched every
 * flush): unbounded per-flush tokenization is exactly what the governor
 * above exists to prevent. Uncached code tokenizes through it — paced,
 * coalesced, size-capped, memoized — and each result is applied as a
 * colored prefix with the grown suffix preserved (applyStreamColored). The
 * shared LRU is never written from here: a still-growing source can never
 * be hit again and would only evict useful entries.
 */
export function colorizeNode(node, opts) {
  if (!node || !node.querySelectorAll) return;
  const codes = node.querySelectorAll('pre code');
  if (!codes.length) return;
  const streamingTail = !!(opts && opts.streamingTail);
  const gov = streamingTail ? streamGovernor(node) : null;

  for (const code of codes) {
    // Authoritative plain text for the element. Monaco's <br/> line
    // separators make textContent unusable as a source after a result has
    // been applied, and the streaming patch renderer keeps this in step
    // with the DOM it appends to.
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
        // Queued through the scheduler: no Monaco import mid-replay, and
        // no tokenize for code that is not near the viewport.
        scheduleColorizeJob(code, () => enqueueColorize(code, source, lang));
      }
      continue;
    }

    const source = streamColorizeSource(code);
    if (!source.trim()) continue;
    // A closed fence's text is FINAL: publish the element's current colored
    // content under its displayed text, so a handoff that re-renders the
    // block (a burst promotion, the end-of-stream full render) paints from a
    // synchronous cache hit instead of flashing plain while an async
    // tokenize re-runs for text that was already colored. `raw === plain`
    // means the fence closed (the raw code text then carries the newline
    // before the closer); an open fence is still growing, and publishing each
    // of its prefixes would only evict useful entries.
    if (code.dataset.monacoColorized === '1' && code.__gogenRaw !== undefined
        && code.__gogenRaw === code.__gogenPlain
        && !colorizeCache.has(lang + '\u0000' + code.__gogenPlain)) {
      // Map.has guard: a closed fence can sit in the tail across several
      // flushes, and re-serializing its span-heavy innerHTML every flush
      // would be pure cost once published.
      colorizeCachePut(lang + '\u0000' + code.__gogenPlain, code.innerHTML);
    }
    // Re-apply the governor's last result whenever its source still covers
    // the element's text — equal, or a PREFIX of it (the element grew since
    // that tokenize). This is what keeps a full re-render of the tail from
    // wiping the colors: the patcher cannot handle every fenced shape (a
    // fence preceded by text, an indented fence, CRLF), and those shapes
    // rebuild the whole tail element on every flush. Without the prefix
    // re-apply the fresh element started plain and only the next PACED
    // tokenize (>=250ms) colored it again — the "colored → plain → colored"
    // flash. The grown remainder re-appends as plain text, exactly the state
    // the patch path maintains.
    const memo = gov.memo;
    let applied = false;
    if (memo && memo.lang === lang && (memo.source === source || source.startsWith(memo.source))) {
      if (applyStreamColored(code, memo.html, memo.source)) {
        applied = true;
        if (memo.source === source) continue; // unchanged: nothing new to tokenize
      }
    }
    if (!applied) {
      // Sync cache hits still apply instantly (a closed fence's source can
      // match a cached completed block); the governor only owns the tokenize
      // path. Never a cache WRITE from here: the source is still growing, so
      // the key would only evict useful entries.
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

// Class of a unified-diff line (meta/hunk/add/del), or null. Shared by the
// full and append-only decoration passes and the fallback renderer's
// applyDiffLineClass twin.
function diffLineClass(text) {
  if (text.startsWith('+++') || text.startsWith('---') || text.startsWith('diff ') || text.startsWith('index ')) {
    return 'gogen-diff-meta';
  }
  if (text.startsWith('@@')) return 'gogen-diff-hunk';
  if (text.startsWith('+')) return 'gogen-diff-add';
  if (text.startsWith('-')) return 'gogen-diff-del';
  return null;
}

/** Colorize unified-diff lines via decorations (works even if language tokens are missing). */
export function applyUnifiedDiffDecorations(ed) {
  if (!ed) return;
  const model = ed.getModel();
  if (!model) return;
  const lineCount = model.getLineCount();
  const decorations = [];
  const decoLines = [];
  for (let i = 1; i <= lineCount; i++) {
    const cls = diffLineClass(model.getLineContent(i));
    if (!cls) continue;
    decorations.push({
      range: new monaco.Range(i, 1, i, model.getLineMaxColumn(i)),
      options: {
        isWholeLine: true,
        className: cls,
        marginClassName: cls + '-margin',
      },
    });
    decoLines.push(i);
  }
  const prev = ed.__gogenDiffDecorations || [];
  ed.__gogenDiffDecorations = ed.deltaDecorations(prev, decorations);
  // Line each tracked decoration covers, same order as the ids — lets the
  // append-only pass find (and replace) the grown last line's decoration.
  ed.__gogenDiffDecoLines = decoLines;
}

// Append-only decoration pass for a streaming diff: classify and decorate
// only lines from `fromLine` to the end, keeping the decorations already
// tracked in ed.__gogenDiffDecorations (ids) / ed.__gogenDiffDecoLines
// (parallel line numbers). Valid because on an append-only delta the content
// above the edit point is byte-identical. (Rescanning every line per delta
// was O(n²) over the stream.)
//
// `regrowLine` is the model line the tail's first segment landed on when
// that line previously had content (a still-streaming incomplete line). Its
// class can change as it grows — classes are prefix matches, so a line can
// cross a threshold ('@a' → '@@ …', 'index' → 'index …', '--' → '---' del→
// meta) though it can never lose one. When it was decorated, it is the
// highest tracked line: drop that decoration and re-classify the line with
// the tail below.
function appendUnifiedDiffDecorations(ed, fromLine, regrowLine) {
  const model = ed.getModel();
  if (!model) return;
  const ids = ed.__gogenDiffDecorations || (ed.__gogenDiffDecorations = []);
  const lines = ed.__gogenDiffDecoLines || (ed.__gogenDiffDecoLines = []);
  const removed = [];
  if (regrowLine != null && lines.length > 0 && lines[lines.length - 1] === regrowLine) {
    removed.push(ids.pop());
    lines.pop();
  }
  const decorations = [];
  const newLines = [];
  for (let i = fromLine; i <= model.getLineCount(); i++) {
    const cls = diffLineClass(model.getLineContent(i));
    if (!cls) continue;
    decorations.push({
      range: new monaco.Range(i, 1, i, model.getLineMaxColumn(i)),
      options: {
        isWholeLine: true,
        className: cls,
        marginClassName: cls + '-margin',
      },
    });
    newLines.push(i);
  }
  if (!removed.length && !decorations.length) return;
  ids.push(...ed.deltaDecorations(removed, decorations));
  lines.push(...newLines);
}

// ── Editor preferences (settings modal ↔ Monaco options) ──
function getEditorPrefs() {
  const ls = (key, dflt) => storageGet(key) || dflt;
  let fontSize = parseInt(ls('gogen_editor_fontsize', ''), 10);
  if (!Number.isFinite(fontSize) || fontSize < 8 || fontSize > 32) fontSize = 13;
  return {
    minimap: ls('gogen_editor_minimap', 'off') === 'on',
    wordWrap: ls('gogen_editor_wordwrap', 'on') === 'on' ? 'on' : 'off',
    stickyScroll: ls('gogen_editor_sticky', 'on') !== 'off',
    fontSize,
  };
}

function editorOptions(prefs) {
  return {
    automaticLayout: true,
    minimap: { enabled: prefs.minimap },
    fontSize: prefs.fontSize,
    wordWrap: prefs.wordWrap,
    stickyScroll: { enabled: prefs.stickyScroll },
    scrollBeyondLastLine: false,
  };
}

/**
 * Re-apply editor preferences to the live edit editor (called when settings
 * change). Deliberately NOT applied to diffEditor: the diff panes keep fixed
 * options for documented reasons (wheel chaining to chat, ResizeObserver vs
 * flex layout, cursor-blink keeping the refresh driver at 60 Hz, sticky
 * scroll disabled in diff view). See showDiffPane()/mountDiffEditor().
 */
export function applyEditorPrefs() {
  const opts = editorOptions(getEditorPrefs());
  if (editor) editor.updateOptions(opts);
}

function ensureEditors() {
  const host = $('monaco-host');
  if (!host) return;
  if (!editor) {
    editor = monaco.editor.create(host, editorOptions(getEditorPrefs()));
    editor.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyS, () => {
      saveActive();
    });
    // Shift+Alt+F (VS Code convention) rather than Ctrl+Shift+F, which the
    // app reserves for Find in Files (document keydown handler in app.js).
    editor.addCommand(monaco.KeyMod.Shift | monaco.KeyMod.Alt | monaco.KeyCode.KeyF, () => {
      formatActive(editor);
    });
    editor.onDidChangeModelContent(() => {
      updateDirtyState();
      // A reference highlight is a pointer, not an edit — drop it on the
      // first content change so it never lingers over the user's work.
      if (refDecorationIds.length) {
        refDecorationIds = editor.deltaDecorations(refDecorationIds, []);
      }
    });
    // onDidChangeCursorSelection fires for both cursor moves and selection
    // changes, so it drives the Ln/Col and "N selected" readouts together.
    editor.onDidChangeCursorSelection(() => updateStatusBar());
    // detectIndentation can change tabSize/insertSpaces after the first edit;
    // reflect that in the indent readout. Guarded because the standalone
    // editor API does not document this event on every build.
    if (editor.onDidChangeModelOptions) editor.onDidChangeModelOptions(() => updateStatusBar());
    editor.onDidChangeModel(() => {
      // Model switch invalidates any reference-highlight decoration ids.
      refDecorationIds = [];
      updateStatusBar();
      refreshTabBadges();
    });
    editor.onDidChangeModelLanguage(() => {
      updateStatusBar();
      updateUndoRedoButtons(); // Format button visibility depends on language
    });

    // --- Context menu: add selection reference to chat input ---
    editor.addAction({
      id: 'gogen-add-reference-to-chat',
      label: 'Add Reference to Chat',
      contextMenuGroupId: 'navigation',
      contextMenuOrder: 1.5,
      run(ed) {
        const selection = ed.getSelection();
        if (!selection || !activePath) return;
        const startLine = selection.startLineNumber;
        const endLine = selection.endLineNumber;
        const hasSelection = !selection.isEmpty();

        // Build the reference string
        let ref;
        if (hasSelection && startLine !== endLine) {
          ref = `@${activePath}:${startLine}-${endLine}`;
        } else {
          ref = `@${activePath}:${startLine}`;
        }

        const inputEl = document.getElementById('message-input');
        if (!inputEl) return;

        // Insert reference at cursor or append
        const start = inputEl.selectionStart;
        const end = inputEl.selectionEnd;
        const before = inputEl.value.slice(0, start);
        const after = inputEl.value.slice(end);
        const spacer = before && !before.endsWith(' ') && !before.endsWith('\n') ? ' ' : '';
        inputEl.value = before + spacer + ref + after;
        // Move cursor after the inserted reference
        const cursorPos = start + spacer.length + ref.length;
        inputEl.selectionStart = cursorPos;
        inputEl.selectionEnd = cursorPos;
        inputEl.focus();
        inputEl.dispatchEvent(new Event('input', { bubbles: true }));
      },
    });

    // --- Context menu: navigation + symbol actions (silently no-op for
    // languages without a language service). ---
    editor.addAction({
      id: 'gogen-go-to-definition',
      label: 'Go to Definition',
      contextMenuGroupId: 'navigation',
      contextMenuOrder: 1.1,
      run(ed) {
        ed.getAction('editor.action.revealDefinition')?.run();
      },
    });
    editor.addAction({
      id: 'gogen-peek-definition',
      label: 'Peek Definition',
      contextMenuGroupId: 'navigation',
      contextMenuOrder: 1.2,
      run(ed) {
        ed.getAction('editor.action.peekDefinition')?.run();
      },
    });
    editor.addAction({
      id: 'gogen-rename-symbol',
      label: 'Rename Symbol',
      contextMenuGroupId: 'navigation',
      contextMenuOrder: 1.3,
      run(ed) {
        ed.getAction('editor.action.rename')?.run();
      },
    });
    editor.addAction({
      id: 'gogen-format-document',
      label: 'Format Document',
      contextMenuGroupId: '1_modification',
      contextMenuOrder: 1.1,
      run(ed) {
        formatActive(ed);
      },
    });
    editor.addAction({
      id: 'gogen-copy-path',
      label: 'Copy Path',
      contextMenuGroupId: '9_cutcopypaste',
      contextMenuOrder: 5,
      run(_ed) {
        if (!activePath || !navigator.clipboard) return;
        navigator.clipboard.writeText(activePath).catch(() => {});
      },
    });

  }
}

function showEditPane() {
  mode = 'edit';
  const host = $('monaco-host');
  const diffHost = $('monaco-diff-host');
  if (host) host.style.display = 'block';
  if (diffHost) diffHost.style.display = 'none';
  const diffBar = $('editor-diff-toolbar');
  if (diffBar) diffBar.hidden = true;
  if (diffEditor) {
    // Keep models; just hide
  }
  updateStatusBar();
}

function showDiffPane() {
  mode = 'diff';
  const host = $('monaco-host');
  const diffHost = $('monaco-diff-host');
  if (host) host.style.display = 'none';
  if (diffHost) diffHost.style.display = 'block';
  const diffBar = $('editor-diff-toolbar');
  if (diffBar) diffBar.hidden = false;
  if (!diffEditor) {
    diffEditor = monaco.editor.createDiffEditor(diffHost, {
      automaticLayout: true,
      readOnly: true,
      // Read-only diff pane: no text cursor (avoids a permanent blink
      // animation per editor keeping the refresh driver busy).
      cursorBlinking: 'hidden',
      // Monaco 0.56's line-based diff algorithm (pure JS, bundled — no wasm
      // asset; "advanced-external"/"advanced-wasm" need a runtime
      // import("@vscode/diff") that the vendored build does not ship).
      // Better hunks/block-move handling than the legacy default ("smart").
      diffAlgorithm: 'advanced',
      renderSideBySide: GOGEN_UI.diffRenderSideBySide,
      minimap: { enabled: false },
      fontSize: 13,
      stickyScroll: { enabled: false },
      renderIndicators: true,
      originalEditable: false,
    });
    diffEditor.onDidUpdateDiff(() => updateDiffStat());
  } else {
    diffEditor.updateOptions({ renderSideBySide: GOGEN_UI.diffRenderSideBySide });
  }
  updateStatusBar();
}

function basename(path) {
  const parts = path.split('/');
  return parts[parts.length - 1] || path;
}

function touchBuffer(path) {
  const b = buffers.get(path);
  if (b) b.lastUsed = Date.now();
}

function isDirty(path) {
  const b = buffers.get(path);
  if (!b || !b.model) return false;
  return b.model.getAlternativeVersionId() !== b.savedVersionId;
}

// True when any open buffer has unsaved edits; drives the beforeunload
// guard so closing/reloading the page never silently discards changes
// (the close-tab modal only covers closing individual tabs).
function anyDirtyBuffer() {
  for (const path of buffers.keys()) {
    if (isDirty(path)) return true;
  }
  return false;
}

function updatePathLabel() {
  const label = $('editor-path-label');
  if (!label) return;
  if (mode === 'diff' && activePath) {
    label.textContent = `${activePath} (unstaged diff)${diffStatText}`;
  } else if (activePath) {
    label.textContent = isDirty(activePath) ? `${activePath} *` : activePath;
  } else {
    label.textContent = 'No file open';
  }
}

// Cheap per-keystroke path: mutate only the active tab's dirty dot and the
// path label instead of rebuilding the whole tab strip (renderTabs).
function updateDirtyState() {
  if (mode === 'edit' && activePath) {
    const strip = $('editor-tabs');
    const active = strip && strip.querySelector('.file-tab.active');
    if (active) {
      active.classList.toggle('dirty', isDirty(activePath));
    }
  }
  updatePathLabel();
  updateUndoRedoButtons();
}

function updateDirtyIndicators() {
  renderTabs();
  updatePathLabel();
  updateUndoRedoButtons();
  // Keep the tree's active-file highlight in step with the active path.
  highlightActiveTreeRow();
}

function updateUndoRedoButtons() {
  const undoBtn = $('btn-undo');
  const redoBtn = $('btn-redo');
  const canEdit = mode === 'edit' && editor && editor.getModel();
  let canUndo = false;
  let canRedo = false;
  if (canEdit) {
    // Monaco does not expose stack depth; enable whenever an editable model is active.
    canUndo = true;
    canRedo = true;
  }
  if (undoBtn) undoBtn.disabled = !canUndo;
  if (redoBtn) redoBtn.disabled = !canRedo;
  const fmtBtn = $('btn-format');
  // The Format button is only useful for languages with a formatting provider
  // (the four Monaco language services); hide it for everything else so no
  // dead control remains visible.
  const fmtLang = editor && editor.getModel() ? editor.getModel().getLanguageId() : '';
  const canFormat = canEdit && FORMATTER_LANGS.has(fmtLang);
  if (fmtBtn) {
    fmtBtn.hidden = !canFormat;
    fmtBtn.disabled = !canFormat;
  }
  const prevBtn = $('btn-diff-prev');
  const nextBtn = $('btn-diff-next');
  // Navigation is only meaningful while viewing a diff that has changes —
  // hide the buttons entirely otherwise so no dead controls remain visible.
  const hasDiffChanges = mode === 'diff' && !!diffEditor && !!diffEditor.getModel() && diffChangeCount > 0;
  if (prevBtn) {
    prevBtn.hidden = !hasDiffChanges;
    prevBtn.disabled = !hasDiffChanges;
  }
  if (nextBtn) {
    nextBtn.hidden = !hasDiffChanges;
    nextBtn.disabled = !hasDiffChanges;
  }
}

export function editorUndo() {
  if (mode !== 'edit' || !editor) return;
  editor.focus();
  editor.trigger('toolbar', 'undo', null);
}

export function editorRedo() {
  if (mode !== 'edit' || !editor) return;
  editor.focus();
  editor.trigger('toolbar', 'redo', null);
}

function renderTabs() {
  const strip = $('editor-tabs');
  if (!strip) return;
  strip.innerHTML = '';
  strip.setAttribute('role', 'tablist');
  for (const path of openOrder) {
    // The active tab stays highlighted in diff mode too, with an accent
    // underline (.diffing) so it reads as "viewing diff", not "editing".
    const isActive = path === activePath;
    const tab = document.createElement('div');
    tab.className = 'file-tab'
      + (isActive ? ' active' : '')
      + (isActive && mode === 'diff' ? ' diffing' : '');
    tab.title = path;
    tab.dataset.path = path;
    // Roving-tabindex tablist: only the active tab is in the Tab order;
    // Left/Right arrows move between tabs (setupEditorUI).
    tab.setAttribute('role', 'tab');
    tab.tabIndex = isActive ? 0 : -1;
    tab.setAttribute('aria-selected', isActive ? 'true' : 'false');
    tab.classList.toggle('dirty', isDirty(path));
    const name = document.createElement('span');
    name.className = 'file-tab-name';
    name.textContent = basename(path);
    const dot = document.createElement('span');
    dot.className = 'file-tab-dot';
    dot.setAttribute('aria-hidden', 'true');
    const close = document.createElement('button');
    close.className = 'file-tab-close';
    close.type = 'button';
    close.textContent = '×';
    close.title = 'Close';
    close.tabIndex = -1;
    close.setAttribute('aria-label', `Close ${basename(path)}`);
    close.onclick = (e) => {
      e.stopPropagation();
      closeTab(path);
    };
    tab.appendChild(name);
    tab.appendChild(dot);
    const badge = bufferBadge(path);
    if (badge) tab.appendChild(badge);
    tab.appendChild(close);
    tab.onclick = () => activatePath(path);
    // Middle-click closes (desktop-editor convention). The mousedown
    // preventDefault suppresses the middle-click autoscroll.
    tab.addEventListener('mousedown', (e) => {
      if (e.button === 1) e.preventDefault();
    });
    tab.addEventListener('auxclick', (e) => {
      if (e.button !== 1) return;
      e.preventDefault();
      closeTab(path);
    });
    tab.addEventListener('contextmenu', (e) => {
      e.preventDefault();
      openTabMenu(path, e.clientX, e.clientY);
    });
    strip.appendChild(tab);
  }
  // Keep the active tab visible when the strip overflows horizontally.
  const active = strip.querySelector('.file-tab.active');
  if (active && typeof active.scrollIntoView === 'function') {
    try { active.scrollIntoView({ inline: 'nearest', block: 'nearest' }); } catch (_) { /* jsdom */ }
  }
}

/** Error/warning badge span for a tab, or null when the buffer is clean. */
function bufferBadge(path) {
  const counts = markerCounts.get(path);
  if (!counts || (!counts.errors && !counts.warnings)) return null;
  const span = document.createElement('span');
  span.className = 'file-tab-badge';
  // Error color wins when both are present; the text always shows both counts.
  if (counts.errors) span.classList.add('has-errors');
  else if (counts.warnings) span.classList.add('has-warnings');
  const parts = [];
  if (counts.errors) parts.push(`⨯${counts.errors}`);
  if (counts.warnings) parts.push(`⚠${counts.warnings}`);
  span.textContent = parts.join(' ');
  span.title = `${counts.errors} error(s), ${counts.warnings} warning(s)`;
  return span;
}

/** Recompute marker counts for all open buffers and refresh badges in place. */
function refreshTabBadges() {
  if (!monaco) return;
  let changed = false;
  for (const [path, b] of buffers) {
    if (!b || !b.model) continue;
    let errors = 0;
    let warnings = 0;
    for (const m of monaco.editor.getModelMarkers({ resource: b.model.uri })) {
      if (m.severity === monaco.MarkerSeverity.Error) errors++;
      else if (m.severity === monaco.MarkerSeverity.Warning) warnings++;
    }
    const prev = markerCounts.get(path);
    if (!prev || prev.errors !== errors || prev.warnings !== warnings) {
      markerCounts.set(path, { errors, warnings });
      changed = true;
    }
  }
  if (!changed) return; // nothing to repaint
  const strip = $('editor-tabs');
  if (!strip) return;
  if (strip.children.length !== openOrder.length) {
    // Tab list drifted (open/close since the last render) — rebuild.
    renderTabs();
    updateStatusBar();
    return;
  }
  // Update badges in place instead of rebuilding the whole strip: markers can
  // change per keystroke while typing, and a full rebuild would also fire for
  // transient diff-pane models whose counts never enter markerCounts.
  for (let i = 0; i < openOrder.length; i++) {
    const tab = strip.children[i];
    if (!tab || !tab.classList.contains('file-tab')) continue;
    const old = tab.querySelector('.file-tab-badge');
    if (old) old.remove();
    const badge = bufferBadge(openOrder[i]);
    if (badge) tab.insertBefore(badge, tab.querySelector('.file-tab-close'));
  }
  updateStatusBar();
}

/** Render the error/warning readout for the active file (diff or edit pane). */
function renderProblemsText(probs) {
  let errors = 0;
  let warnings = 0;
  if (mode === 'diff' && diffEditor && diffEditor.getModel() && monaco) {
    // The diff pane's transient models are separate from edit buffers; count
    // their diagnostics directly so the readout matches the diff being viewed.
    const dm = diffEditor.getModel();
    for (const model of [dm.original, dm.modified]) {
      if (!model) continue;
      for (const mk of monaco.editor.getModelMarkers({ resource: model.uri })) {
        if (mk.severity === monaco.MarkerSeverity.Error) errors++;
        else if (mk.severity === monaco.MarkerSeverity.Warning) warnings++;
      }
    }
  } else {
    const counts = markerCounts.get(activePath);
    if (counts) {
      errors = counts.errors;
      warnings = counts.warnings;
    }
  }
  probs.textContent = '';
  probs.className = '';
  if (errors || warnings) {
    probs.textContent = `⨯ ${errors}   ⚠ ${warnings}`;
    probs.classList.add(errors ? 'status-problems-error' : 'status-problems-warn');
  }
}

/**
 * Render the branch/ahead/behind readout from the last git_status reply.
 * Hidden when there is no repo, no branch, or the last status call failed.
 */
function renderStatusBranch() {
  const wrap = $('editor-status-branch');
  const name = $('editor-status-branch-name');
  if (!wrap || !name) return;
  const info = gitBranch;
  if (!info || !info.branch) {
    wrap.hidden = true;
    wrap.title = '';
    name.textContent = '';
    return;
  }
  let text = info.branch;
  const ab = [];
  if (info.ahead > 0) ab.push(`↑${info.ahead}`);
  if (info.behind > 0) ab.push(`↓${info.behind}`);
  if (ab.length) text += ' ' + ab.join(' ');
  name.textContent = text;
  wrap.hidden = false;
  wrap.title = info.upstream ? `${info.branch} → ${info.upstream}` : info.branch;
}

/** Update the branch, cursor, selection, document and problems readout. */
function updateStatusBar() {
  renderStatusBranch();
  const lncol = $('editor-status-lncol');
  const lang = $('editor-status-lang');
  const probs = $('editor-status-problems');
  const sel = $('editor-status-selection');
  const indent = $('editor-status-indent');
  const eol = $('editor-status-eol');

  // In diff mode the edit editor is hidden: its cursor, selection, indent and
  // EOL would describe a different document than the diff on screen. Show the
  // diff file's language and problems only.
  const model = mode === 'diff' || !editor ? null : editor.getModel();

  if (lncol) {
    const pos = model ? editor.getPosition() : null;
    lncol.textContent = pos ? `Ln ${pos.lineNumber}, Col ${pos.column}` : '';
  }
  if (lang) {
    lang.textContent = mode === 'diff' ? 'diff' : (model ? model.getLanguageId() : '');
  }
  if (probs) renderProblemsText(probs);

  // Selection: a single-line selection shows its character count; a
  // multi-line one shows its line count. Neither materializes the selected
  // text, so selecting a whole large file stays cheap.
  if (sel) {
    const s = model ? editor.getSelection() : null;
    let text = '';
    if (s && !s.isEmpty()) {
      text = s.startLineNumber === s.endLineNumber
        ? `${s.endColumn - s.startColumn} selected`
        : `${s.endLineNumber - s.startLineNumber + 1} lines selected`;
    }
    sel.textContent = text;
    sel.hidden = !text;
  }
  if (indent) {
    let text = '';
    if (model) {
      const opts = model.getOptions();
      text = opts.insertSpaces ? `Spaces: ${opts.tabSize}` : `Tab Size: ${opts.tabSize}`;
    }
    indent.textContent = text;
    indent.hidden = !text;
  }
  if (eol) {
    const text = model ? (model.getEOL() === '\r\n' ? 'CRLF' : 'LF') : '';
    eol.textContent = text;
    eol.hidden = !text;
  }
}

// Monaco standalone only ships formatting providers for the four language
// services (TypeScript/JavaScript, JSON, CSS, HTML); every other language is
// tokenizer-only, so the Format action would be a silent no-op for them.
const FORMATTER_LANGS = new Set(['typescript', 'javascript', 'json', 'css', 'html']);

/** Format the active document, reporting when the language has no formatter. */
export function formatActive(ed) {
  const target = ed || editor;
  if (!target || !target.getModel()) return;
  const lang = target.getModel().getLanguageId();
  if (!FORMATTER_LANGS.has(lang)) {
    if (toastFn) toastFn(`No formatter available for ${lang || 'plaintext'}`, 'info');
    return;
  }
  target.focus();
  const action = target.getAction('editor.action.formatDocument');
  if (action) action.run();
}

/** Jump to the previous/next diff change in the unstaged-diff pane. */
export function diffNav(target) {
  if (mode !== 'diff' || !diffEditor) return;
  diffEditor.goToDiff(target === 'prev' ? 'previous' : 'next');
}

/** Recompute "+N −M" from the diff editor's change list and refresh the label. */
function updateDiffStat() {
  diffStatText = '';
  diffChangeCount = 0;
  if (diffEditor && diffEditor.getModel()) {
    const changes = diffEditor.getLineChanges();
    if (changes) {
      diffChangeCount = changes.length;
      let added = 0;
      let removed = 0;
      for (const ch of changes) {
        // Empty ranges (end < start) represent pure insertions/deletions; clamp to 0.
        added += Math.max(0, ch.modifiedEndLineNumber - ch.modifiedStartLineNumber + 1);
        removed += Math.max(0, ch.originalEndLineNumber - ch.originalStartLineNumber + 1);
      }
      if (added || removed) diffStatText = `  +${added} −${removed}`;
    }
  }
  updatePathLabel();
  updateUndoRedoButtons();
}

// Oldest clean (non-dirty) non-active tab that may be evicted to make room,
// or null when every other tab has unsaved edits. Dirty tabs are NEVER
// evicted silently — that would discard unsaved edits (closeTab and the
// beforeunload guard both treat them as requiring confirmation).
function findEvictionVictim() {
  let victim = null;
  let oldest = Infinity;
  for (const p of openOrder) {
    if (p === activePath) continue;
    if (isDirty(p)) continue;
    const b = buffers.get(p);
    const t = b ? b.lastUsed : 0;
    if (t < oldest) {
      oldest = t;
      victim = p;
    }
  }
  return victim;
}

async function enforceTabCap() {
  while (openOrder.length > GOGEN_UI.maxOpenTabs) {
    const victim = findEvictionVictim();
    if (!victim) break; // all remaining tabs are dirty — keep them
    disposeBuffer(victim);
  }
}

function disposeBuffer(path) {
  const b = buffers.get(path);
  if (b && b.model) b.model.dispose();
  buffers.delete(path);
  markerCounts.delete(path);
  openOrder = openOrder.filter((p) => p !== path);
  if (activePath === path) {
    activePath = openOrder.length ? openOrder[openOrder.length - 1] : null;
    if (activePath) {
      showEditPane();
      ensureEditors();
      const nb = buffers.get(activePath);
      editor.setModel(nb.model);
      if (nb.viewState) editor.restoreViewState(nb.viewState);
    } else if (editor) {
      editor.setModel(null);
    }
  }
  updateDirtyIndicators();
}

async function closeTab(path) {
  if (isDirty(path)) {
    const confirmed = await showCloseTabModal(basename(path));
    if (!confirmed) return;
  }
  disposeBuffer(path);
}

function showCloseTabModal(filename) {
  return new Promise((resolve) => {
    const overlay = document.getElementById('close-tab-overlay');
    const filenameEl = document.getElementById('close-tab-filename');
    if (!overlay) { resolve(window.confirm(`Close ${filename} and discard unsaved changes?`)); return; }
    filenameEl.textContent = `${filename} has unsaved changes that will be lost.`;
    // Discard = confirm (the destructive action), Keep = cancel; Esc and a
    // backdrop click keep editing. openDialog wires buttons, Esc, backdrop
    // and the teardown; the focus trap / restore come from openModal.
    openDialog(overlay, {
      confirm: 'close-tab-discard-btn',
      cancel: 'close-tab-keep-btn',
      onConfirm: () => resolve(true),
      onCancel: () => resolve(false),
    });
  });
}

// ── Tab context menu ──
// Right-clicking a tab (renderTabs attaches the handler) opens a small
// floating menu at the pointer. The bulk "close" actions only ever close
// CLEAN tabs — a dirty tab is kept and reported, never silently discarded
// (except "Close all", which asks once before discarding).

/** Whether a tab-menu action would do anything for `path` (grey it out if not). */
function tabMenuActionEnabled(action, path) {
  switch (action) {
    case 'close':
    case 'copy-path':
      return true;
    case 'close-others':
      return openOrder.length > 1;
    case 'close-saved':
      return openOrder.some((p) => !isDirty(p));
    case 'close-all':
      return openOrder.length > 0;
    default:
      return false;
  }
}

/** Position and show the tab context menu for `path` at viewport (x, y). */
function openTabMenu(path, x, y) {
  const menu = $('tab-context-menu');
  if (!menu) return;
  tabMenuPath = path;
  for (const row of menu.querySelectorAll('.tab-menu-row')) {
    row.disabled = !tabMenuActionEnabled(row.dataset.action, path);
  }
  menu.hidden = false;
  // Measure after showing, then clamp into the viewport so a tab near the
  // right/bottom edge never pushes the menu off-screen.
  const rect = menu.getBoundingClientRect();
  const left = Math.max(8, Math.min(x, window.innerWidth - rect.width - 8));
  const top = Math.max(8, Math.min(y, window.innerHeight - rect.height - 8));
  menu.style.left = left + 'px';
  menu.style.top = top + 'px';
}

function closeTabMenu() {
  const menu = $('tab-context-menu');
  if (menu) menu.hidden = true;
  tabMenuPath = null;
}

// Close every clean tab matching `match`; dirty matching tabs are kept and
// the count reported, so a bulk action can never discard unsaved edits.
function closeCleanTabs(match) {
  let kept = 0;
  for (const p of [...openOrder]) {
    if (!match(p)) continue;
    if (isDirty(p)) { kept++; continue; }
    disposeBuffer(p);
  }
  if (kept) toast(`Kept ${kept} tab(s) with unsaved changes`, 'info');
}

/** Copy a tab's full path, mirroring the Monaco "Copy Path" action. */
function copyTabPath(path) {
  if (!navigator.clipboard) {
    toast('Clipboard unavailable', 'error');
    return;
  }
  navigator.clipboard.writeText(path).then(
    () => toast(`Copied ${basename(path)} path`, 'success'),
    () => toast('Copy failed', 'error'),
  );
}

/**
 * One "discard N unsaved files?" confirmation for the Close all action,
 * reusing the close-tab modal (only one modal is open at a time, so sharing
 * its message element is safe).
 */
function confirmDiscardAll(n) {
  return new Promise((resolve) => {
    const overlay = document.getElementById('close-tab-overlay');
    const filenameEl = document.getElementById('close-tab-filename');
    if (!overlay) { resolve(window.confirm(`${n} files have unsaved changes. Discard and close all tabs?`)); return; }
    filenameEl.textContent = `${n} open file(s) have unsaved changes that will be lost.`;
    openDialog(overlay, {
      confirm: 'close-tab-discard-btn',
      cancel: 'close-tab-keep-btn',
      onConfirm: () => resolve(true),
      onCancel: () => resolve(false),
    });
  });
}

/** Run a tab-menu action against the tab the menu was opened on. */
async function runTabMenuAction(action) {
  const path = tabMenuPath;
  closeTabMenu();
  if (!path) return;
  switch (action) {
    case 'close':
      closeTab(path);
      return;
    case 'close-others':
      closeCleanTabs((p) => p !== path);
      return;
    case 'close-saved':
      closeCleanTabs(() => true);
      return;
    case 'close-all': {
      const dirty = openOrder.filter((p) => isDirty(p)).length;
      if (dirty && !(await confirmDiscardAll(dirty))) return;
      for (const p of [...openOrder]) disposeBuffer(p);
      return;
    }
    case 'copy-path':
      copyTabPath(path);
      return;
  }
}

function activatePath(path) {
  if (!buffers.has(path)) return;
  if (activePath && editor && mode === 'edit') {
    const cur = buffers.get(activePath);
    if (cur) cur.viewState = editor.saveViewState();
  }
  activePath = path;
  touchBuffer(path);
  showEditPane();
  ensureEditors();
  const b = buffers.get(path);
  // Drop any reference highlight from the previous model before switching
  // away, so it can't resurface when the old tab is reopened.
  if (refDecorationIds.length) {
    refDecorationIds = editor.deltaDecorations(refDecorationIds, []);
  }
  editor.setModel(b.model);
  if (b.viewState) editor.restoreViewState(b.viewState);
  editor.focus();
  updateDirtyIndicators();
}

async function openFile(path, line, endLine) {
  await initMonaco();
  ensureEditors();
  if (buffers.has(path)) {
    activatePath(path);
  } else {
    // Opening would exceed the tab cap with no clean tab to evict — refuse
    // up front instead of creating the buffer and then discarding a dirty
    // tab's edits to make room.
    if (openOrder.length >= GOGEN_UI.maxOpenTabs && !findEvictionVictim()) {
      toast(`All ${GOGEN_UI.maxOpenTabs} tabs have unsaved changes — save or close one first`, 'info');
      return false;
    }
    let data;
    try {
      data = await wsRequest('fs_read', { path });
    } catch (err) {
      toast(`Cannot open ${basename(path)}: ${err.message || 'read failed'}`, 'error');
      return false;
    }
    if (data.error) {
      toast(`Cannot open ${basename(path)}: ${data.error}`, 'error');
      return false;
    }
    const model = monaco.editor.createModel(data.content || '', data.language || 'plaintext');
    buffers.set(path, {
      model,
      viewState: null,
      savedVersionId: model.getAlternativeVersionId(),
      lastUsed: Date.now(),
    });
    openOrder.push(path);
    await enforceTabCap();
    activatePath(path);
    refreshTabBadges();
  }
  if (line && line > 0 && editor) {
    editor.revealLineInCenter(line);
    editor.setPosition({ lineNumber: line, column: 1 });
    editor.focus();
    highlightRefRange(line, endLine);
  }
  // On mobile the explorer is an overlay drawer: once a file is open it
  // would cover the editor, so close it (all open paths funnel through
  // openFile: tree, unstaged list, find-in-files, chat references).
  closeEditorSidebarOnMobile();
  return true;
}

function closeEditorSidebarOnMobile() {
  if (window.innerWidth > 768) return;
  $('editor-sidebar')?.classList.remove('open');
}

export async function openFileAtLine(path, line, endLine) {
  const ok = await openFile(path, line, endLine);
  // "Open & switch to editor" applies to file references opened from chat
  // (this is the chat entry point). Tree and find-in-files clicks call
  // openFile directly and must never switch panes or re-render the tree.
  if (ok && storageGet('gogen_file_click_behavior') === 'open-switch') {
    switchToEditorPane();
  }
}

/**
 * Highlight a line range in the edit pane (used when jumping from a chat
 * reference like @path:12-20). The highlight clears on the next content
 * change so it never lingers over edits.
 */
function highlightRefRange(startLine, endLine) {
  if (!editor || !editor.getModel()) return;
  const model = editor.getModel();
  const lastLine = Math.max(startLine, endLine || startLine);
  if (lastLine > model.getLineCount()) return;
  refDecorationIds = editor.deltaDecorations(refDecorationIds, [
    {
      range: new monaco.Range(startLine, 1, lastLine, model.getLineMaxColumn(lastLine)),
      options: {
        isWholeLine: true,
        className: 'gogen-ref-highlight',
        linesDecorationsClassName: 'gogen-ref-highlight-gutter',
      },
    },
  ]);
}

async function savePath(path) {
  const b = buffers.get(path);
  if (!b) return false;
  try {
    await wsRequest('fs_write', { path, content: b.model.getValue() }, WS_REQUEST_SLOW_TIMEOUT_MS);
    b.savedVersionId = b.model.getAlternativeVersionId();
    updateDirtyIndicators();
    await refreshGitStatus();
    toast(`Saved ${basename(path)}`, 'success');
    return true;
  } catch (err) {
    toast(`Save failed ${basename(path)}: ${err.message}`, 'error');
    return false;
  }
}

async function saveActive() {
  if (!activePath || mode !== 'edit') return;
  await savePath(activePath);
}

async function saveAll() {
  const loading = document.getElementById('save-all-loading');
  if (loading) loading.classList.add('active');
  let any = false;
  for (const path of [...openOrder]) {
    if (isDirty(path)) {
      any = true;
      const ok = await savePath(path);
      if (!ok) {
        if (loading) loading.classList.remove('active');
        return;
      }
    }
  }
  if (loading) loading.classList.remove('active');
  if (any) toast('All files saved', 'success');
}

async function openUnstagedDiff(path) {
  await initMonaco();
  const prevPath = activePath;
  const prevMode = mode;
  if (activePath && editor && mode === 'edit') {
    const cur = buffers.get(activePath);
    if (cur) cur.viewState = editor.saveViewState();
  }
  activePath = path;
  showDiffPane();
  try {
    const data = await wsRequest('git_file_diff', { path }, WS_REQUEST_SLOW_TIMEOUT_MS);
    const lang = data.language || 'plaintext';
    const original = monaco.editor.createModel(data.original || '', lang);
    const modified = monaco.editor.createModel(data.modified || '', lang);
    const prev = diffEditor.getModel();
    diffChangeCount = 0; // unknown until the diff is computed; onDidUpdateDiff corrects it
    diffEditor.setModel({ original, modified });
    updateUndoRedoButtons();
    if (prev) {
      if (prev.original) prev.original.dispose();
      if (prev.modified) prev.modified.dispose();
    }
  } catch (err) {
    toast(`Diff failed: ${err.message}`, 'error');
    // Restore the pre-call state. Neither editor was touched on failure
    // (diff models are only swapped in on success, and the edit editor
    // keeps its previous model), so only activePath and the visible pane
    // need to go back. Without this the path label would name the failed
    // diff path while the editor shows the previous file, and Ctrl+S
    // would hit a path with no buffer and silently no-op.
    activePath = prevPath;
    if (prevMode === 'diff') showDiffPane();
    else showEditPane();
  }
  updateDirtyIndicators();
}

/** Icon markup from the index.html sprite (names here are static literals). */
function iconSvg(name) {
  return `<svg class="icon" aria-hidden="true"><use href="#i-${name}"></use></svg>`;
}

/** Visible tree rows (offsetParent is null while an ancestor folder is collapsed). */
function visibleTreeRows() {
  const tree = $('file-tree');
  if (!tree) return [];
  return [...tree.querySelectorAll('.tree-item')].filter((el) => el.offsetParent !== null);
}

/** Move the tree's roving tabindex + focus onto `row`. */
function focusTreeRow(row) {
  if (!row) return;
  for (const r of visibleTreeRows()) r.tabIndex = -1;
  row.tabIndex = 0;
  row.focus();
}

/** Seed the roving tabindex so at least one row is reachable by Tab. */
function seedTreeTabindex() {
  const tree = $('file-tree');
  if (!tree) return;
  const rows = tree.querySelectorAll('.tree-item');
  if (!rows.length || tree.querySelector('.tree-item[tabindex="0"]')) return;
  rows[0].tabIndex = 0;
}

// Mirror the active editor file into the tree: tag its row, clear the old
// one, keep it in view. The row exists only when its folder chain is expanded
// (lazy tree), so a miss is a no-op.
function highlightActiveTreeRow() {
  const tree = $('file-tree');
  if (!tree) return;
  for (const el of tree.querySelectorAll('.tree-item')) {
    const on = !!activePath && el.dataset.path === activePath;
    el.classList.toggle('active', on);
    el.setAttribute('aria-selected', on ? 'true' : 'false');
    if (on && typeof el.scrollIntoView === 'function') {
      try { el.scrollIntoView({ block: 'nearest' }); } catch (_) { /* jsdom */ }
    }
  }
}

/** Expand/collapse a folder row, updating its chevron and aria-expanded. */
function setDirExpanded(row, child, chevron, ent, expand) {
  child.style.display = expand ? 'block' : 'none';
  row.setAttribute('aria-expanded', expand ? 'true' : 'false');
  chevron.innerHTML = iconSvg(expand ? 'chevron-down' : 'chevron-right');
  if (expand) expandedDirs.add(ent.path);
  else expandedDirs.delete(ent.path);
}

/** Toggle a folder row, lazily loading its children on first open. */
async function toggleDir(row, child, chevron, ent) {
  const open = child.style.display !== 'none';
  setDirExpanded(row, child, chevron, ent, !open);
  if (!open && !child.dataset.loaded) {
    child.dataset.loaded = '1';
    await loadTree(ent.path, child);
  }
}

// appendTreeRow builds one row (role=treeitem) and, for a folder, its lazily
// loaded children container, appending both to `container`. It does NOT load
// children itself — loadTree restores expansion so it can await.
function appendTreeRow(container, ent) {
  const row = document.createElement('div');
  row.className = 'tree-item' + (ent.isDir ? ' dir' : ' file');
  row.dataset.path = ent.path;
  row.title = ent.path;
  row.setAttribute('role', 'treeitem');
  row.tabIndex = -1;
  row.setAttribute('aria-selected', 'false');

  const chevron = document.createElement('span');
  chevron.className = 'tree-chevron';
  const iconEl = document.createElement('span');
  iconEl.className = 'tree-icon';
  iconEl.innerHTML = iconSvg(ent.isDir ? 'folder' : 'file');
  const label = document.createElement('span');
  label.className = 'tree-label';
  label.textContent = ent.name;
  row.append(chevron, iconEl, label);

  if (!ent.isDir) {
    row.addEventListener('click', () => openFile(ent.path));
    container.appendChild(row);
    return { row, child: null };
  }

  const child = document.createElement('div');
  child.className = 'tree-children';
  child.setAttribute('role', 'group');
  const expanded = expandedDirs.has(ent.path);
  child.style.display = expanded ? 'block' : 'none';
  row.setAttribute('aria-expanded', expanded ? 'true' : 'false');
  chevron.innerHTML = iconSvg(expanded ? 'chevron-down' : 'chevron-right');
  row.addEventListener('click', () => { toggleDir(row, child, chevron, ent); });
  container.append(row, child);
  return { row, child };
}

async function loadTree(path, container) {
  container.innerHTML = '';
  let entries;
  try {
    const data = await wsRequest('fs_list', { path: path || '.' });
    entries = data.entries || [];
  } catch (err) {
    container.textContent = err.message;
    return;
  }
  entries.sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
    return a.name.localeCompare(b.name);
  });
  for (const ent of entries) {
    const { child } = appendTreeRow(container, ent);
    // A rebuild (refreshExplorer) wipes the DOM; re-expand folders the user
    // had open and lazily load their children, same as a click.
    if (child && expandedDirs.has(ent.path) && !child.dataset.loaded) {
      child.dataset.loaded = '1';
      await loadTree(ent.path, child);
    }
  }
  seedTreeTabindex();
  highlightActiveTreeRow();
}

/**
 * Create an empty file at the path typed in the new-file row, then reveal and
 * open it. Parent directories are created server-side (fs_write →
 * WriteFileAtomic).
 */
async function createNewFile() {
  const row = $('new-file-row');
  const input = $('new-file-input');
  if (!input) return;
  const path = input.value.trim();
  if (!path) {
    input.focus();
    return;
  }
  try {
    await wsRequest('fs_write', { path, content: '' }, WS_REQUEST_SLOW_TIMEOUT_MS);
  } catch (err) {
    toast(`Create failed: ${err.message || 'write failed'}`, 'error');
    return;
  }
  if (row) row.hidden = true;
  input.value = '';
  await refreshExplorer();
  await openFile(path);
  toast(`Created ${basename(path)}`, 'success');
}

/** Collapse every folder and rebuild the tree. */
function collapseTree() {
  expandedDirs.clear();
  refreshExplorer().catch((e) => toast(e.message, 'error'));
}

/**
 * changesBadge builds the colored status letter for a Changes row. `status` is the
 * porcelain-v2 column code (M/A/D/R/C/T, or U for untracked/unmerged);
 * `secCls` is the section class, so a conflict row's letter reads red.
 */
function changesBadge(status, secCls) {
  const code = (status || '?').charAt(0);
  const badge = document.createElement('span');
  badge.className = 'changes-badge changes-badge-' + code.toLowerCase();
  if (secCls === 'conflict') badge.classList.add('changes-badge-conflict');
  badge.textContent = code;
  badge.title = changesStatusTitle(code);
  return badge;
}

/** Human-readable label for a porcelain status code (tooltip). */
function changesStatusTitle(code) {
  switch (code) {
    case 'M': return 'Modified';
    case 'A': return 'Added';
    case 'D': return 'Deleted';
    case 'R': return 'Renamed';
    case 'C': return 'Copied';
    case 'T': return 'Type changed';
    case 'U': return 'Untracked / unmerged';
    default: return 'Changed';
  }
}

async function refreshGitStatus() {
  const list = $('changes-list');
  if (!list) return;
  list.innerHTML = '';
  try {
    const data = await wsRequest('git_status', {}, WS_REQUEST_SLOW_TIMEOUT_MS);
    // Capture the branch readout for the status bar (present only in the v2
    // payload). The clean-tree early return below must still publish it.
    gitBranch = data.gitStatus
      ? {
          branch: data.gitStatus.branch,
          upstream: data.gitStatus.upstream,
          ahead: data.gitStatus.ahead,
          behind: data.gitStatus.behind,
        }
      : null;
    renderStatusBranch();
    // v2 payload: pre-bucketed lists (the client never sees the XY matrix).
    // Fall back to the legacy flat list (Unstaged+Untracked) if a stale
    // server predates gitStatus.
    let sections;
    if (data.gitStatus) {
      const gs = data.gitStatus;
      sections = [
        { title: 'Conflicts', entries: gs.unmerged || [], cls: 'conflict' },
        { title: 'Staged', entries: gs.staged || [] },
        { title: 'Unstaged', entries: gs.unstaged || [] },
        { title: 'Untracked', entries: gs.untracked || [] },
      ];
    } else {
      sections = [{ title: 'Changes', entries: data.gitEntries || [] }];
    }
    // Header count: unique paths (a partially staged file appears in both
    // Staged and Unstaged but counts once).
    const uniquePaths = new Set();
    for (const sec of sections) for (const ent of sec.entries) uniquePaths.add(ent.path);
    const countEl = $('changes-count');
    if (countEl) {
      countEl.textContent = uniquePaths.size ? String(uniquePaths.size) : '';
      countEl.hidden = uniquePaths.size === 0;
    }
    if (!sections.some((s) => s.entries.length)) {
      list.textContent = 'Working tree clean';
      return;
    }
    for (const sec of sections) {
      if (!sec.entries.length) continue; // empty sections are hidden
      const wrap = document.createElement('div');
      wrap.className = 'changes-section' + (sec.cls ? ` ${sec.cls}` : '');
      const head = document.createElement('div');
      head.className = 'changes-section-head';
      head.textContent = sec.title;
      wrap.appendChild(head);
      for (const ent of sec.entries) {
        const row = document.createElement('div');
        row.className = 'changes-item';
        const label = document.createElement('span');
        label.className = 'changes-item-label';
        label.title = ent.path;
        label.onclick = () => openUnstagedDiff(ent.path);
        label.appendChild(changesBadge(ent.status, sec.cls));
        // Directory dimmed, basename bright — the path truncates as a unit.
        const pathEl = document.createElement('span');
        pathEl.className = 'changes-path';
        const slash = ent.path.lastIndexOf('/');
        if (slash >= 0) {
          const dirEl = document.createElement('span');
          dirEl.className = 'changes-path-dir';
          dirEl.textContent = ent.path.slice(0, slash + 1);
          pathEl.appendChild(dirEl);
        }
        const baseEl = document.createElement('span');
        baseEl.className = 'changes-path-base';
        baseEl.textContent = slash >= 0 ? ent.path.slice(slash + 1) : ent.path;
        pathEl.appendChild(baseEl);
        label.appendChild(pathEl);
        row.appendChild(label);
        const btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'changes-item-action';
        if (sec.title === 'Staged') {
          btn.textContent = 'Unstage';
          btn.title = `Unstage ${ent.path}`;
          btn.onclick = (e) => {
            e.stopPropagation();
            unstagePath(ent.path, btn);
          };
        } else {
          // Unstaged / Untracked (and Conflicts, where staging is the
          // sensible next step) rows stage their path.
          btn.textContent = 'Stage';
          btn.title = `Stage ${ent.path}`;
          btn.onclick = (e) => {
            e.stopPropagation();
            stagePath(ent.path, btn);
          };
        }
        row.appendChild(btn);
        wrap.appendChild(row);
      }
      list.appendChild(wrap);
    }
  } catch (err) {
    // A failed status call (no repo, git error) has no reliable branch.
    gitBranch = null;
    renderStatusBranch();
    const countEl = $('changes-count');
    if (countEl) {
      countEl.textContent = '';
      countEl.hidden = true;
    }
    list.textContent = err.message;
  }
}

// ── Git mutations (stage / unstage / push) ──
// gitErrorToast surfaces git failures. Errors mentioning index.lock are
// transient — a concurrent git operation (e.g. the agent, whose git tools
// deliberately do NOT take the editor fsMu) holds the lock — so they get a
// retryable wording instead of a generic failure.
function gitErrorToast(err, prefix) {
  const msg = err && err.message ? err.message : String(err);
  if (/index\.lock|unable to lock/i.test(msg)) {
    toast(`${prefix}: git is busy (index.lock) — retry in a moment`, 'error');
  } else {
    toast(`${prefix}: ${msg}`, 'error');
  }
}

// stagePath stages one Changes-panel row (git_stage with a single path).
// The row's button is disabled while the request is in flight; the panel
// re-renders on success so the row moves to the Staged section.
async function stagePath(path, btn) {
  if (btn && btn.disabled) return; // request already in flight
  if (btn) btn.disabled = true;
  try {
    await wsRequest('git_stage', { paths: [path] }, WS_REQUEST_SLOW_TIMEOUT_MS);
    toast(`Staged ${basename(path)}`, 'success');
    await refreshGitStatus();
  } catch (err) {
    gitErrorToast(err, 'Stage failed');
  } finally {
    if (btn) btn.disabled = false;
  }
}

// unstagePath unstages one Staged row (git_unstage with a single path).
async function unstagePath(path, btn) {
  if (btn && btn.disabled) return; // request already in flight
  if (btn) btn.disabled = true;
  try {
    await wsRequest('git_unstage', { paths: [path] }, WS_REQUEST_SLOW_TIMEOUT_MS);
    toast(`Unstaged ${basename(path)}`, 'success');
    await refreshGitStatus();
  } catch (err) {
    gitErrorToast(err, 'Unstage failed');
  } finally {
    if (btn) btn.disabled = false;
  }
}

// pushBranch pushes the current branch to origin (git_push; the server
// runs a fixed argv, no user-controlled arguments).
async function pushBranch() {
  const btn = $('btn-push');
  if (!btn || btn.disabled) return; // request already in flight
  btn.disabled = true;
  const prevLabel = btn.textContent;
  btn.textContent = 'Pushing…';
  try {
    await wsRequest('git_push', {}, WS_REQUEST_SLOW_TIMEOUT_MS);
    toast('Pushed', 'success');
  } catch (err) {
    gitErrorToast(err, 'Push failed');
  } finally {
    btn.disabled = false;
    btn.textContent = prevLabel;
  }
}

// ── Commit composer ──
// Composer in the Changes sidebar. "AI" fills the textarea from the staged
// diff via a one-shot LLM call (git_commit_message) — it never creates or
// touches a chat session and never auto-commits. "Stage all before commit"
// runs git_stage (empty paths = `git add -A`) before committing.

async function generateCommitMessage() {
  const ta = $('commit-message');
  const btn = $('btn-commit-ai');
  if (!ta || !btn) return;
  if (btn.disabled) return; // request already in flight
  btn.disabled = true;
  const prevLabel = btn.textContent;
  btn.textContent = 'Generating…';
  try {
    const data = await wsRequest('git_commit_message', {}, WS_REQUEST_SLOW_TIMEOUT_MS);
    ta.value = (data.content || '').trim();
    if (ta.value) {
      ta.focus();
      toast('Commit message generated', 'success');
    }
  } catch (err) {
    // Keep the composer usable: the textarea stays as-is, the error toasts.
    toast(`Generate failed: ${err.message}`, 'error');
  } finally {
    btn.disabled = false;
    btn.textContent = prevLabel;
  }
}

async function commitStaged() {
  const ta = $('commit-message');
  const btn = $('btn-commit');
  if (!ta || !btn) return;
  if (btn.disabled) return; // request already in flight
  const message = ta.value.trim();
  if (!message) {
    toast('Commit message is required', 'error');
    return;
  }
  btn.disabled = true;
  const prevLabel = btn.textContent;
  btn.textContent = 'Committing…';
  try {
    if ($('stage-all')?.checked) {
      await wsRequest('git_stage', {}, WS_REQUEST_SLOW_TIMEOUT_MS);
    }
    await wsRequest('git_commit', { content: message }, WS_REQUEST_SLOW_TIMEOUT_MS);
    ta.value = '';
    toast('Committed', 'success');
    await refreshGitStatus();
  } catch (err) {
    gitErrorToast(err, 'Commit failed');
  } finally {
    btn.disabled = false;
    btn.textContent = prevLabel;
  }
}

export async function refreshExplorer() {
  const tree = $('file-tree');
  if (tree) await loadTree('.', tree);
  await refreshGitStatus();
}

export function focusFindInFiles() {
  switchToEditorPane();
  // The find input lives in the explorer sidebar, which is a closed drawer
  // on mobile — open it so the input is visible and the keyboard focus
  // lands somewhere the user can see.
  if (window.innerWidth <= 768) {
    $('editor-sidebar')?.classList.add('open');
  }
  const input = $('find-in-files-input');
  if (input) {
    input.focus();
    input.select();
  }
}

function switchToEditorPane() {
  const editorPane = $('editor-pane');
  const alreadyActive = !!editorPane && editorPane.classList.contains('active');
  document.querySelectorAll('.main-tab').forEach((t) => {
    const on = t.dataset.pane === 'editor';
    t.classList.toggle('active', on);
    // Keep the tab's assistive-tech state in sync (app.js switchMainPane).
    if (on) t.setAttribute('aria-current', 'true');
    else t.removeAttribute('aria-current');
  });
  document.querySelectorAll('.pane').forEach((p) => {
    p.classList.toggle('active', p.id === 'editor-pane');
  });
  // Refresh the explorer only when actually entering the pane. Re-rendering
  // while already active would rebuild the file tree from the root and
  // collapse every expanded folder (openFile -> switchToEditorPane under the
  // "Open & switch to editor" chat setting).
  if (!alreadyActive) {
    initMonaco().then(() => refreshExplorer()).catch(() => {});
  }
}

/** Reflect the current find-in-files case mode on the toggle button. */
function syncFindCaseButton() {
  const btn = $('find-in-files-case');
  if (!btn) return;
  btn.setAttribute('aria-pressed', findCaseSensitive ? 'true' : 'false');
  btn.classList.toggle('active', findCaseSensitive);
  btn.title = findCaseSensitive ? 'Match case (on) — click to ignore case' : 'Ignore case — click to match case';
}

async function runFindInFiles(pattern) {
  const results = $('find-in-files-results');
  if (!results) return;
  const q = (pattern || '').trim();
  if (!q) {
    results.textContent = '';
    return;
  }
  const gen = ++searchGen;
  results.textContent = 'Searching…';
  const globInput = $('find-in-files-glob');
  const glob = globInput ? globInput.value.trim() : '';
  const ignoreCase = !findCaseSensitive;
  try {
    const data = await wsRequest('fs_search', { pattern: q, glob, ignoreCase }, WS_REQUEST_SLOW_TIMEOUT_MS);
    if (gen !== searchGen) return;
    renderSearchResults(results, data.matches || [], data.truncated, q, ignoreCase);
  } catch (err) {
    if (gen !== searchGen) return;
    results.textContent = err.message;
    toast(`Search failed: ${err.message}`, 'error');
  }
}

/** Render the summary + per-file collapsible groups for a search result set. */
function renderSearchResults(container, matches, truncated, pattern, ignoreCase) {
  container.innerHTML = '';
  const byFile = new Map();
  for (const m of matches) {
    if (!byFile.has(m.path)) byFile.set(m.path, []);
    byFile.get(m.path).push(m);
  }
  const summary = document.createElement('div');
  summary.className = 'search-summary';
  summary.textContent = matches.length
    ? `${matches.length} match${matches.length === 1 ? '' : 'es'} in ${byFile.size} file${byFile.size === 1 ? '' : 's'}`
    : 'No matches';
  container.appendChild(summary);
  if (truncated) {
    const note = document.createElement('div');
    note.className = 'search-note';
    note.textContent = 'Results truncated';
    container.appendChild(note);
  }
  for (const [path, fileMatches] of byFile) {
    container.appendChild(buildSearchFileGroup(path, fileMatches, pattern, ignoreCase));
  }
}

/** One collapsible per-file group of matches. */
function buildSearchFileGroup(path, matches, pattern, ignoreCase) {
  const group = document.createElement('div');
  group.className = 'search-file';
  const head = document.createElement('button');
  head.type = 'button';
  head.className = 'search-file-head';
  head.setAttribute('aria-expanded', 'true');
  head.title = path;
  const chevron = document.createElement('span');
  chevron.className = 'search-file-chevron';
  chevron.innerHTML = iconSvg('chevron-down');
  const name = document.createElement('span');
  name.className = 'search-file-name';
  name.textContent = basename(path);
  const count = document.createElement('span');
  count.className = 'search-file-count';
  count.textContent = String(matches.length);
  head.append(chevron, name, count);

  const body = document.createElement('div');
  body.className = 'search-file-matches';
  for (const m of matches) body.appendChild(buildSearchMatchRow(m, pattern, ignoreCase));

  head.addEventListener('click', () => {
    const open = body.hidden;
    body.hidden = !open;
    head.setAttribute('aria-expanded', open ? 'true' : 'false');
    chevron.innerHTML = iconSvg(open ? 'chevron-down' : 'chevron-right');
  });
  group.append(head, body);
  return group;
}

/** One match row: line number + highlighted source text; click opens the file. */
function buildSearchMatchRow(m, pattern, ignoreCase) {
  const row = document.createElement('div');
  row.className = 'search-result';
  row.title = `${m.path}:${m.line}`;
  row.onclick = () => { openFile(m.path, m.line).catch((e) => toast(e.message, 'error')); };
  const line = document.createElement('span');
  line.className = 'search-result-line';
  line.textContent = String(m.line);
  const text = document.createElement('span');
  text.className = 'search-result-text';
  appendHighlighted(text, m.text || '', pattern, ignoreCase);
  row.append(line, text);
  return row;
}

// appendHighlighted appends `text` to `parent`, wrapping pattern matches in
// <mark> nodes. Mirrors the server's pattern handling: regex with a literal
// fallback, case-insensitive when ignoreCase. Builds DOM nodes, never HTML.
function appendHighlighted(parent, text, pattern, ignoreCase) {
  const re = searchHighlightRegex(pattern, ignoreCase);
  if (!re || !text) {
    parent.textContent = text;
    return;
  }
  let last = 0;
  let match;
  while ((match = re.exec(text)) !== null) {
    if (match.index > last) parent.appendChild(document.createTextNode(text.slice(last, match.index)));
    const mark = document.createElement('mark');
    mark.className = 'search-hit';
    mark.textContent = match[0];
    parent.appendChild(mark);
    last = match.index + match[0].length;
    if (match[0] === '') re.lastIndex++; // zero-length match: advance
  }
  if (last < text.length) parent.appendChild(document.createTextNode(text.slice(last)));
}

/** Compile the search pattern for client-side highlighting (regex with a
 *  literal fallback), or null when there is no usable pattern. */
function searchHighlightRegex(pattern, ignoreCase) {
  const src = (pattern || '').trim();
  if (!src) return null;
  const flags = ignoreCase ? 'gi' : 'g';
  try {
    return new RegExp(src, flags);
  } catch (_) {
    return new RegExp(src.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), flags);
  }
}

// --- Search & Replace ---

/**
 * Refresh open editor buffers for files that a global replace modified on
 * disk, so saving an affected tab later cannot revert the replacement:
 * - clean buffers are reloaded from disk via fs_read;
 * - dirty buffers keep the user's unsaved edits, but the same
 *   pattern→replacement is applied in memory (best effort: JS regex
 *   semantics may differ from the server's for exotic patterns, and a
 *   pattern that is not a valid regex is treated as a literal).
 * @param {string[]} affectedPaths - workspace-relative paths from fs_replace_result
 * @param {string} pattern - the search pattern that was replaced
 * @param {string} replacement - the replacement string
 */
async function syncBuffersAfterReplace(affectedPaths, pattern, replacement) {
  const affected = new Set(affectedPaths || []);
  for (const path of [...buffers.keys()]) {
    if (!affected.has(path)) continue;
    const b = buffers.get(path);
    if (!b || !b.model) continue;
    if (!isDirty(path)) {
      try {
        const data = await wsRequest('fs_read', { path });
        if (data && !data.error) {
          b.model.setValue(data.content || '');
          b.savedVersionId = b.model.getAlternativeVersionId();
        }
      } catch (err) {
        toast(`Could not refresh ${basename(path)}: ${err.message || 'read failed'}`, 'error');
      }
    } else {
      applyReplaceInModel(b.model, pattern, replacement);
    }
  }
  updateDirtyIndicators();
}

/**
 * Apply a pattern→replacement to a Monaco model in place, preserving the
 * undo stack (pushEditOperations) and the dirty state.
 */
function applyReplaceInModel(model, pattern, replacement) {
  const value = model.getValue();
  let next;
  try {
    next = value.replace(new RegExp(pattern, 'g'), replacement);
  } catch {
    next = value.split(pattern).join(replacement);
  }
  if (next === value) return;
  model.pushEditOperations(
    [{ range: model.getFullModelRange(), text: next, forceMoveStack: true }],
    () => model.getFullModelRange()
  );
}

/**
 * Replace all occurrences of `search` with `replace` across matching files.
 * Uses the backend `fs_replace` message which performs a safe text replacement.
 * @param {string} [scopePath] - If provided, restrict replacement to this file.
 */
async function runReplaceAll(searchPattern, replacement, scopePath) {
  const results = $('find-in-files-results');
  if (!results) return;
  const q = (searchPattern || '').trim();
  const r = (replacement || '');
  if (!q) {
    toast('Search pattern is empty', 'error');
    return;
  }
  const gen = ++searchGen;
  results.textContent = 'Replacing…';
  try {
    const payload = { pattern: q, replacement: r };
    if (scopePath) payload.path = scopePath;
    const data = await wsRequest('fs_replace', payload, WS_REQUEST_SLOW_TIMEOUT_MS);
    if (gen !== searchGen) return;
    if (data.replaced && data.replaced > 0) {
      toast(`Replaced ${data.replaced} occurrence(s) in ${data.fileCount || '?'} file(s)`, 'success');
      // The files changed on disk — refresh any affected open buffers so a
      // later save cannot overwrite the replacement with a stale model.
      await syncBuffersAfterReplace(data.files, q, r);
      // Re-run the search to show updated results
      runFindInFiles(searchPattern);
    } else {
      results.textContent = 'No matches to replace';
    }
  } catch (err) {
    if (gen !== searchGen) return;
    results.textContent = err.message;
    toast(`Replace failed: ${err.message}`, 'error');
  }
}

/**
 * Escape HTML special characters for safe insertion into HTML strings.
 * Covers &, <, >, " and ' while safely handling null/undefined/numbers.
 */
export function escapeHtml(str) {
    if (str == null) return '';
    const map = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
    return String(str).replace(/[&<>"']/g, (ch) => map[ch]);
}

export function setupEditorUI() {
  // Mobile-only file-explorer drawer: toggle button + close on outside tap
  // (mirrors the chat sidebar's behavior in app.js).
  $('editor-sidebar-toggle')?.addEventListener('click', () => {
    $('editor-sidebar')?.classList.toggle('open');
  });
  document.addEventListener('click', (e) => {
    if (window.innerWidth > 768) return;
    const sb = $('editor-sidebar');
    if (!sb || !sb.classList.contains('open')) return;
    if (sb.contains(e.target) || e.target === $('editor-sidebar-toggle')) return;
    sb.classList.remove('open');
  });
  $('btn-refresh-explorer')?.addEventListener('click', () => {
    refreshExplorer().catch((e) => toast(e.message, 'error'));
  });
  // File-tree header actions: new file / collapse all.
  $('btn-collapse-tree')?.addEventListener('click', collapseTree);
  $('btn-new-file')?.addEventListener('click', () => {
    const row = $('new-file-row');
    const input = $('new-file-input');
    if (!row || !input) return;
    row.hidden = false;
    input.value = '';
    input.focus();
  });
  $('new-file-create')?.addEventListener('click', () => {
    createNewFile().catch((e) => toast(e.message, 'error'));
  });
  $('new-file-cancel')?.addEventListener('click', () => {
    const row = $('new-file-row');
    if (row) row.hidden = true;
  });
  $('new-file-input')?.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      createNewFile().catch((err) => toast(err.message, 'error'));
    } else if (e.key === 'Escape') {
      e.preventDefault();
      const row = $('new-file-row');
      if (row) row.hidden = true;
    }
  });
  // File-tree keyboard navigation (role=tree): arrows move/expand, Enter
  // opens, Home/End jump. Focus is roving — one row is tabbable at a time.
  const fileTree = $('file-tree');
  if (fileTree) {
    fileTree.addEventListener('keydown', (e) => {
      const row = e.target.closest('.tree-item');
      if (!row) return;
      const rows = visibleTreeRows();
      const idx = rows.indexOf(row);
      if (idx < 0) return;
      switch (e.key) {
        case 'ArrowDown':
          e.preventDefault();
          focusTreeRow(rows[Math.min(idx + 1, rows.length - 1)]);
          return;
        case 'ArrowUp':
          e.preventDefault();
          focusTreeRow(rows[Math.max(idx - 1, 0)]);
          return;
        case 'Home':
          e.preventDefault();
          focusTreeRow(rows[0]);
          return;
        case 'End':
          e.preventDefault();
          focusTreeRow(rows[rows.length - 1]);
          return;
        case 'Enter':
        case ' ':
          e.preventDefault();
          row.click();
          return;
        case 'ArrowRight':
          e.preventDefault();
          if (row.getAttribute('aria-expanded') === 'false') row.click();
          else if (row.classList.contains('dir') && rows[idx + 1]) focusTreeRow(rows[idx + 1]);
          return;
        case 'ArrowLeft': {
          e.preventDefault();
          if (row.getAttribute('aria-expanded') === 'true') {
            row.click();
            return;
          }
          const group = row.parentElement && row.parentElement.closest('.tree-children');
          const parentRow = group && group.previousElementSibling;
          if (parentRow && parentRow.classList.contains('tree-item')) focusTreeRow(parentRow);
          return;
        }
      }
    });
  }
  $('btn-commit-ai')?.addEventListener('click', () => {
    generateCommitMessage().catch((e) => toast(e.message, 'error'));
  });
  $('btn-commit')?.addEventListener('click', () => {
    commitStaged().catch((e) => toast(e.message, 'error'));
  });
  $('btn-push')?.addEventListener('click', () => {
    pushBranch().catch((e) => toast(e.message, 'error'));
  });
  // Commit composer collapse (persisted; default expanded).
  {
    const composer = $('commit-composer');
    const toggle = $('commit-toggle');
    const COLLAPSE_KEY = 'gogen_commit_collapsed';
    const applyCommitCollapsed = (collapsed) => {
      if (composer) composer.classList.toggle('collapsed', collapsed);
      if (toggle) {
        toggle.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
        toggle.innerHTML = iconSvg(collapsed ? 'chevron-up' : 'chevron-down');
      }
    };
    let collapsed = storageGet(COLLAPSE_KEY) === '1';
    applyCommitCollapsed(collapsed);
    toggle?.addEventListener('click', () => {
      collapsed = !collapsed;
      applyCommitCollapsed(collapsed);
      storageSet(COLLAPSE_KEY, collapsed ? '1' : '0');
    });
  }
  // Ctrl/Cmd+Enter in the message textarea commits (mirrors the chat input).
  $('commit-message')?.addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
      e.preventDefault();
      commitStaged().catch((err) => toast(err.message, 'error'));
    }
  });
  $('btn-save-file')?.addEventListener('click', () => saveActive());
  $('btn-save-all')?.addEventListener('click', () => saveAll());
  $('btn-undo')?.addEventListener('click', () => editorUndo());
  $('btn-redo')?.addEventListener('click', () => editorRedo());
  $('btn-format')?.addEventListener('click', () => formatActive());
  $('btn-diff-prev')?.addEventListener('click', () => diffNav('prev'));
  $('btn-diff-next')?.addEventListener('click', () => diffNav('next'));
  // Keyboard shortcuts help modal (toolbar "?" button).
  $('btn-keys-help')?.addEventListener('click', () => {
    const overlay = $('keybindings-overlay');
    if (overlay) openModal(overlay);
  });
  $('keybindings-close-btn')?.addEventListener('click', () => {
    closeModal($('keybindings-overlay'));
  });
  const kbOverlay = $('keybindings-overlay');
  if (kbOverlay) {
    kbOverlay.addEventListener('click', (e) => {
      if (e.target === kbOverlay) closeModal(kbOverlay);
    });
  }
  // Sync the toolbar button label with the persisted layout (the HTML
  // default says "Side-by-side" even when the saved preference is Inline).
  {
    const lbl = $('btn-diff-layout');
    if (lbl) lbl.textContent = GOGEN_UI.diffRenderSideBySide ? 'Side-by-side' : 'Inline';
  }
  $('btn-diff-layout')?.addEventListener('click', () => {
    GOGEN_UI.diffRenderSideBySide = !GOGEN_UI.diffRenderSideBySide;
    storageSet('gogen_diff_layout', GOGEN_UI.diffRenderSideBySide ? 'side-by-side' : 'inline');
    if (diffEditor) {
      diffEditor.updateOptions({ renderSideBySide: GOGEN_UI.diffRenderSideBySide });
    }
    const btn = $('btn-diff-layout');
    if (btn) btn.textContent = GOGEN_UI.diffRenderSideBySide ? 'Side-by-side' : 'Inline';
  });
  const searchInput = $('find-in-files-input');
  if (searchInput) {
    searchInput.addEventListener('input', () => {
      clearTimeout(searchDebounceTimer);
      searchDebounceTimer = setTimeout(() => {
        runFindInFiles(searchInput.value);
      }, 250);
    });
    searchInput.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        clearTimeout(searchDebounceTimer);
        runFindInFiles(searchInput.value);
      }
    });
  }
  // Find-in-files options: "Match case" toggle + include-glob filter.
  syncFindCaseButton();
  const globInput = $('find-in-files-glob');
  if (globInput) {
    globInput.value = findGlob;
    globInput.addEventListener('input', () => {
      findGlob = globInput.value;
      storageSet('gogen_find_glob', findGlob);
      clearTimeout(searchDebounceTimer);
      searchDebounceTimer = setTimeout(() => {
        if (searchInput) runFindInFiles(searchInput.value);
      }, 250);
    });
    globInput.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        if (searchInput) runFindInFiles(searchInput.value);
      }
    });
  }
  $('find-in-files-case')?.addEventListener('click', () => {
    findCaseSensitive = !findCaseSensitive;
    storageSet('gogen_find_case', findCaseSensitive ? '1' : '0');
    syncFindCaseButton();
    if (searchInput && searchInput.value.trim()) runFindInFiles(searchInput.value);
  });
  // Keyboard shortcut: Ctrl+H opens the replace field
  document.addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key === 'h') {
      e.preventDefault();
      if (e.shiftKey) {
        // Ctrl+Shift+H: toggle sidebar replace row
        const row = $('find-in-files-replace-row');
        if (row) {
          row.classList.toggle('open');
          if (row.classList.contains('open')) {
            $('find-in-files-replace-input')?.focus();
          } else {
            $('find-in-files-input')?.focus();
          }
        }
      } else {
        // Ctrl+H: open Monaco's built-in find/replace widget
        if (editor) {
          editor.getAction('editor.action.startFindReplaceAction').run();
        }
      }
    }
  });

  // --- Editor tabs: context menu, keyboard navigation, cycling ---

  // Delegate menu-row clicks: the menu is static markup; rows carry a
  // data-action the dispatcher maps to a close/copy operation.
  const tabMenu = $('tab-context-menu');
  if (tabMenu) {
    tabMenu.addEventListener('click', (e) => {
      const row = e.target.closest('.tab-menu-row');
      if (!row || row.disabled) return;
      runTabMenuAction(row.dataset.action);
    });
    // Dismiss on any interaction outside the menu (Escape, scroll, resize).
    document.addEventListener('click', (e) => {
      if (!tabMenu.hidden && !tabMenu.contains(e.target)) closeTabMenu();
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') closeTabMenu();
    });
    document.addEventListener('scroll', () => closeTabMenu(), true);
    window.addEventListener('resize', () => closeTabMenu());
  }

  // Roving keyboard navigation for the tab strip (role=tablist): Left/Right
  // move between tabs, Enter/Space activates, Delete closes. The close
  // button keeps its own native click/Enter handling.
  const tabStrip = $('editor-tabs');
  if (tabStrip) {
    tabStrip.addEventListener('keydown', (e) => {
      const tab = e.target.closest('.file-tab');
      if (!tab || e.target.closest('.file-tab-close')) return;
      const path = tab.dataset.path;
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        activatePath(path);
      } else if (e.key === 'Delete') {
        e.preventDefault();
        closeTab(path);
      } else if (e.key === 'ArrowLeft' || e.key === 'ArrowRight') {
        e.preventDefault();
        const idx = openOrder.indexOf(path);
        if (idx < 0 || openOrder.length < 2) return;
        const next = e.key === 'ArrowRight'
          ? (idx + 1) % openOrder.length
          : (idx - 1 + openOrder.length) % openOrder.length;
        activatePath(openOrder[next]);
        // activatePath moves focus into the editor; keep it on the tab that
        // the arrow key just moved to (the strip re-rendered).
        const el = tabStrip.querySelector('.file-tab.active');
        if (el) el.focus();
      }
    });
  }

  // Ctrl/Cmd+PageDown / PageUp cycle tabs (Ctrl+W is browser-reserved, so it
  // is deliberately not used). Only while the editor pane is showing.
  document.addEventListener('keydown', (e) => {
    const mod = e.ctrlKey || e.metaKey;
    if (!mod || (e.key !== 'PageDown' && e.key !== 'PageUp')) return;
    const pane = $('editor-pane');
    if (!pane || !pane.classList.contains('active') || !openOrder.length) return;
    e.preventDefault();
    const idx = Math.max(0, openOrder.indexOf(activePath));
    const next = e.key === 'PageDown'
      ? (idx + 1) % openOrder.length
      : (idx - 1 + openOrder.length) % openOrder.length;
    activatePath(openOrder[next]);
  });

  // --- Replace toggle & preview modal ---

  // Toggle replace row visibility
  $('btn-toggle-replace')?.addEventListener('click', () => {
    const row = $('find-in-files-replace-row');
    if (!row) return;
    row.classList.toggle('open');
    if (row.classList.contains('open')) {
      $('find-in-files-replace-input')?.focus();
    }
  });

  /**
   * Build the preview HTML for the replace modal.
   * Highlights the search term inside each matching line.
   */
  function buildPreviewHTML(matches, search, replacement) {
    // Group by file
    const byFile = new Map();
    for (const m of matches) {
      if (!byFile.has(m.path)) byFile.set(m.path, []);
      byFile.get(m.path).push(m);
    }

    let html = '';
    for (const [file, fileMatches] of byFile) {
      html += `<div class="rp-file-header">${escapeHtml(file)}</div>`;
      for (const m of fileMatches) {
        const line = m.text || '';
        const highlighted = highlightMatch(line, search);
        html += `<div class="rp-line">`
          + `<span class="rp-line-num">${m.line}</span>`
          + `<span class="rp-line-old">${highlighted}</span>`
          + `<span class="rp-arrow">→</span>`
          + `<span class="rp-line-new">${highlightMatch(line, search, replacement)}</span>`
          + `</div>`;
      }
    }
    return html;
  }

  /**
   * Highlight `search` inside `text` using the same regex semantics as backend search.
   * If `replacement` is provided, show the replaced line instead.
   */
  function highlightMatch(text, search, replacement) {
    let re;
    try {
      re = new RegExp(search, 'g');
    } catch {
      const lit = search.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
      re = new RegExp(lit, 'g');
    }
    if (replacement !== undefined) {
      return escapeHtml(text.replace(re, replacement));
    }
    let out = '';
    let last = 0;
    for (const m of text.matchAll(re)) {
      out += escapeHtml(text.slice(last, m.index));
      out += `<span class="rp-highlight">${escapeHtml(m[0])}</span>`;
      last = m.index + m[0].length;
    }
    out += escapeHtml(text.slice(last));
    return out;
  }

  /**
   * Show the replace preview modal.
   * Returns a Promise that resolves to true (confirm) or false (cancel).
   */
  function showReplacePreview(matches, search, replacement, scopeLabel) {
    return new Promise((resolve) => {
      const overlay = $('replace-preview-overlay');
      const summary = $('replace-preview-summary');
      const body = $('replace-preview-body');
      if (!overlay) { resolve(window.confirm(`Replace ${matches.length} occurrence(s)?`)); return; }

      // Set summary
      const fileCount = new Set(matches.map(m => m.path)).size;
      summary.textContent = `${matches.length} occurrence(s) in ${fileCount} file(s)${scopeLabel ? ' — ' + scopeLabel : ''}`;
      body.innerHTML = buildPreviewHTML(matches, search, replacement);
      openDialog(overlay, {
        confirm: 'rp-confirm',
        cancel: 'rp-cancel',
        onConfirm: () => resolve(true),
        onCancel: () => resolve(false),
      });
    });
  }

  // Shared handler: search first, then show preview modal, then apply on confirm
  async function handleReplace(scopePath, scopeLabel) {
    const search = $('find-in-files-input')?.value || '';
    const replacement = $('find-in-files-replace-input')?.value || '';
    if (!search.trim()) { toast('Search pattern is empty', 'error'); return; }
    try {
      const data = await wsRequest('fs_search', { pattern: search, ...(scopePath ? { path: scopePath } : {}) }, WS_REQUEST_SLOW_TIMEOUT_MS);
      const matches = data.matches || [];
      if (!matches.length) { toast('No matches found', 'info'); return; }
      const confirmed = await showReplacePreview(matches, search, replacement, scopeLabel);
      if (confirmed) {
        await runReplaceAll(search, replacement, scopePath);
      }
    } catch (err) {
      toast(`Search failed: ${err.message}`, 'error');
    }
  }

  $('btn-replace-one')?.addEventListener('click', () => {
    if (!activePath) { toast('No file open', 'error'); return; }
    handleReplace(activePath, `scope: ${activePath}`);
  });
  $('btn-replace-all')?.addEventListener('click', () => {
    handleReplace(null, 'all files');
  });

  updateUndoRedoButtons();

  // Warn before the page unloads with unsaved buffers: reloading or closing
  // the tab would otherwise discard edits silently.
  window.addEventListener('beforeunload', (e) => {
    if (!anyDirtyBuffer()) return;
    e.preventDefault();
    e.returnValue = '';
  });
}

export { saveAll, saveActive };

// --- Chat tool-card Monaco helpers ---

export function extractDiffValue(rawJSON) {
  const idx = rawJSON.indexOf('"diff"');
  if (idx < 0) return { ok: false, value: '', complete: false };
  let rest = rawJSON.slice(idx + 6).replace(/^[ \t]+/, '');
  if (!rest.startsWith(':')) return { ok: false, value: '', complete: false };
  rest = rest.slice(1).replace(/^[ \t]+/, '');
  if (!rest.startsWith('"')) return { ok: false, value: '', complete: false };
  rest = rest.slice(1);
  let out = '';
  for (let i = 0; i < rest.length; i++) {
    const ch = rest[i];
    if (ch === '\\' && i + 1 < rest.length) {
      const n = rest[i + 1];
      if (n === 'n') out += '\n';
      else if (n === 't') out += '\t';
      else if (n === '"') out += '"';
      else if (n === '\\') out += '\\';
      else if (n === 'r') { /* skip */ }
      else {
        out += ch;
        out += n;
      }
      i++;
    } else if (ch === '"') {
      return { ok: true, value: out, complete: true };
    } else {
      out += ch;
    }
  }
  return { ok: out.length > 0, value: out, complete: false };
}

/**
 * Incremental twin of extractDiffValue for the streaming tool-card updater:
 * the accumulated rawArgs only ever grows, so the state carries the decoded
 * value and the scan position and each call processes only the newly appended
 * text. Re-decoding the whole value on every frame was O(args²) over a
 * streamed patch — hundreds of ms of main-thread work for a large diff.
 *
 * State (create it once per card and reuse):
 *   keyed      — the `"diff": "` prefix was found; valueStart is set
 *   valueStart — absolute offset of the value's first character
 *   pos        — next offset to decode
 *   out        — the decoded value so far
 *   complete   — the closing quote was seen; scanning stops forever
 *
 * Returns { ok, value, complete } exactly like extractDiffValue. A trailing
 * lone backslash (an escape split across deltas) is left unconsumed so the
 * next call can finish it.
 */
export function scanDiffValueIncremental(state, rawJSON) {
  if (!state || typeof state !== 'object') return { ok: false, value: '', complete: false };
  if (state.complete) return { ok: true, value: state.out || '', complete: true };
  if (!state.keyed) {
    const idx = rawJSON.indexOf('"diff"');
    if (idx < 0) return { ok: false, value: '', complete: false };
    let rest = rawJSON.slice(idx + 6).replace(/^[ \t]+/, '');
    if (!rest.startsWith(':')) return { ok: false, value: '', complete: false };
    rest = rest.slice(1).replace(/^[ \t]+/, '');
    if (!rest.startsWith('"')) return { ok: false, value: '', complete: false };
    state.keyed = true;
    state.valueStart = rawJSON.length - rest.length + 1; // past the opening quote
    state.pos = state.valueStart;
    state.out = '';
  }
  let out = state.out;
  let i = state.pos;
  for (; i < rawJSON.length; i++) {
    const ch = rawJSON[i];
    if (ch === '\\') {
      if (i + 1 >= rawJSON.length) break; // escape split across deltas: retry later
      const n = rawJSON[i + 1];
      if (n === 'n') out += '\n';
      else if (n === 't') out += '\t';
      else if (n === '"') out += '"';
      else if (n === '\\') out += '\\';
      else if (n === 'r') { /* skip */ }
      else {
        out += ch;
        out += n;
      }
      i++;
    } else if (ch === '"') {
      state.out = out;
      state.pos = i + 1;
      state.complete = true;
      return { ok: true, value: out, complete: true };
    } else {
      out += ch;
    }
  }
  state.out = out;
  state.pos = i;
  return { ok: out.length > 0, value: out, complete: false };
}

const DIFF_MIN_HEIGHT = 80;
const DIFF_MAX_HEIGHT = 400;

/** Resize the container to fit Monaco content, clamped to min/max. */
function resizeDiffContainer(container, ed) {
  if (!container || !ed) return;
  const w = container.clientWidth || 0;
  if (w <= 0) return;
  const h = Math.max(DIFF_MIN_HEIGHT, Math.min(DIFF_MAX_HEIGHT, Math.ceil(ed.getContentHeight())));
  // Hysteresis: ignore sub-2px changes so flex layout and Monaco's async
  // layout passes cannot ping-pong the container height — every height change
  // forces a reflow of the whole chat plus a repin scroll.
  if (container._lastDiffH !== undefined && Math.abs(h - container._lastDiffH) < 2) return;
  container.style.height = h + 'px';
  ed.layout({ width: w, height: h });
  // The diff host grows asynchronously after the tool card is placed (Monaco
  // mounts via a lazy import and lays out after paint), so the card's bottom
  // can end up below the chat fold. Notify the scroll system that the DOM
  // height may have changed; it re-pins the bottom if the user is still there.
  if (h !== container._lastDiffH) {
    container._lastDiffH = h;
    window.dispatchEvent(new CustomEvent('gogen-colorized', { bubbles: false }));
  }
}

export async function mountDiffEditor(container, value, opts = {}) {
  // Keep a fallback <pre> so diffs remain visible if Monaco layout/workers fail.
  container.innerHTML = '';
  container.classList.add('monaco-tool-host');

  const fallback = document.createElement('pre');
  fallback.className = 'diff-fallback';
  container.appendChild(fallback);
  updateDiffFallback(container, value || '');

  try {
    // initMonaco is inside the try so a failed/never-loading Monaco never
    // rejects the caller (which would skip the post-append scroll fixup);
    // the fallback <pre> stays visible instead.
    await initMonaco();
    const host = document.createElement('div');
    host.className = 'monaco-tool-editor';
    host.style.visibility = 'hidden';
    container.appendChild(host);

    // File-line numbers for the diff, derived from the @@ hunks (old numbers
    // for '-' lines, new numbers for '+'/context lines). Served from the
    // incremental cache hanging off the editor (ed.__gogenDiffNums), which
    // mountDiffEditor seeds below and updateDiffEditor keeps in sync as the
    // diff streams in. The old cache keyed on model.getValue() rebuilt the
    // entire diff string per rendered line — O(full diff) while streaming.
    let edRef = null;
    const ed = monaco.editor.create(host, {
      value: value || '',
      language: 'diff',
      readOnly: true,
      // No text cursor in a read-only diff viewer: Monaco's default blinking
      // cursor runs a permanent CSS animation per editor (observed keeping the
      // refresh driver ticking at ~60 Hz across all mounted diff cards).
      cursorBlinking: 'hidden',
      // Fixed host size — avoid ResizeObserver fighting flex layout.
      automaticLayout: false,
      // Let boundary wheel events chain to the chat. Both flags must be off:
      // alwaysConsumeMouseWheel:true swallows wheels even at the edges, and
      // consumeMouseWheelIfScrollbarIsNeeded:true (the editor default) re-
      // swallows them whenever the diff has a scrollbar — which tall diffs
      // always do. With both false, Monaco only consumes wheels it can
      // actually scroll, and edge wheels chain to #messages natively.
      alwaysConsumeMouseWheel: false,
      consumeMouseWheelIfScrollbarIsNeeded: false,
      minimap: { enabled: false },
      fontSize: 12,
      wordWrap: 'on',
      scrollBeyondLastLine: false,
      // Show the *file* line number per diff line (old numbers for '-'
      // lines, new numbers for '+'/context), matching the @@ hunks — not the
      // sequential index of the patch text.
      lineNumbers: (line) => {
        if (!edRef) return '';
        let cache = edRef.__gogenDiffNums;
        if (!cache) {
          // Not seeded yet (never happens after the seed below — mount
          // assigns edRef and the cache back to back); rebuild lazily.
          const model = edRef.getModel();
          cache = edRef.__gogenDiffNums = makeDiffNumsCache(model ? model.getValue() : '');
        }
        const n = cache.nums[line - 1];
        return n || '';
      },
      folding: false,
      renderLineHighlight: 'none',
      ...opts,
    });
    edRef = ed;
    // Seed the streaming state updateDiffEditor maintains: a mirror of the
    // model text (so per-frame no-op checks and append detection never call
    // model.getValue(), which materializes the whole diff string) and the
    // incremental file-line-number cache (see makeDiffNumsCache).
    ed.__gogenDiffValue = value || '';
    ed.__gogenDiffNums = makeDiffNumsCache(value || '');
    chatEditors.add(ed);
    applyUnifiedDiffDecorations(ed);

    // Layout after the tool card has a real width in the flex column.
    requestAnimationFrame(() => {
      try {
        resizeDiffContainer(container, ed);
        // Prefer Monaco once it has painted; hide plain fallback.
        if (host.clientWidth > 0 && host.clientHeight > 0) {
          fallback.style.display = 'none';
          host.style.visibility = 'visible';
          resizeDiffContainer(container, ed);
        }
      } catch (_) { /* keep fallback visible */ }
    });
    return ed;
  } catch (err) {
    console.warn('monaco diff mount failed, using text fallback', err);
    return null;
  }
}

export function updateDiffEditor(ed, value) {
  if (!ed) return;
  const model = ed.getModel();
  if (!model) return;
  // ed.__gogenDiffValue mirrors the model text (mountDiffEditor seeds it and
  // every update below keeps it in sync). Checking the mirror keeps the
  // unchanged short-circuit off model.getValue(), which materializes the
  // whole diff string on every streaming frame.
  const prev = ed.__gogenDiffValue;
  if (prev === value) return;
  const appendOnly = typeof prev === 'string' && value.startsWith(prev) &&
    // Models EOL-normalize on setValue/create; appending raw text next to a
    // previously normalized value would render CRLF inconsistently. Rare —
    // take the slow safe path instead.
    value.indexOf('\r') === -1;
  if (!appendOnly && typeof prev !== 'string' && model.getValue() === value) {
    // Untracked editor (did not come from mountDiffEditor) that already
    // holds this text: adopt tracking and stop, like the old no-op check.
    ed.__gogenDiffValue = value;
    ed.__gogenDiffNums = makeDiffNumsCache(value);
    return;
  }
  const atBottom = diffAtBottom(ed);
  if (appendOnly) {
    // Streaming append (the normal tool_call_delta case): the diff only ever
    // grows at the end, so splice the new tail into the model. The previous
    // model.setValue per delta was the worst CPU path in the UI during a
    // large patch: whole-buffer replace + re-tokenize + scroll reset for
    // every tool_call_delta — O(n²) over the stream (the fallback <pre>
    // already got this fix; the Monaco path had not). applyEdits, not
    // pushEditOperations: this model is a read-only viewer, and applyEdits
    // never records undo elements, so hundreds of deltas cannot accumulate
    // undo-stack state.
    const lastLine = model.getLineCount();
    const lastCol = model.getLineMaxColumn(lastLine);
    model.applyEdits([{
      // Plain IRange object — structurally what monaco.Range wraps; the edit
      // needs no monaco symbol beyond the model itself.
      range: {
        startLineNumber: lastLine,
        startColumn: lastCol,
        endLineNumber: lastLine,
        endColumn: lastCol,
      },
      text: value.slice(prev.length),
    }]);
    ed.__gogenDiffValue = value;
    // Line numbers: extend the incremental cache with just the tail (see
    // extendDiffNumsCache); a full diffLineNumbers rescan per delta was O(n²).
    if (ed.__gogenDiffNums) extendDiffNumsCache(ed.__gogenDiffNums, value);
    else ed.__gogenDiffNums = makeDiffNumsCache(value);
    // Decorations: only the appended tail can introduce classifiable lines —
    // content above the edit point is byte-identical. The line the tail's
    // first segment lands on (the empty line after the previous final
    // newline, the single empty line of an empty model, or the grown
    // incomplete last line) is rescanned: prefix classes can be crossed as a
    // line grows (see appendUnifiedDiffDecorations).
    appendUnifiedDiffDecorations(ed, lastLine, lastCol === 1 ? null : lastLine);
    // The edit sits at the end of the model: the viewport above it is
    // untouched, so a reader scrolled up stays put without any re-reveal.
    // Only a pinned-to-bottom reader needs to follow the new tail.
    if (atBottom) ed.revealLine(model.getLineCount());
  } else {
    // First tracked update, or the rare non-append rewrite (server re-sent a
    // corrected diff): full replace. Preserve the user's reading position —
    // model.setValue resets the scroll to top, so re-reveal: follow the new
    // last line only if the user was at the bottom, otherwise keep the line
    // that was at the top of the view in place.
    const topLine = diffTopVisibleLine(ed);
    model.setValue(value);
    ed.__gogenDiffValue = value;
    ed.__gogenDiffNums = makeDiffNumsCache(value);
    applyUnifiedDiffDecorations(ed);
    if (atBottom) {
      ed.revealLine(model.getLineCount());
    } else if (topLine > 1) {
      ed.revealLine(topLine);
    }
  }
  requestAnimationFrame(() => {
    try {
      const dom = ed.getDomNode();
      const container = dom && dom.parentElement && dom.parentElement.parentElement;
      resizeDiffContainer(container, ed);
    } catch (_) { /* ignore */ }
  });
}

function diffTopVisibleLine(ed) {
  const ranges = ed.getVisibleRanges();
  return ranges && ranges[0] ? ranges[0].startLineNumber : 1;
}

function diffAtBottom(ed) {
  const h = ed.getLayoutInfo().height;
  return ed.getScrollHeight() - ed.getScrollTop() - h < 16;
}

/** Update fallback <pre> inside a monaco-tool-host (always kept in sync). */
export function updateDiffFallback(container, value) {
  if (!container) return;
  let pre = container.querySelector('.diff-fallback');
  const text = value || '';
  if (!pre) {
    pre = document.createElement('pre');
    pre.className = 'diff-fallback';
    container.appendChild(pre);
  }
  const lines = text.split('\n');
  if (lines.length && lines[lines.length - 1] === '') lines.pop(); // trailing newline
  const prev = pre._renderedText;
  if (text === prev) return; // unchanged — nothing to (re)render
  if (prev !== undefined && text.startsWith(prev) && pre._scrollHeight != null) {
    // Append-only delta (the normal streaming case): the content only ever
    // grows at the end, so keep the already-rendered rows and add just the
    // new trailing ones. Rebuilding every row per delta was O(n^2) DOM work
    // (plus a forced layout read) for an n-line diff.
    const top = pre.scrollTop; // scroll-offset read; no layout flush
    appendDiffRows(pre, text, lines);
    // Scroll preservation without a pre-mutation layout read: the content
    // only grew, so shift scrollTop by the growth. That keeps the same
    // distance from the bottom as the full rebuild below (pinned-to-bottom
    // stays pinned; a reader scrolled up into the diff is not yanked). One
    // layout read, after the write. _scrollHeight is re-cached on every
    // call, so a viewport/width change between deltas costs at most one
    // delta of drift before the cache refreshes.
    const h = pre.scrollHeight;
    const growth = Math.max(0, h - pre._scrollHeight);
    pre._scrollHeight = h;
    const maxTop = h - pre.clientHeight;
    pre.scrollTop = maxTop > 0 ? Math.min(Math.max(top + growth, 0), maxTop) : 0;
    return;
  }
  // First render, or the rare non-append rewrite (server re-sent a
  // corrected diff): full rebuild. Rebuilding the <pre> resets its scroll
  // position; preserve it by keeping the same distance from the bottom
  // (streaming content grows append-only, so the visible lines stay put).
  const distFromBottom = Math.max(0, pre.scrollHeight - pre.scrollTop - pre.clientHeight);
  pre.textContent = '';
  const state = { oldN: 0, newN: 0, inHunk: false };
  const nums = new Array(lines.length);
  // Colorize plain-text fallback so diffs stay readable if Monaco fails or
  // in static (tokenizer) mode. Each line is a row with a file-line-number
  // gutter, matching the Monaco viewer's numbering. An incomplete trailing
  // line is only previewed (see appendDiffRows for the invariant).
  const incompleteTail = text !== '' && text.charAt(text.length - 1) !== '\n';
  for (let i = 0; i < lines.length; i++) {
    const incomplete = incompleteTail && i === lines.length - 1;
    nums[i] = diffLineNumbersStep(incomplete ? { ...state } : state, lines[i]);
    pre.appendChild(makeDiffRow(nums[i], lines[i]));
  }
  // Restore the previous reading position (clamped to the new bounds).
  const maxTop = pre.scrollHeight - pre.clientHeight;
  pre.scrollTop = maxTop > 0 ? Math.min(Math.max(maxTop - distFromBottom, 0), maxTop) : 0;
  pre._renderedText = text;
  pre._renderedLines = lines.length;
  pre._nums = nums;
  pre._numsState = state;
  pre._scrollHeight = pre.scrollHeight;
}

// Appends the new trailing lines of an append-only delta to the fallback
// <pre> (see updateDiffFallback). pre._renderedText/_renderedLines/_nums/
// _numsState describe what is already rendered; all four are refreshed.
//
// Number-scan invariant: the scan state (pre._numsState) is only advanced
// past COMPLETE lines. An incomplete trailing line (the text does not end
// with '\n' while it streams in) gets a preview number — computed on a
// state copy — and the state advances past it only once it completes. This
// matters because the hunk-header regex only matches the complete line:
// scanning a partial header would leave inHunk/oldN/newN stale and number
// every following line wrong.
function appendDiffRows(pre, text, lines) {
  const start = pre._renderedLines;
  const prev = pre._renderedText;
  const state = pre._numsState;
  let nums;
  if (prev !== '' && !prev.endsWith('\n')) {
    // The last rendered line was incomplete (number previewed, scan state
    // held back). It has either grown in place or just completed with the
    // incoming newline. The gutter number is unchanged by growth
    // (numbering depends only on the line's first char and the unchanged
    // prefix), but its diff class can change, so re-derive both from the
    // full line.
    const line = lines[start - 1];
    const num = diffLineNumbersStep({ ...state }, line);
    const row = pre.children[start - 1];
    if (row) {
      const code = row.lastElementChild;
      code.className = 'diff-code';
      applyDiffLineClass(code, line);
      code.textContent = line;
      row.firstElementChild.textContent = num || '';
    }
    nums = pre._nums.slice(start - 1);
    nums[0] = num;
    if (text.indexOf('\n', prev.length) !== -1) {
      // The line just completed (the delta carries its terminating newline):
      // advance the scan past it now.
      diffLineNumbersStep(state, line);
    }
  } else {
    nums = pre._nums.slice(start);
  }
  for (let i = start; i < lines.length; i++) {
    const line = lines[i];
    const incomplete = i === lines.length - 1 && text.charAt(text.length - 1) !== '\n';
    nums.push(diffLineNumbersStep(incomplete ? { ...state } : state, line));
    pre.appendChild(makeDiffRow(nums[nums.length - 1], line));
  }
  pre._renderedText = text;
  pre._renderedLines = lines.length;
  pre._nums = nums;
}

// One fallback row: file-line-number gutter + colored code span.
function makeDiffRow(num, line) {
  const row = document.createElement('div');
  row.className = 'diff-row';
  const numEl = document.createElement('span');
  numEl.className = 'diff-num';
  numEl.textContent = num || '';
  const code = document.createElement('span');
  code.className = 'diff-code';
  applyDiffLineClass(code, line);
  code.textContent = line;
  row.appendChild(numEl);
  row.appendChild(code);
  return row;
}

// Per-line color class for the plain-text fallback (meta/hunk/add/del).
function applyDiffLineClass(code, line) {
  if (line.startsWith('+++') || line.startsWith('---') || line.startsWith('diff ') || line.startsWith('index ')) {
    code.classList.add('gogen-diff-meta');
  } else if (line.startsWith('@@')) {
    code.classList.add('gogen-diff-hunk');
  } else if (line.startsWith('+')) {
    code.classList.add('gogen-diff-add');
  } else if (line.startsWith('-')) {
    code.classList.add('gogen-diff-del');
  }
}

// Map each line of a unified diff to a file line number for display. Header
// and hunk-marker lines get '' (no number); '-' lines show the old-file line
// number; '+' and context lines show the new-file line number — matching how
// standard diff viewers number unified diffs.
export function diffLineNumbers(text) {
  return makeDiffNumsCache(text).nums;
}

// Build the incremental file-line-number cache for a diff editor
// (ed.__gogenDiffNums): the exact nums array diffLineNumbers would produce,
// plus the scan state and the trailing incomplete line so
// extendDiffNumsCache can grow the array per streaming delta without
// rescanning the whole diff.
//
// Scan invariant (same one appendDiffRows holds for the fallback renderer):
// the state advances only past COMPLETE lines. While a patch streams in, the
// trailing line may still grow — advancing past a half-received '@@ ...'
// hunk header would number every following line wrong. The incomplete line
// gets a preview number computed on a state copy; the real state advances
// past it only once its terminating newline arrives.
function makeDiffNumsCache(text) {
  const lines = String(text || '').split('\n');
  const state = { oldN: 0, newN: 0, inHunk: false };
  const nums = new Array(lines.length);
  const last = lines.length - 1;
  for (let i = 0; i < last; i++) nums[i] = diffLineNumbersStep(state, lines[i]);
  const incomplete = text !== '' && !text.endsWith('\n');
  nums[last] = diffLineNumbersStep(incomplete ? { ...state } : state, lines[last]);
  return { text, nums, state, lastLine: incomplete ? lines[last] : null };
}

// Extend an incremental line-number cache with an append-only text change
// (text.startsWith(cache.text) — enforced by updateDiffEditor's append-only
// test). Only the new tail is scanned: a full diffLineNumbers rescan per
// tool_call_delta was O(n²) over a streaming patch. Maintains the
// makeDiffNumsCache invariant: state advances past complete lines only; the
// trailing incomplete line keeps a preview number from a state copy.
function extendDiffNumsCache(cache, text) {
  const tail = text.slice(cache.text.length);
  if (tail === '') return;
  const parts = tail.split('\n'); // final part is '' iff text ends with '\n'
  const nums = cache.nums;
  const state = cache.state;
  // The tail's first segment lands on the model's current last line: it
  // either grows the still-streaming line (cache.lastLine) or fills the
  // empty line after the previous final newline. That entry already exists
  // in nums — rewrite it in place, never append.
  const line = (cache.lastLine != null ? cache.lastLine : '') + parts[0];
  if (parts.length === 1) {
    // No newline in the tail: the last line is still streaming. Preview on
    // a state copy (its number cannot actually change — same first
    // character, same state before it).
    nums[nums.length - 1] = diffLineNumbersStep({ ...state }, line);
    cache.lastLine = line;
  } else {
    // The line just completed (its '\n' arrived): scan it with the real state.
    nums[nums.length - 1] = diffLineNumbersStep(state, line);
    for (let i = 1; i < parts.length - 1; i++) {
      nums.push(diffLineNumbersStep(state, parts[i]));
    }
    const last = parts[parts.length - 1];
    if (last === '') {
      // Text ends with '\n': the '' split-entry after the final newline is
      // complete (and a no-op for the scan).
      nums.push(diffLineNumbersStep(state, ''));
      cache.lastLine = null;
    } else {
      // Trailing line still streaming: preview only.
      nums.push(diffLineNumbersStep({ ...state }, last));
      cache.lastLine = last;
    }
  }
  cache.text = text;
}

// One step of the diffLineNumbers scan: returns the displayed number for
// `line` and advances `state` ({ oldN, newN, inHunk }). Both diff renderers
// drive it incrementally (updateDiffFallback's rows and the Monaco path's
// ed.__gogenDiffNums cache) so appended lines don't re-scan the whole diff.
function diffLineNumbersStep(state, line) {
  const h = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(line);
  if (h) {
    state.oldN = parseInt(h[1], 10);
    state.newN = parseInt(h[2], 10);
    state.inHunk = true;
    return '';
  }
  if (!state.inHunk) return '';
  const c = line.charAt(0);
  if (c === '-') return String(state.oldN++);
  if (c === '+') return String(state.newN++);
  if (c === ' ') {
    state.oldN++;
    return String(state.newN++);
  }
  // '\ No newline at end of file' and anything else: no number.
  return '';
}

// Returns the chat diff editor whose DOM node contains the event target, or
// null. Used by the wheel-edge takeover in app.js.
function chatEditorAt(e) {
  const t = e && e.target;
  if (!t || typeof t.closest !== 'function' || !t.closest('.monaco-tool-host')) return null;
  for (const ed of chatEditors) {
    const dom = ed.getDomNode && ed.getDomNode();
    if (dom && dom.contains(t)) return ed;
  }
  return null;
}

// Wheel-boundary state for a chat diff editor, via the editor API. Monaco's
// internal scroll model is transform-based, so DOM scrollTop/scrollHeight on
// .monaco-scrollable-element are unreliable (scrollTop always 0, scrollHeight
// ≈ clientHeight) — the API values are the real ones.
export function chatDiffWheelEdge(e) {
  const ed = chatEditorAt(e);
  if (!ed) return { over: false };
  const st = ed.getScrollTop();
  const sh = ed.getScrollHeight();
  const h = ed.getLayoutInfo().height;
  return {
    over: true,
    atTop: st <= 0,
    atBottom: sh - st - h <= 0,
  };
}

export function disposeChatEditors() {
  for (const ed of chatEditors) {
    try {
      ed.dispose();
    } catch (_) { /* ignore */ }
  }
  chatEditors.clear();
  // The transcript DOM these parked colorize jobs target is being discarded
  // (clearChat): drop them and stop observing their elements.
  releaseColorizeTargets();
}

export { monaco };
