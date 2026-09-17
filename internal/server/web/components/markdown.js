// Markdown rendering pipeline for the GoGen web UI: the shared
// text→sanitized-HTML renderer (marked + DOMPurify), the code-block
// copy-button enhancement, the @path:line linkifier, the .message-body
// wrapper helper, the single-shot full render (setMessageMarkdown) and
// the incremental streaming renderer (renderStreamMarkdown).
//
// This is the "given an element + text, render markdown into it" layer.
// The streaming STATE (currentStreamDiv, appendStreamToken, the rAF
// flush cadence) stays in app.js and drives renderStreamMarkdown; the
// ws handlers and history replay call setMessageMarkdown.
//
// Wiring: app.js calls initMarkdown(deps) once at startup.
//   deps.showToast(message, kind)      — the toast stack
//   deps.copyTextToClipboard(text)     — Promise<boolean> clipboard write
//   deps.getMessageRawStore()          — the WeakMap<el, rawText> (shared
//                                        with the sessions export/resend)
import { marked } from '/vendor/marked.esm.js';
import DOMPurify from '/vendor/dompurify.esm.js';
import { colorizeNode, openFileAtLine, sealStreamColorize } from '/editor.js';

let deps = null;

export function initMarkdown(d) {
    deps = d;
}

// Raw marked parse (no sanitization). The promotion-time reconciliation
// compares RAW output: DOMPurify sanitization is by far the dominant cost of
// that pass (an order of magnitude above the parse) and is only needed for
// HTML that actually lands in the DOM. Sanitization is deterministic, so
// equal raw output implies equal sanitized output — a raw comparison can
// only err toward an unnecessary repaint, which is correct anyway (the
// repaint renders exactly what the final single-shot render would).
function renderMarkdownRaw(text) {
    return marked.parse(text || '', { async: false });
}

function sanitizeMarkdownHTML(raw) {
    return DOMPurify.sanitize(raw, { USE_PROFILES: { html: true } });
}

function renderMarkdownHTML(text) {
    return sanitizeMarkdownHTML(renderMarkdownRaw(text));
}

// Whitespace-normalized form of a rendered HTML string, used to compare
// two renderings of the same completed markdown (the promotion-time
// reconciliation in renderStreamMarkdown). marked separates block-level
// elements with one or more newlines depending on whether it parsed the
// blocks jointly or apart, and that boundary whitespace is never
// significant: blocks only split at blank lines, so the joins are always
// between block-level tags, while whitespace inside a block is
// byte-identical on both sides of the comparison. Collapsing it makes the
// comparison insensitive to the boundary formatting alone.
function normalizeBlockHTML(html) {
    return html.replace(/>\s+</g, '><').trim();
}

// Reconciliation parses the completed prefix in one shot. Past this cap
// the per-block freeze proceeds unreconciled — the prefix only ever
// grows, so once over the cap every later promotion would pay the same
// O(n) parse — and the final full render (setMessageMarkdown) stays the
// corrector. 128 KiB of completed markdown in one bubble is far beyond
// anything a streaming turn produces.
const RECONCILE_MAX_PREFIX = 128 * 1024;

