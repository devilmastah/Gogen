// Composer input helpers for the GoGen web UI: the slash-command
// suggest box and the attachment flow (file picker, clipboard paste,
// drag-and-drop onto the input area, preview chips).
//
// The send path itself (sendMessage) stays in app.js — it is glue over
// the socket, panes and stream state. app.js reads the pending
// attachments through getPendingAttachments / clearAttachments, asks
// composeMessageContent to inline text-file attachments into the
// outgoing content, and routes the composer's keydown through
// slashKeydown (true = consumed).
//
// Wiring: app.js calls initComposer(deps) once at startup.
//   deps.showToast(message, kind) — the toast stack
import { icon } from '/components/icons.js';

const inputArea = document.getElementById('message-input');
const slashSuggest = document.getElementById('slash-suggest');
const attachBtn = document.getElementById('attach-btn');
const imageUpload = document.getElementById('image-upload');
const attachmentPreview = document.getElementById('attachment-preview');
// The whole composer row (textarea + buttons) is the drop target —
// dropping on the buttons or the attachment strip must work too.
const composerEl = document.getElementById('input-area');
const dropOverlay = document.getElementById('composer-drop-overlay');

let deps = null;

export function initComposer(d) {
    deps = d;
}

// ── Slash-command suggest ──
// Keep in sync with agent.SlashCommands (Web: true).
const SLASH_COMMANDS = [
    { name: '/help', description: 'Show available commands' },
    { name: '/plan', description: 'Switch to plan (read-only) mode' },
    { name: '/act', description: 'Switch to act mode' },
    { name: '/mode', description: 'Show current mode' },
    { name: '/think', description: 'Set thinking/reasoning level (off/low/medium/high)' },
    { name: '/models', description: 'List or switch models' },
    { name: '/context', description: 'Context usage details' },
    { name: '/new', description: 'Start a new session' },
    { name: '/resume', description: 'List, restore, or delete sessions' },
];
let slashMatches = [];
let slashIndex = 0;

export function getSlashCommands() {
    return SLASH_COMMANDS;
}

export function hideSlashSuggest() {
    slashSuggest.classList.remove('open');
    // hideSlashSuggest runs on every keystroke via
    // updateSlashSuggest; skip the innerHTML write when the box is
    // already empty so the common (non-slash) path is a true no-op.
    if (slashSuggest.childElementCount > 0) slashSuggest.innerHTML = '';
    slashMatches = [];
    slashIndex = 0;
}

function matchSlashCommands(value) {
    // Don't suggest once the user has typed args. Without whitespace
    // the whole value IS the command token, so the split form would
    // just be value.toLowerCase() — drop it.
    if (!value.startsWith('/') || /\s/.test(value)) return [];
    return SLASH_COMMANDS.filter((c) => c.name.startsWith(value.toLowerCase()));
}

function renderSlashSuggest() {
    if (slashMatches.length === 0) {
        hideSlashSuggest();
        return;
    }
    if (slashIndex >= slashMatches.length) slashIndex = 0;
    if (slashIndex < 0) slashIndex = slashMatches.length - 1;
    slashSuggest.innerHTML = '';
    slashMatches.forEach((cmd, i) => {
        const el = document.createElement('div');
        el.className = 'slash-item' + (i === slashIndex ? ' active' : '');
        el.setAttribute('role', 'option');
        el.setAttribute('aria-selected', i === slashIndex ? 'true' : 'false');
        el.innerHTML = '<span class="slash-name"></span><span class="slash-desc"></span>';
        el.querySelector('.slash-name').textContent = cmd.name;
        el.querySelector('.slash-desc').textContent = cmd.description;
        el.onmousedown = (e) => {
            e.preventDefault();
            applySlashCompletion(cmd.name);
        };
        slashSuggest.appendChild(el);
    });
    slashSuggest.classList.add('open');
    const active = slashSuggest.querySelector('.slash-item.active');
    if (active) active.scrollIntoView({ block: 'nearest' });
}

export function updateSlashSuggest() {
    slashMatches = matchSlashCommands(inputArea.value);
    slashIndex = 0;
    renderSlashSuggest();
}

