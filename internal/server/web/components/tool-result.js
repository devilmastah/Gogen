// Tool-result rendering for the GoGen web UI: the expandable/collapsible
// result body (summarize + copy + expand), the deferred-during-replay
// Monaco colorize queue, and the compact tool-args formatters shared with
// the tool-card header.
//
// This is the "render a tool's result into a card body" layer, called by
// the tool-card renderers (updateToolCardWithResult) and the streaming
// card header (formatArgsCompact). The tool-card DOM + the streaming
// state (pendingToolCards / streamingToolCards) stay in app.js.
//
// Wiring: app.js calls initToolResult(deps) once at startup.
//   deps.showToast(message, kind)   — the toast stack
//   deps.copyTextToClipboard(text)  — Promise<boolean> clipboard write
//   deps.isReplaying()              — true while a history replay is
//                                     rebuilding the pane (colorize defers)
import { colorizeElement } from '/editor.js';

let deps = null;

export function initToolResult(d) {
    deps = d;
}

// Collapse only when output is genuinely large (DOM / scroll cost).
const BIG_RESULT_CHARS = 32000;
const BIG_RESULT_LINES = 400;
const BIG_ARG_CHARS = 4000;
// Shared with app.js's tool-args formatters (formatToolArgs /
// formatToolArgsFragment) as a function — the web test harness evals each
// module in its own scope, so only function declarations are visible
// cross-module (the components/ sharing convention).
export function bigArgChars() {
    return BIG_ARG_CHARS;
}

// Tool-result colorize requests made while a history replay is in
// flight are deferred until the replay finishes and first paint lands,
// so Monaco tokenization doesn't fight the transcript rebuild for
// main-thread time. Same colorize output, just later.
let deferredResultColorize = [];

function isResultReallyBig(text) {
    const s = text || '';
    if (!s) return false;
    if (s.length > BIG_RESULT_CHARS) return true;
    // Avoid split on huge strings when length already qualifies.
    let lines = 1;
    for (let i = 0; i < s.length; i++) {
        if (s.charCodeAt(i) === 10) {
            lines++;
            if (lines > BIG_RESULT_LINES) return true;
        }
    }
    return false;
}

function summarizeResult(result, success) {
    const trimmed = (result || '').trim();
    if (!trimmed) return success ? '(empty)' : '(no output)';
    // Show full output unless it's really big.
    if (!isResultReallyBig(trimmed)) return trimmed;
    const lines = trimmed.split('\n').length;
    const chars = trimmed.length;
    if (!success) {
        let first = trimmed.split('\n')[0];
        if (first.length > 200) first = first.slice(0, 197) + '...';
        return `${first}\n\n… (${lines} lines, ${chars} chars)`;
    }
    return `(large output: ${lines} lines, ${chars} chars)`;
}

// Tool-result syntax highlighting (Monaco tokenization) is deferred
// while a history replay is rebuilding the pane: restored sessions
// paint the transcript first, then colorize in idle-time slices.
// The colorize itself is unchanged, so restored cards end up looking
// identical to live ones.
function scheduleResultColorize(el, language) {
    if (deps.isReplaying()) {
        deferredResultColorize.push(() => colorizeElement(el, language));
        return;
    }
    colorizeElement(el, language);
}

// Run deferred tool-result colorizes after the replay finishes, in
// ~12ms idle slices so a session with many large results doesn't jank
// the main thread in one block. A job only ENQUEUES colorize work now:
// colorizeElement hands off to the editor scheduler, which owns the
// pacing and the work bound (the replay deferral + viewport gate), so
// awaiting it here merely keeps the enqueue order deterministic.
export function flushDeferredResultColorize() {
    if (!deferredResultColorize.length) return;
    const jobs = deferredResultColorize;
    deferredResultColorize = [];
    let i = 0;
    const runSlice = async () => {
        const deadline = performance.now() + 12;
        while (i < jobs.length && performance.now() < deadline) {
            try { await jobs[i](); } catch (_) { /* keep going */ }
            i++;
        }
        if (i < jobs.length) {
            if (typeof requestIdleCallback === 'function') {
                requestIdleCallback(runSlice, { timeout: 100 });
            } else {
                setTimeout(runSlice, 0);
            }
        }
    };
    if (typeof requestIdleCallback === 'function') {
        requestIdleCallback(runSlice, { timeout: 400 });
    } else {
        setTimeout(runSlice, 50);
    }
}

export function appendExpandableResult(body, result, success, truncated, options = {}) {
    const { language = '' } = options;
    const content = document.createElement('div');
    content.className = `tool-result-content ${success ? '' : 'error-content'}`;
    const full = result || '';
    const summary = summarizeResult(full, success);
    content.textContent = summary;
    body.appendChild(content);

    // Copy button on tool results
    if (full.trim()) {
        const copyBtn = document.createElement('button');
        copyBtn.className = 'tool-result-copy';
        copyBtn.textContent = 'Copy';
        copyBtn.onclick = () => { deps.copyTextToClipboard(full).then((ok) => deps.showToast(ok ? 'Copied' : 'Copy failed', ok ? 'success' : 'error')); };
        body.appendChild(copyBtn);
    }

    const canHighlight = success && language && language !== 'plaintext';
    const showingFull = summary === full.trim();
    if (canHighlight && showingFull) {
        scheduleResultColorize(content, language);
    }

    if (summary !== full.trim() && (full.trim() || truncated)) {
        const expandBtn = document.createElement('button');
        expandBtn.textContent = truncated ? 'Show received output' : 'Show full output';
        expandBtn.className = 'btn-link';
        expandBtn.onclick = () => {
            content.textContent = full;
            content.classList.remove('monaco-colorized');
            delete content.dataset.monacoColorized;
            if (canHighlight) colorizeElement(content, language);
            expandBtn.remove();
        };
        body.appendChild(expandBtn);
    }
}

export function readFilePathFromArgs(args) {
    if (!args || typeof args !== 'object') return '';
    const p = args.file_path || args.path || '';
    return typeof p === 'string' ? p : '';
}

function parseInlineJSONArgs(raw) {
    const s = (raw || '').trim();
    if (!s.startsWith('{')) return null;
    try {
        return JSON.parse(s);
    } catch (_) {
        return null;
    }
}

export function formatArgsCompact(rawJSON, maxLen = BIG_ARG_CHARS) {
    const args = parseInlineJSONArgs(rawJSON);
    if (!args || typeof args !== 'object') return '';
    const parts = [];
    for (const [k, v] of Object.entries(args)) {
        if (k === 'diff') continue;
        let val = typeof v === 'string' ? v : String(v);
        if (val.length > maxLen) val = val.slice(0, maxLen - 3) + '...';
        parts.push(`${k}=${JSON.stringify(val)}`);
    }
    return parts.length ? `(${parts.join(', ')})` : '';
}