function enhanceCodeBlocksWithCopy(root) {
    if (!root || !root.querySelectorAll) return;
    root.querySelectorAll('pre').forEach((pre) => {
        if (pre.closest('.code-block-wrap')) return;
        const wrap = document.createElement('div');
        wrap.className = 'code-block-wrap';
        pre.parentNode.insertBefore(wrap, pre);
        // Header bar: language chip (when marked tagged the fence)
        // on the left, copy button on the right.
        const head = document.createElement('div');
        head.className = 'code-block-head';
        const codeEl = pre.querySelector('code');
        const langMatch = codeEl && codeEl.className.match(/language-([\w+#.-]+)/);
        if (langMatch) {
            const lang = document.createElement('span');
            lang.className = 'code-lang';
            lang.textContent = langMatch[1];
            head.appendChild(lang);
        }
        wrap.appendChild(head);
        wrap.appendChild(pre);
        const btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'code-copy-btn';
        btn.textContent = 'Copy';
        btn.title = 'Copy code';
        btn.addEventListener('click', async (e) => {
            e.preventDefault();
            e.stopPropagation();
            const code = pre.querySelector('code') || pre;
            const text = code.textContent || '';
            const ok = await deps.copyTextToClipboard(text);
            if (ok) {
                btn.textContent = 'Copied';
                setTimeout(() => { btn.textContent = 'Copy'; }, 1500);
            } else {
                deps.showToast('Copy failed', 'error');
            }
        });
        head.appendChild(btn);
    });
}

// Shared post-render pipeline for a markdown node: the (sanitized) HTML,
// the copy buttons, and the per-code-element plain-text bookkeeping the
// colorize paths rely on. Deliberately no colorize here — renderBlockNode
// owns that split, so the streaming tail can arm its code patch in between.
function renderBlockHtml(node, blockText, opts) {
    // Callers that already hold the rendered HTML (the reconciliation
    // path renders the prefix once and reuses it) skip the re-parse.
    node.innerHTML = opts && opts.html !== undefined ? opts.html : renderMarkdownHTML(blockText);
    enhanceCodeBlocksWithCopy(node);
    // Authoritative plain text per code element. Monaco's colorize replaces
    // the element's content with <br/>-separated spans, so textContent is no
    // longer a plain-text source afterwards; every writer (the colorizer,
    // the streaming patch renderer) updates this instead.
    for (const code of node.querySelectorAll('pre code')) {
        code.__gogenPlain = code.textContent || '';
    }
}

// Shared post-render pipeline for a markdown node: the HTML, copy buttons,
// and Monaco colorize. Cached tokenized blocks inline synchronously (one
// DOM write, no async); uncached ones colorize in the background
// (colorizeNode). `linkify` is enabled only for full single-shot renders —
// the streaming path linkifies once at stream end via setMessageMarkdown.
// `streamingTail` marks the in-flight tail render (content still growing,
// node patched or re-rendered every flush; colorizeNode's governor paces
// the tokenization).
function renderBlockNode(node, blockText, opts) {
    renderBlockHtml(node, blockText, opts);
    colorizeNode(node, { streamingTail: !!(opts && opts.streamingTail) });
    if (opts && opts.linkify) linkifyMessageRefs(node);
}

// ── Streaming tail: single fenced code block patching ──
// A tail whose markdown is exactly one fenced code block is updated by
// appending only the grown text to its <code> element instead of re-parsing
// and re-swapping the whole block on every flush. That keeps the colorized
// spans alive, costs O(delta) per flush instead of O(block), and lets the
// colorize governor amortize its tokenizes. Any other shape (another block
// kind, a rewrite, a non-append delta, a structural surprise) falls back to
// the full renderer, which is always correct.

function firstFenceWord(info) {
    const w = String(info || '').trim().split(/\s+/)[0];
    return w || '';
}

// The code content of `raw` when raw is EXACTLY one fenced code block (the
// opener is the first line; a closer, when present, is the last line with
// nothing after it): { lang, codeText } or null. Mirrors marked's fence
// shape rules for the cases this patch can preserve.
function fenceCodeInfo(raw) {
    const open = /^ {0,3}(`{3,}|~{3,})([^\n]*)\n/.exec(raw);
    if (!open) return null;
    const closeRe = new RegExp('^ {0,3}' + open[1][0] + '{' + open[1].length + ',}[ \\t]*$');
    const codeStart = open[0].length;
    const rest = raw.slice(codeStart);
    const lines = rest.split('\n');
    // A trailing '' element is the artifact of a final newline, not a line.
    const lineCount = lines.length - (lines[lines.length - 1] === '' ? 1 : 0);
    let lineStart = codeStart;
    for (let i = 0; i < lineCount; i++) {
        if (closeRe.test(lines[i])) {
            // The closer ends the block: only a closer that is the LAST line
            // keeps the block patchable (content after it makes the block
            // fence + more, which needs the full renderer).
            if (i !== lineCount - 1) return null;
            return { lang: firstFenceWord(open[2]), codeText: raw.slice(codeStart, lineStart) };
        }
        lineStart += lines[i].length + 1;
    }
    return { lang: firstFenceWord(open[2]), codeText: rest };
}

// marked normalizes a fenced code block's content to end with a newline, so
// the rendered code text is the raw content plus one newline unless it
// already ends with one (or is empty). Keep the display in exactly that
// shape so live and single-shot renders stay identical.
function fenceDisplayText(codeText) {
    if (codeText === '' || codeText.endsWith('\n')) return codeText;
    return codeText + '\n';
}

function appendCodeText(code, delta) {
    if (!delta) return;
    const last = code.lastChild;
    if (last && last.nodeType === 3) last.data += delta;
    else code.appendChild(document.createTextNode(delta));
}

// Remove the trailing newline the display carries for an incomplete last
// line (the plain tail's last character). Returns false when it is not in a
// plain text node (the colored prefix covers it) — the caller then falls
// back to a full render.
function trimCodeTrailingNewline(code) {
    const last = code.lastChild;
    if (!last || last.nodeType !== 3 || !last.data.endsWith('\n')) return false;
    last.data = last.data.slice(0, -1);
    if (last.data === '') last.remove();
    return true;
}

// Update the tail's code block in place. Returns true when the DOM is (or
// needs no) update; false when the caller must fall back to a full render.
function patchTailCodeBlock(node, blockText) {
    const patch = node._gogenTail;
    // Containment (not document connectivity): the invariant is that this
    // code element is still the one inside our tail node — a full re-render
    // replaces it. Works for offscreen/detached containers too.
    if (!patch || !node.contains(patch.code)) return false;
    if (!blockText.endsWith('\n')) return false;
    // CRLF is excluded: the patch appends bytes verbatim, while marked
    // normalizes CRLF to LF on a full render — patching would make a live
    // block diverge from its own final rendering (until the closed fence
    // re-renders it). Fall back to the full renderer.
    if (blockText.includes('\r')) return false;
    // splitStreamBlocks appends exactly one '\n' to the raw in-flight text.
    const raw = blockText.slice(0, -1);
    if (!raw.startsWith(patch.rawText)) return false;
    const info = fenceCodeInfo(raw);
    if (!info || info.lang !== patch.lang) return false;
    if (!info.codeText.startsWith(patch.codeText)) return false;
    const displayNow = fenceDisplayText(info.codeText);
    const displayWas = patch.code.__gogenPlain !== undefined ? patch.code.__gogenPlain : '';
    if (displayNow !== displayWas) {
        if (displayNow.startsWith(displayWas)) {
            appendCodeText(patch.code, displayNow.slice(displayWas.length));
        } else if (displayWas.endsWith('\n') && displayNow.startsWith(displayWas.slice(0, -1))) {
            // The trailing newline was the normalized blank line of an
            // incomplete last line: it moves as the line grows. Drop it from
            // the DOM and append from there.
            if (!trimCodeTrailingNewline(patch.code)) return false;
            appendCodeText(patch.code, displayNow.slice(displayWas.length - 1));
        } else {
            return false;
        }
        patch.code.__gogenPlain = displayNow;
    }
    patch.code.__gogenRaw = info.codeText;
    patch.rawText = raw;
    patch.codeText = info.codeText;
    return true;
}

// After a full render of the tail, remember how to patch it when it is a
// single fenced code block.
function armTailCodePatch(node, blockText) {
    node._gogenTail = null;
    if (!blockText.endsWith('\n')) return;
    // CRLF streams take the full renderer every flush (see patchTailCodeBlock).
    if (blockText.includes('\r')) return;
    const raw = blockText.slice(0, -1);
    const info = fenceCodeInfo(raw);
    if (!info) return;
    const codes = node.querySelectorAll('pre code');
    if (codes.length !== 1) return;
    const code = codes[0];
    if (info.lang) {
        const want = ('language-' + info.lang).toLowerCase();
        if (!code.className.split(/\s+/).some((c) => c.toLowerCase() === want)) return;
    }
    const display = fenceDisplayText(info.codeText);
    const current = code.__gogenPlain !== undefined ? code.__gogenPlain : code.textContent;
    if (current !== display) return;
    code.__gogenPlain = display;
    code.__gogenRaw = info.codeText;
    node._gogenTail = { code, lang: info.lang, rawText: raw, codeText: info.codeText };
}

// The streaming tail renderer: a single fenced code block that only grew is
// patched in place; anything else takes the full block render. Either way
// the tail is handed to the colorize governor.
function renderStreamTail(node, blockText) {
    if (!patchTailCodeBlock(node, blockText)) {
        renderBlockHtml(node, blockText);
        armTailCodePatch(node, blockText);
    }
    colorizeNode(node, { streamingTail: true });
}

// Returns the .message-body wrapper that holds a bubble's flow
// content (markdown, timestamp, model chip), creating it lazily.
// The hover buttons (fork/resend/edit) and the inline-edit bar are
// appended to .message itself, OUTSIDE this wrapper, so any future
// containment on the wrapper can't clip the buttons' overhang past
// the bubble's edge. Non-.message elements (e.g. thought-card bodies)
// pass through.
//
// The wrapper is paint-skipped off-screen via .paint-skip once its
// content is final (see styles.css and components/paint-skip.js),
// never with a bare content-visibility: auto: an unconditional skip
// makes the transcript's scrollHeight fall short of the real height,
// which breaks the scrollbar, the jump button and every scroll-up.
export function msgBody(el) {
    if (!el || !el.classList || !el.classList.contains('message')) return el;
    let body = el.querySelector('.message-body');
    if (!body) {
        body = document.createElement('div');
        body.className = 'message-body';
        el.appendChild(body);
    }
    return body;
}

// Full single-shot render (stream end, history replay, edit/resend,
// thinking finalize). The WHOLE text goes through marked at once:
// per-block renders (renderStreamMarkdown) can split lists at blank
// lines, and this pass is the artifact corrector. The block cache
// from a prior streaming phase must be dropped — its nodes are
// detached by the innerHTML wipe below, and renderStreamMarkdown's
// index-based reconciliation would otherwise trust stale entries.
export function setMessageMarkdown(el, text) {
    el.classList.add('md');
    // Publish the streaming colorization before the re-render replaces it:
    // a code block that was colored through the streaming governor (which
    // never writes the LRU) would otherwise be recreated plain and flash
    // "colored → plain → colored" while an async tokenize re-runs for text
    // that was already colored. The entry makes this render a synchronous
    // cache hit (see sealStreamColorize in editor.js).
    sealStreamColorize(el);
    // Wrap rendered content in a child element so edit-resend can
    // hide/show it without touching appended buttons.
    let textWrap = el.querySelector('.msg-text');
    if (!textWrap) {
        textWrap = document.createElement('div');
        textWrap.className = 'msg-text';
        msgBody(el).appendChild(textWrap);
    }
    delete el._gogenBlocks;
    deps.getMessageRawStore().set(el, text);
    renderBlockNode(textWrap, text, { linkify: true });
}

/**
 * Make `@path:line` (and `@path:start-end`) references in rendered
 * assistant messages clickable. Code blocks, links and already-wrapped
 * nodes are skipped. Clicking opens the file in the editor, reveals the
 * line and highlights the range (matches the "Add Reference to Chat"
 * context-menu format).
 */
function linkifyMessageRefs(root) {
    if (!root || !root.querySelectorAll) return;
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    const candidates = [];
    while (walker.nextNode()) {
        const n = walker.currentNode;
        if (!n.nodeValue || !n.nodeValue.includes('@')) continue;
        const pe = n.parentElement;
        if (pe && pe.closest('pre, code, a')) continue;
        candidates.push(n);
    }
    const re = /(^|\s)@([\w./\\~-]+):(\d+)(?:-(\d+))?/g;
    for (const node of candidates) {
        const text = node.nodeValue;
        const frag = document.createDocumentFragment();
        let last = 0;
        let changed = false;
        let m;
        re.lastIndex = 0;
        while ((m = re.exec(text))) {
            changed = true;
            if (m.index > last) frag.appendChild(document.createTextNode(text.slice(last, m.index)));
            const path = m[2];
            const start = parseInt(m[3], 10);
            const end = m[4] ? parseInt(m[4], 10) : start;
            const span = document.createElement('span');
            span.className = 'file-ref';
            span.textContent = m[1] + '@' + path + ':' + m[3] + (m[4] ? '-' + m[4] : '');
            span.title = `Open ${path}:${start} in editor`;
            span.addEventListener('click', (e) => {
                e.stopPropagation();
                openFileAtLine(path, start, end).catch(() => {});
            });
            frag.appendChild(span);
            last = m.index + m[0].length;
        }
        if (changed) {
            if (last < text.length) frag.appendChild(document.createTextNode(text.slice(last)));
            node.parentNode.replaceChild(frag, node);
        }
    }
}

// ===== Incremental streaming render =====
// Streaming messages are re-rendered on a ~32ms cadence. Re-parsing,
// re-sanitizing and re-swapping the whole accumulated markdown on every
// flush is O(n²) in message length and forces a full reflow per flush.
// Instead we render stable markdown blocks once and only re-render the
// in-flight tail block. Block boundaries are conservative (blank lines
// outside fenced code blocks, with one deliberate exception: a list
// block stays open across blank lines when a same-type list item
// follows, so the live render matches the one-shot loose-list parse),
// so no paragraph is ever rendered partially. Cross-block parser state
// that the splitter cannot foresee (indented continuations,
// reference-style link and footnote definitions) is caught by the
// promotion-time reconciliation in renderStreamMarkdown; the final full
// render at stream end (see endStream/finalizeThinking) remains the
// last-resort corrector.
//
// Splits accumulated stream text into conservative markdown blocks
// (blank lines outside fenced code, with the list-continuation lookahead
// below). Returns { blocks, lastStart }:
// blocks[i] is the i-th block and lastStart is the offset in `text`
// where the LAST block began (text.length when there are no blocks).
// The offset lets the incremental renderer remember the in-flight
// block boundary and re-split only the tail on the next flush.
//
// listMarkerOf classifies a line as a top-level list-item marker
// (indent <= 3 spaces, per CommonMark): bullets (-, *, +) or ordered
// (digits + . or )). Returns null for non-marker lines. Used only by the
// blank-line lookahead to decide whether a list block continues across
// the blank line.
function listMarkerOf(line) {
    const m = /^ {0,3}([-*+]|\d{1,9}[.)])(\s|$)/.exec(line);
    if (!m) return null;
    const tok = m[1];
    if (tok === '-' || tok === '*' || tok === '+') return { kind: 'bullet', ch: tok };
    return { kind: 'ordered', delim: tok[tok.length - 1] };
}

function splitStreamBlocks(text) {
    const src = String(text);
    const blocks = [];
    let cur = '';
    let curStart = 0;   // offset where the current (in-flight) block began
    let lastStart = 0;  // offset where the most recently pushed block began
    let fence = null; // { mark: '`'|'~', len } while inside a fenced code block
    let offset = 0;
    // Marker info while `cur` is a block that OPENS with a list-item line
    // (null for any other block; a fence opening clears it). Drives the
    // blank-line lookahead below. A non-marker line after the opener does
    // NOT clear it: a lazy continuation still renders inside the list in
    // the one-shot parse, so continuation terms are unchanged.
    let list = null;
    const lines = src.split('\n');
    for (let i = 0; i < lines.length; i++) {
        const line = lines[i];
        const lineLen = line.length + 1; // +1 for the '\n'
        if (fence) {
            if (cur === '') curStart = offset;
            cur += line + '\n';
            // CommonMark closing rule: same char, length >= opener,
            // nothing but trailing whitespace after it. This keeps a
            // 3-tick line from closing a 4-tick outer fence (nested
            // code blocks) and "```js" from closing a fence.
            const c = /^\s*(`+|~+)\s*$/.exec(line);
            if (c && c[1][0] === fence.mark && c[1].length >= fence.len) fence = null;
            offset += lineLen;
            continue;
        }
        const m = /^\s*(```+|~~~+)/.exec(line);
        if (m) {
            if (cur === '') curStart = offset;
            cur += line + '\n';
            list = null; // a fence (even indented) ends list-continuation terms
            fence = { mark: m[1][0], len: m[1].length };
            offset += lineLen;
            continue;
        }
        if (line.trim() === '') {
            if (cur !== '') {
                // List continuation lookahead: keep the block open across
                // the blank line(s) when the next non-blank line is another
                // same-type list item — the one-shot parse renders those as
                // ONE loose list, and freezing the block here would paint a
                // tight fragment the final render would reflow (the
                // streaming "jump"). The blank lines stay inside the block
                // text: they are what makes the parse loose. Anything else
                // (paragraph, blockquote, changed marker type) splits as
                // before; residual divergence is caught by the
                // reconciliation in renderStreamMarkdown.
                let j = i + 1;
                while (j < lines.length && lines[j].trim() === '') j++;
                const next = j < lines.length ? lines[j] : null;
                const nextMarker = next ? listMarkerOf(next) : null;
                if (list && nextMarker && nextMarker.kind === list.kind
                    && (list.kind === 'bullet'
                        ? nextMarker.ch === list.ch
                        : nextMarker.delim === list.delim)) {
                    cur += line + '\n';
                    offset += lineLen;
                    continue;
                }
                blocks.push(cur);
                lastStart = curStart;
                cur = '';
                list = null;
            }
            offset += lineLen;
            continue;
        }
        if (cur === '') {
            curStart = offset;
            list = listMarkerOf(line);
        }
        cur += line + '\n';
        offset += lineLen;
    }
    if (cur !== '') {
        blocks.push(cur);
        lastStart = curStart;
    }
    return { blocks, lastStart: blocks.length ? lastStart : src.length };
}

// Incremental renderer for the live streaming paths (assistant stream
// and thinking). Per-element state lives on el._gogenBlocks; the DOM
// shape mirrors setMessageMarkdown (.msg-text wrapper) so edit/resend
// and history replay behave identically. Completed blocks are frozen
// per-block, but their COMBINED rendering is reconciled against the
// one-shot parse of the same completed text at every block promotion
// (see below), so what the user sees mid-stream is what the final
// render will paint — no end-of-stream reflow.
export function renderStreamMarkdown(el, text) {
    el.classList.add('md');
    let textWrap = el.querySelector('.msg-text');
    if (!textWrap) {
        textWrap = document.createElement('div');
        textWrap.className = 'msg-text';
        msgBody(el).appendChild(textWrap);
    }
    const st = el._gogenBlocks || (el._gogenBlocks = {
        done: [],        // per-block frozen nodes painted since the last repaint
        mergedNode: null, // one node holding the whole frozen prefix after a repaint (null in per-block mode)
        rawPrefixHTML: '', // RAW marked HTML of the frozen prefix (per-block concats, or the repaint's joint parse) — the reconcile compare accumulator
        tailNode: null,
        processedLen: 0,
        lastText: null,
    });

    // Incremental split: the streaming text for a given element is
    // append-only (appendStreamToken appends; rewinds start a fresh
    // element; setMessageMarkdown deletes this state), so
    // splitStreamBlocks is prefix-stable — the SPLIT of completed
    // blocks before st.processedLen can never change (their combined
    // RENDERING is still reconciled at each promotion, see below). We
    // therefore re-split only the tail (from the last in-flight block
    // boundary to the end) instead of the whole message, keeping each
    // flush O(tail) rather than O(n). st.processedLen is the offset
    // where the in-flight block began; text[processedLen-1] is the
    // blank line that ends the last completed block, which doubles as a
    // cheap staleness probe.
    //
    // The guards cover the ways the cache can go stale, all O(1) in the
    // common append path: a rewind (text shrank below processedLen), a
    // boundary rewrite (the blank line that ends the last completed
    // block is gone), a same-length content rewrite (text is the same
    // length but differs — the O(n) compare only runs when the length
    // is unchanged, i.e. no new token arrived, so it never costs on the
    // hot path), or a full re-render that detached the nodes while the
    // cache survived (setMessageMarkdown deletes it, but the isConnected
    // check keeps this safe). A longer rewrite (prefix changed but the
    // text grew) is impossible under the append-only invariant — a
    // rewind starts a fresh element — so it is deliberately not probed
    // (doing so would require an O(n) prefix compare every flush, which
    // is exactly the cost this optimization removes). On staleness we
    // drop the cached nodes and re-split from offset 0.
    const stale = text.length < st.processedLen
        || (st.processedLen > 0 && text[st.processedLen - 1] !== '\n')
        || (st.lastText !== null && text.length === st.lastText.length && text !== st.lastText)
        || (st.done.length
            ? !st.done[0].node.isConnected
            : (st.mergedNode !== null && !st.mergedNode.isConnected));
    if (stale) {
        for (const entry of st.done) entry.node.remove();
        if (st.mergedNode) st.mergedNode.remove();
        st.done = [];
        st.mergedNode = null;
        st.rawPrefixHTML = '';
        st.processedLen = 0;
        // A rewrite (shrink / boundary change) invalidates the tail's code
        // patch; the next render re-arms it from scratch.
        if (st.tailNode) st.tailNode._gogenTail = null;
    }

    const tail = text.slice(st.processedLen);
    const { blocks: tailBlocks, lastStart: tailLastStart } = splitStreamBlocks(tail);
    // The last block of the tail is the in-flight block; everything
    // before it is a newly-completed block (promoted from a prior
    // tail). The in-flight block is re-rendered every flush; completed
    // blocks are rendered once and cached.
    const inFlight = tailBlocks[tailBlocks.length - 1] || '';
    const newDone = tailBlocks.slice(0, tailBlocks.length - 1);

    // Promotion: freeze the newly-completed blocks, but first reconcile
    // the WHOLE frozen prefix against the one-shot parse of the same
    // completed text. marked carries state across blocks — list
    // looseness (the blank lines between items), reference-style link
    // and footnote definitions, indented continuations — so per-block
    // renders can differ from the joint parse the final render
    // (setMessageMarkdown) will apply. Left alone, that difference is
    // the end-of-stream reflow. Comparing at promotion time and
    // repainting the prefix from the joint parse keeps the screen
    // identical to the final render at every point in the stream. The
    // compare only runs when a block is promoted (once per blank-line
    // boundary, not per flush), so the hot path stays O(tail): two
    // O(prefix) parses per promotion, never per token flush.
    if (newDone.length) {
        // The blocks being promoted are about to be recreated from HTML —
        // which starts plain — and their colorized DOM lives in the TAIL node
        // right now. Seal exactly that node: sealing the whole message would
        // re-serialize every colorized block in it at every block boundary
        // (Monaco's span-heavy HTML makes that O(prefix) per promotion, felt
        // as the stream slowing down as the answer grows). The reconcile
        // repaint below seals the frozen nodes it is about to replace.
        sealStreamColorize(st.tailNode);
        const prefixText = text.slice(0, st.processedLen + tailLastStart);
        if (prefixText.length <= RECONCILE_MAX_PREFIX) {
            // Compare on RAW marked output (see renderMarkdownRaw): the
            // DOMPurify sanitize that used to run on the joint parse is the
            // dominant cost of this pass and is only needed for HTML that
            // actually lands in the DOM. One raw parse per new block, one for
            // the joint prefix — the sanitize now happens only where the
            // result is rendered.
            const newRaw = newDone.map((block) => renderMarkdownRaw(block));
            const rawPerBlock = st.rawPrefixHTML + newRaw.join('');
            const rawPrefix = renderMarkdownRaw(prefixText);
            if (normalizeBlockHTML(rawPerBlock) !== normalizeBlockHTML(rawPrefix)) {
                // Repaint: every frozen node (and the merged node) is about
                // to be replaced — seal their colorized blocks first so the
                // repaint paints them from the cache instead of flashing
                // plain. This is the rare divergence path, already O(prefix)
                // for the parse above.
                for (const entry of st.done) sealStreamColorize(entry.node);
                sealStreamColorize(st.mergedNode);
                // Divergence: repaint the whole frozen prefix from the
                // joint parse, collapsing it into one node. Order is
                // preserved: the merged node takes the place of every
                // per-block node before the tail.
                for (const entry of st.done) entry.node.remove();
                st.done = [];
                if (!st.mergedNode) {
                    st.mergedNode = document.createElement('div');
                    st.mergedNode.className = 'md-block';
                    if (st.tailNode) {
                        textWrap.insertBefore(st.mergedNode, st.tailNode);
                    } else {
                        textWrap.appendChild(st.mergedNode);
                    }
                }
                renderBlockNode(st.mergedNode, prefixText, { linkify: false, html: sanitizeMarkdownHTML(rawPrefix) });
                st.rawPrefixHTML = rawPrefix;
            } else {
                // Match: freeze per-block as before (each block's raw is
                // sanitized for the DOM; the raw stays the compare
                // accumulator).
                for (let i = 0; i < newDone.length; i++) {
                    const node = document.createElement('div');
                    node.className = 'md-block';
                    renderBlockNode(node, newDone[i], { linkify: false, html: sanitizeMarkdownHTML(newRaw[i]) });
                    if (st.tailNode) {
                        textWrap.insertBefore(node, st.tailNode);
                    } else {
                        textWrap.appendChild(node);
                    }
                    st.done.push({ text: newDone[i], node });
                }
                st.rawPrefixHTML += newRaw.join('');
            }
        } else {
            // Oversized prefix: freeze unreconciled (once over the cap
            // every later promotion would pay the same parse; the final
            // full render corrects any divergence). st.rawPrefixHTML is
            // intentionally left behind: it is only read by the compare
            // above, which can no longer be reached.
            for (const block of newDone) {
                const node = document.createElement('div');
                node.className = 'md-block';
                renderBlockNode(node, block, { linkify: false });
                if (st.tailNode) {
                    textWrap.insertBefore(node, st.tailNode);
                } else {
                    textWrap.appendChild(node);
                }
                st.done.push({ text: block, node });
            }
        }
    }

    // Tail: one stable node. A single fenced code block that only grew is
    // PATCHED in place (append only the delta, keep the colorized spans);
    // any other shape takes the full block render. Either way the node is
    // handed to the colorize governor, which paces the tokenization
    // (streamingTail mode: no LRU writes, size cap, one job in flight).
    if (!st.tailNode) {
        st.tailNode = document.createElement('div');
        st.tailNode.className = 'md-block md-tail';
        textWrap.appendChild(st.tailNode);
    }
    renderStreamTail(st.tailNode, inFlight);

    // Advance the boundary to the start of the in-flight block; it is
    // the only part re-split next flush. (When the tail is all blank
    // lines there is no in-flight block, so consume the whole tail.)
    st.processedLen += tailBlocks.length ? tailLastStart : tail.length;
    st.lastText = text;

    deps.getMessageRawStore().set(el, text);
}

// ===== Message timestamps =====
// Single relative-time helper for messages and session rows. `now` is