function applySlashCompletion(name) {
    inputArea.value = name + ' ';
    hideSlashSuggest();
    inputArea.focus();
}

export function slashSuggestOpen() {
    return slashSuggest.classList.contains('open') && slashMatches.length > 0;
}

// The composer keydown handling for the suggest box. Returns true when
// the event was consumed (app.js's keydown handler stops there); false
// falls through to send / turn-cancel.
export function slashKeydown(e) {
    if (!slashSuggestOpen()) return false;
    if (e.key === 'ArrowDown') {
        e.preventDefault();
        slashIndex = (slashIndex + 1) % slashMatches.length;
        renderSlashSuggest();
        return true;
    }
    if (e.key === 'ArrowUp') {
        e.preventDefault();
        slashIndex = (slashIndex - 1 + slashMatches.length) % slashMatches.length;
        renderSlashSuggest();
        return true;
    }
    if (e.key === 'Tab') {
        e.preventDefault();
        applySlashCompletion(slashMatches[slashIndex].name);
        return true;
    }
    if (e.key === 'Enter' && !e.shiftKey) {
        const selected = slashMatches[slashIndex].name;
        const token = inputArea.value.split(/\s/, 1)[0];
        if (token.toLowerCase() !== selected.toLowerCase()) {
            e.preventDefault();
            applySlashCompletion(selected);
            return true;
        }
        hideSlashSuggest();
        // fall through to send
        return false;
    }
    if (e.key === 'Escape') {
        e.preventDefault();
        hideSlashSuggest();
        return true;
    }
    return false;
}

// ── Attachments (vision input + text context) ──
// Image limits mirror the server's (internal/server/server.go
// validateImageInputs). The attachment-count cap is shared across both
// kinds; images additionally ride the payload's images array (max 4
// server-side), so the client cap can never exceed it.
const MAX_ATTACHMENTS = 4;
const MAX_ATTACHMENT_BYTES = 5 * 1024 * 1024;
// Text files are capped tighter than images: their content is inlined
// verbatim into the message text (and so into the LLM context and the
// persisted transcript), where even a few hundred KB is a lot of tokens.
const MAX_TEXT_ATTACHMENT_BYTES = 256 * 1024;
let pendingAttachments = []; // images: [{dataUrl, name}] · text files: [{text, name}]

export function getPendingAttachments() {
    return pendingAttachments;
}

function addImageAttachment(file) {
    if (!file) return;
    if (!file.type || !file.type.startsWith('image/')) return;
    if (file.size > MAX_ATTACHMENT_BYTES) {
        deps.showToast(`Image "${file.name}" is larger than 5 MB`, 'error');
        return;
    }
    if (pendingAttachments.length >= MAX_ATTACHMENTS) {
        deps.showToast(`Max ${MAX_ATTACHMENTS} attachments per message`, 'error');
        return;
    }
    const reader = new FileReader();
    reader.onload = () => {
        const dataUrl = String(reader.result || '');
        if (!dataUrl.startsWith('data:image/')) {
            deps.showToast(`"${file.name}" is not a supported image`, 'error');
            return;
        }
        pendingAttachments.push({ dataUrl, name: file.name || 'image' });
        renderAttachmentPreview();
    };
    reader.readAsDataURL(file);
}

// ── Text/code files ──
// Attached as context: read as UTF-8 and inlined into the outgoing
// message content by composeMessageContent. The server's wire format
// only carries image attachments separately, so text rides inside the
// message body — which also means edit/resend and the persisted
// transcript reproduce exactly what the model saw.
//
// Files dropped from the OS expose no filesystem path (browsers hide
// it), and the editor pane opens files by workspace-relative path over
// its socket, so "open in the editor" is not offered for drops —
// context attachment is the one behavior that always works.

// Common text/code MIME prefixes (checked before the extension: an
// explicit text/* from the OS beats any guessing).
const TEXT_MIME_PREFIXES = [
    'text/',
    'application/json', 'application/xml', 'application/javascript',
    'application/x-yaml', 'application/yaml', 'application/toml',
    'application/x-sh', 'application/graphql',
];
// MIME types that say nothing about the content. The OS reports these for
// code files it has no association for (a Windows registry without a
// Content Type, a network share, a mail client, a stripped-down file
// manager), so they are treated exactly like an empty type and the
// extension table decides — which is what the picker's accept hint
// promises the user.
const GENERIC_MIME_TYPES = new Set([
    'application/octet-stream',
    'application/unknown',
    'binary/octet-stream',
]);
// Extensions accepted when the OS reports no MIME type. Includes
// extension-less dotfiles and bare names ("Makefile", ".gitignore").
const TEXT_EXTENSIONS = new Set([
    'go', 'js', 'jsx', 'ts', 'tsx', 'mjs', 'cjs', 'py', 'rb', 'rs', 'java',
    'kt', 'kts', 'swift', 'c', 'h', 'cpp', 'hpp', 'cc', 'cxx', 'cs', 'php',
    'sh', 'bash', 'zsh', 'fish', 'ps1', 'sql', 'json', 'jsonc', 'yaml',
    'yml', 'toml', 'xml', 'html', 'htm', 'css', 'scss', 'sass', 'less',
    'md', 'markdown', 'txt', 'text', 'csv', 'tsv', 'ini', 'cfg', 'conf',
    'env', 'properties', 'proto', 'graphql', 'gql', 'vue', 'svelte', 'astro',
    'dart', 'scala', 'pl', 'pm', 'lua', 'r', 'jl', 'ex', 'exs', 'erl', 'hrl',
    'clj', 'cljs', 'lisp', 'el', 'vim', 'dockerfile', 'makefile', 'cmake',
    'gradle', 'tf', 'tfvars', 'hcl', 'gitignore', 'gitattributes', 'editorconfig',
    'npmrc', 'babelrc', 'eslintrc', 'prettierrc', 'patch', 'diff', 'lock',
    'log', 'rst', 'adoc',
]);

// Extension of `name` — for extension-less files the whole lowercase
// basename, so "Makefile" → "makefile" and ".gitignore" → "gitignore".
function fileNameExtension(name) {
    const base = String(name || '').toLowerCase();
    const dot = base.lastIndexOf('.');
    return dot === -1 ? base : base.slice(dot + 1);
}

function isTextFile(file) {
    const type = String(file.type || '').toLowerCase();
    if (type && !GENERIC_MIME_TYPES.has(type)) {
        for (const prefix of TEXT_MIME_PREFIXES) {
            if (type.startsWith(prefix)) return true;
        }
        return false;
    }
    return TEXT_EXTENSIONS.has(fileNameExtension(file.name));
}

function addTextAttachment(file) {
    if (!file) return;
    // Only the text cap applies here: it is far tighter than
    // MAX_ATTACHMENT_BYTES (the image limit), so a 5 MB check would be dead
    // code with a misleading message.
    if (file.size > MAX_TEXT_ATTACHMENT_BYTES) {
        deps.showToast(
            `"${file.name}" is larger than 256 KB — text files are inlined into the message`,
            'error');
        return;
    }
    if (pendingAttachments.length >= MAX_ATTACHMENTS) {
        deps.showToast(`Max ${MAX_ATTACHMENTS} attachments per message`, 'error');
        return;
    }
    const reader = new FileReader();
    reader.onload = () => {
        pendingAttachments.push({ name: file.name || 'file', text: String(reader.result || '') });
        renderAttachmentPreview();
    };
    reader.onerror = () => deps.showToast(`Could not read "${file.name}"`, 'error');
    reader.readAsText(file);
}

function removeAttachment(index) {
    pendingAttachments.splice(index, 1);
    renderAttachmentPreview();
}

export function clearAttachments() {
    pendingAttachments = [];
    renderAttachmentPreview();
}

// Outgoing message content: the typed text with every text-file
// attachment inlined as a fenced code block (images ride separately in
// the payload's images array — see sendMessage in app.js). Called once
// per send; empty when there is nothing to inline.
export function composeMessageContent(text) {
    let content = text;
    for (const att of pendingAttachments) {
        if (att.text === undefined) continue;
        if (content) content += '\n\n';
        content += fencedFileBlock(att);
    }
    return content;
}

// One attachment as "[Attached file: name]" + a fenced block. The fence
// grows past the longest backtick run inside the file so the block can
// never terminate early (markdown's own rule for embedding fences).
function fencedFileBlock(att) {
    const body = String(att.text || '');
    let run = 0;
    let longest = 0;
    for (const ch of body) {
        if (ch === '`') {
            run++;
            if (run > longest) longest = run;
        } else {
            run = 0;
        }
    }
    const fence = '`'.repeat(Math.max(3, longest + 1));
    const dot = String(att.name || '').lastIndexOf('.');
    const ext = dot === -1 ? '' : att.name.slice(dot + 1).toLowerCase();
    const lang = /^[a-z0-9]+$/.test(ext) ? ext : '';
    return '[Attached file: ' + att.name + ']\n' + fence + lang + '\n' + body + '\n' + fence;
}

function renderAttachmentPreview() {
    attachmentPreview.replaceChildren();
    if (pendingAttachments.length === 0) {
        attachmentPreview.hidden = true;
        return;
    }
    attachmentPreview.hidden = false;
    for (let i = 0; i < pendingAttachments.length; i++) {
        const att = pendingAttachments[i];
        const chip = document.createElement('span');
        chip.className = 'attachment-chip';
        chip.title = att.name;
        if (att.text !== undefined) {
            // Text/code file: icon + name instead of the image thumbnail.
            chip.classList.add('text');
            const fileIcon = document.createElement('span');
            fileIcon.className = 'attachment-file-icon';
            fileIcon.innerHTML = icon('code');
            const name = document.createElement('span');
            name.className = 'attachment-file-name';
            name.textContent = att.name;
            chip.appendChild(fileIcon);
            chip.appendChild(name);
        } else {
            const img = document.createElement('img');
            img.src = att.dataUrl;
            img.alt = att.name;
            chip.appendChild(img);
        }
        const remove = document.createElement('button');
        remove.type = 'button';
        remove.className = 'attachment-remove';
        remove.innerHTML = icon('x');
        remove.title = att.text !== undefined ? 'Remove file' : 'Remove image';
        remove.setAttribute('aria-label', remove.title);
        remove.addEventListener('click', () => removeAttachment(i));
        chip.appendChild(remove);
        attachmentPreview.appendChild(chip);
    }
}

// The picker's accept hint, derived from the same tables the routing below
// uses: images first (the dialog's default filter follows the first entry,
// and a screenshot is still the common case), then every text/code MIME and
// extension. It is a hint, not the contract — the routing decides, and
// isTextFile also accepts a generic OS type by extension, so a code file the
// OS mislabels as application/octet-stream is attached, not refused.
// Extension-less names ("Makefile") match neither arm, so the dialog's "all
// files" escape hatch (or a drop) still routes them correctly through
// addFiles.
imageUpload.accept = ['image/*']
    .concat(TEXT_MIME_PREFIXES.map((t) => (t.endsWith('/') ? t + '*' : t)))
    .concat([...TEXT_EXTENSIONS].map((ext) => '.' + ext))
    .join(',');

attachBtn.addEventListener('click', () => {
    imageUpload.click();
});
imageUpload.addEventListener('change', () => {
    // Same router as a drop (addFiles): a pick that is not attachable is
    // rejected with a toast instead of being dropped silently — the old
    // image-only handler made picking a text document look like a broken
    // upload (no chip, no error).
    addFiles(imageUpload.files || []);
    imageUpload.value = '';
});
// Paste an image or a text/code file straight into the composer; text
// pastes behave exactly as before (they arrive as kind === 'string').
//
// The paste is claimed only when at least one item will actually be
// attached: a clipboard that carries both text and a refused file (a PDF
// copied out of a mail client, an attachment from a file manager) must keep
// its native text paste instead of turning into a rejection toast. Files
// that share the paste with an attachable one still get their toast —
// addFiles reports every item it cannot use.
inputArea.addEventListener('paste', (e) => {
    const items = (e.clipboardData && e.clipboardData.items) || [];
    const files = [];
    for (const item of items) {
        if (item.kind !== 'file') continue;
        const f = item.getAsFile();
        if (f) files.push(f);
    }
    if (!files.some(isAttachable)) return;
    e.preventDefault();
    addFiles(files);
});

// ── Drag-and-drop attachments ──
// Dropping files anywhere on the composer row (#input-area) attaches
// them through the same addFiles router as the picker and paste, so all
// three entry points share the limits and the rejections. Only drags
// that actually carry files are claimed — dragging a text selection onto
// the textarea keeps the browser's native insert-on-drop.

// `types` includes the literal "Files" token whenever the drag carries
// file system items (OS file manager, attachments from other apps).
function dragHasFiles(e) {
    const types = e.dataTransfer && e.dataTransfer.types;
    if (!types) return false;
    for (const type of types) {
        if (type === 'Files') return true;
    }
    return false;
}

// isAttachable reports whether addFiles has a path for `file`. The per-kind
// size caps are NOT part of it: those report their own toast, and a paste of
// one oversized image is still the user asking to attach an image (claiming
// it is what the pre-router handler did).
function isAttachable(file) {
    if (!file) return false;
    if (file.type && file.type.startsWith('image/')) return true;
    return isTextFile(file);
}

// One file list → the right attachment path, shared by the picker, the
// paste handler and the drop zone so all three accept the same kinds and
// report the same rejections.
function addFiles(files) {
    for (const file of files) {
        if (!file) continue;
        if (file.type && file.type.startsWith('image/')) {
            addImageAttachment(file);
        } else if (isTextFile(file)) {
            addTextAttachment(file);
        } else {
            deps.showToast(unsupportedAttachmentMessage(file), 'error');
        }
    }
}

// Why `file` cannot be attached. Audio gets its own wording: the wire
// format carries images only (llm.Message.Images), text rides inlined in
// the message body, and there is no audio part at any layer yet — so the
// hint points at the workaround instead of implying a retry would help.
function unsupportedAttachmentMessage(file) {
    const name = file.name || 'file';
    if (String(file.type || '').startsWith('audio/')) {
        return `"${name}" was not attached — audio input is not supported yet (attach a transcript instead)`;
    }
    return `"${name}" was not attached — only images and text/code files are supported`;
}

// Overlay visibility via an enter/leave depth counter: the events bubble
// from every child of #input-area, and dragenter on the new target fires
// before dragleave on the old one, so the depth never dips to 0 while
// the pointer moves between children.
let dragDepth = 0;

function resetDragState() {
    dragDepth = 0;
    dropOverlay.hidden = true;
}

composerEl.addEventListener('dragenter', (e) => {
    if (!dragHasFiles(e)) return;
    e.preventDefault();
    dragDepth++;
    dropOverlay.hidden = false;
});
composerEl.addEventListener('dragleave', () => {
    if (dragDepth > 0) dragDepth--;
    if (dragDepth === 0) dropOverlay.hidden = true;
});
// Accepting the drop (preventDefault on dragover) is also what makes the
// drop event fire at all; copy fits "attach a duplicate of this file".
composerEl.addEventListener('dragover', (e) => {
    if (!dragHasFiles(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
});
composerEl.addEventListener('drop', (e) => {
    if (!dragHasFiles(e)) return; // text drops fall through to the textarea
    e.preventDefault();
    resetDragState();
    addFiles(e.dataTransfer.files || []);
});

// Safety net: a file drop that misses the composer must not trigger the
// browser default — navigating this tab away to the raw file. The
// composer's and board's own handlers run first (target phase) and are
// unaffected; this only cancels truly unhandled file drops. Board card
// drags set only text/plain, so they never match dragHasFiles.
window.addEventListener('dragover', (e) => {
    if (dragHasFiles(e)) e.preventDefault();
});
window.addEventListener('drop', (e) => {
    if (dragHasFiles(e)) e.preventDefault();
});
