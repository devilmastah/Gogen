'use strict';
// Regression test: the composer's three attachment entry points — the
// paperclip picker, clipboard paste, and the drop zone — must route files
// through the SAME path (composer.js addFiles) and never fail silently.
//
// Pre-fix the picker was image-only twice over: `accept="image/*"` hid text
// files from the OS dialog, and its "change" handler called
// addImageAttachment directly, whose `!file.type.startsWith('image/')` guard
// returned without a chip or a toast. Picking a text document therefore did
// nothing at all — it looked like the upload was broken. Text/code files
// only worked when DRAGGED onto the composer.
//
// Covered here:
//   - picker: text document attaches (chip + inlined into the sent content)
//   - picker: image still attaches and rides the payload's images array
//   - picker: unsupported (docx) and audio files are rejected WITH a toast
//   - picker accept hint advertises images and text/code, opens on images,
//     and is derived from the routing tables (no drift)
//   - a code file the OS reports as application/octet-stream attaches by
//     extension (the accept hint offers .go, so the sniff must accept it)
//   - paste: a text file item attaches (images-only before) and claims the
//     event; a refused file alone does NOT claim it (native text paste)
//   - drop: text still attaches (no regression on the working path)
//
// Run: node scripts/test_composer_attachments.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(path.join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');

let failures = 0;
const check = (desc, ok) => {
  console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
  if (!ok) failures++;
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function waitFor(desc, cond, timeoutMs = 2000) {
  const t0 = Date.now();
  while (Date.now() - t0 < timeoutMs) {
    if (cond()) return true;
    await sleep(20);
  }
  console.log(`   (timeout waiting for: ${desc})`);
  return false;
}

function boot() {
  const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');
  const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;
  window.matchMedia = window.matchMedia || (() => ({ matches: false, addEventListener() {}, removeEventListener() {} }));
  window.navigator.clipboard = { writeText: async () => {} };
  if (!('innerText' in window.HTMLElement.prototype)) {
    Object.defineProperty(window.HTMLElement.prototype, 'innerText', {
      get() { return this.textContent; }, set(v) { this.textContent = v; }, configurable: true,
    });
  }
  window.Terminal = class {
    constructor() { this._cbs = {}; }
    open() {} write() {} fit() {}
    onData(cb) { this._cbs.data = cb; }
    onResize() { return { dispose() {} }; }
    dispose() {} reset() {} focus() {}
  };
  window.FitAddon = class { fit() {} };
  const sockets = [];
  class FakeWS {
    constructor() { this.readyState = 0; this.sent = []; sockets.push(this); }
    static OPEN = 1;
    static CONNECTING = 0;
    send(msg) { this.sent.push(JSON.parse(msg)); }
    close() { this.readyState = 3; }
  }
  window.WebSocket = FakeWS;
  installEditorStubs(window);
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };
  window.localStorage.setItem('gogen_notifications', 'off');
  window.addEventListener('error', (e) => {
    console.error('WINDOW ERROR:', (e.error && e.error.stack) || e.message);
    failures++;
  });
  loadAppJs(window);
  const ws = sockets[0];
  if (!ws) throw new Error('app.js did not construct a WebSocket');
  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  return {
    window, ws,
    recv: (obj) => ws.onmessage({ data: JSON.stringify(obj) }),
    sentOf: (type) => ws.sent.filter((m) => m.type === type),
    $: (id) => window.document.getElementById(id),
    chips: () => [...window.document.querySelectorAll('#attachment-preview .attachment-chip')]
      .map((c) => (c.classList.contains('text')
        ? c.querySelector('.attachment-file-name').textContent
        : c.querySelector('img').alt)),
    toasts: () => [...window.document.querySelectorAll('#toast-host .toast')]
      .map((t) => `${t.dataset.kind}: ${t.dataset.message}`),
    file: (body, name, type) => new window.File([body], name, { type }),
  };
}

const configFrame = (sid) => ({
  type: 'config', sessionId: sid, sessionLabel: sid + '-label',
  mode: 'act', thinkingLevel: 'off', reasoningEfforts: [],
  model: 'm1', modelDescription: '', workingDir: '/tmp', globalMode: false,
});

// The picker: set `files` and fire "change" (what the browser does after
// the OS dialog closes). jsdom has no DataTransfer, so the drop zone is
// driven by an Event with a hand-built dataTransfer.
function pickFile(env, file) {
  const input = env.$('image-upload');
  Object.defineProperty(input, 'files', { value: [file], configurable: true });
  input.dispatchEvent(new env.window.Event('change', { bubbles: true }));
}
function dropFile(env, file) {
  const ev = new env.window.Event('drop', { bubbles: true, cancelable: true });
  Object.defineProperty(ev, 'dataTransfer', { value: { types: ['Files'], files: [file], dropEffect: '' } });
  env.$('input-area').dispatchEvent(ev);
}
// Returns the event so a caller can assert whether the paste was claimed
// (defaultPrevented) — a refused file must leave the native text paste alone.
function pasteFile(env, file) {
  const ev = new env.window.Event('paste', { bubbles: true, cancelable: true });
  Object.defineProperty(ev, 'clipboardData', {
    value: { items: [{ kind: 'file', type: file.type, getAsFile: () => file }] },
  });
  env.$('message-input').dispatchEvent(ev);
  return ev;
}
function clearChips(env) {
  for (const btn of [...env.window.document.querySelectorAll('#attachment-preview .attachment-remove')]) btn.click();
}

async function main() {
  const env = boot();
  env.recv(configFrame('A'));

  // ── the picker's accept hint comes from the routing tables ──
  const accept = env.$('image-upload').accept;
  check('picker accept advertises images', accept.includes('image/*'));
  check('picker accept advertises text MIME types', accept.includes('text/*'));
  check('picker accept advertises code extensions', accept.includes('.go') && accept.includes('.md'));
  // The dialog's default filter follows the first accept entry: a screenshot
  // is still the primary use, so images must not be pushed behind text/*.
  check('picker accept opens on images', accept.startsWith('image/*'));
  check('attach button no longer claims image-only', !/^Attach image$/i.test(env.$('attach-btn').getAttribute('aria-label')));

  // ── picker: a text document attaches and is inlined on send ──
  pickFile(env, env.file('alpha\nbeta\n', 'notes.txt', 'text/plain'));
  await waitFor('text chip', () => env.chips().length === 1);
  check('picked text document shows a chip', env.chips()[0] === 'notes.txt');
  check('picked text document raises no error toast', env.toasts().length === 0);

  env.$('message-input').value = 'summarize this';
  env.$('send-btn').click();
  await sleep(30);
  const sent = env.sentOf('message');
  check('send produced one message frame', sent.length === 1);
  check('picked text document is inlined into the content',
    !!sent[0] && sent[0].content.includes('[Attached file: notes.txt]') && sent[0].content.includes('alpha\nbeta'));
  check('a text-only attachment adds no images field', !!sent[0] && sent[0].images === undefined);
  check('preview reset after send', env.chips().length === 0);

  // ── picker: images take the unchanged path ──
  pickFile(env, env.file(new Uint8Array([137, 80, 78, 71]), 'shot.png', 'image/png'));
  await waitFor('image chip', () => env.chips().length === 1);
  check('picked image still attaches', env.chips()[0] === 'shot.png');
  env.$('message-input').value = 'what is this';
  env.$('send-btn').click();
  await sleep(30);
  const withImage = env.sentOf('message')[1];
  check('image rides the payload images array',
    !!withImage && Array.isArray(withImage.images) && withImage.images.length === 1
    && String(withImage.images[0].dataUrl).startsWith('data:image/png'));
  check('image content stays the typed text', !!withImage && withImage.content === 'what is this');

  // ── picker: unsupported kinds are rejected OUT LOUD ──
  clearChips(env);
  const toastCount = env.toasts().length;
  pickFile(env, env.file('PK\u0003\u0004', 'report.docx',
    'application/vnd.openxmlformats-officedocument.wordprocessingml.document'));
  await sleep(60);
  check('unsupported document attaches nothing', env.chips().length === 0);
  check('unsupported document raises an error toast', env.toasts().length === toastCount + 1
    && env.toasts()[toastCount].includes('report.docx'));

  pickFile(env, env.file('RIFFxxxx', 'clip.mp3', 'audio/mpeg'));
  await sleep(60);
  check('audio attaches nothing', env.chips().length === 0);
  const last = env.toasts()[env.toasts().length - 1] || '';
  check('audio rejection names audio explicitly',
    last.startsWith('error: ') && last.includes('clip.mp3') && /audio/i.test(last));

  // ── a code file the OS mislabels as a generic type still attaches ──
  // application/octet-stream says nothing about the content (Windows
  // registry miss, network share, mail client), so the extension decides —
  // otherwise the picker would offer .go and the router would refuse it.
  clearChips(env);
  const genericToasts = env.toasts().length;
  pickFile(env, env.file('package main\n', 'main.go', 'application/octet-stream'));
  await waitFor('generic-type chip', () => env.chips().length === 1);
  check('generic application/octet-stream routes by extension', env.chips()[0] === 'main.go');
  check('generic type attaches without an error toast', env.toasts().length === genericToasts);

  // ── a refused file alone must not swallow the paste ──
  // The old handler claimed the paste for any image; claiming it for a file
  // we then refuse would eat the text a mixed clipboard also carries.
  clearChips(env);
  const refusedToasts = env.toasts().length;
  const refusedPaste = pasteFile(env, env.file('PK\u0003\u0004', 'report.docx',
    'application/vnd.openxmlformats-officedocument.wordprocessingml.document'));
  await sleep(40);
  check('a refused paste leaves the event to the browser', !refusedPaste.defaultPrevented);
  check('a refused paste attaches nothing and stays quiet',
    env.chips().length === 0 && env.toasts().length === refusedToasts);

  // ── paste: a text file item attaches (images-only before) ──
  clearChips(env);
  const textPaste = pasteFile(env, env.file('# pasted\n', 'pasted.md', 'text/markdown'));
  await waitFor('pasted chip', () => env.chips().length === 1);
  check('pasted text file attaches', env.chips()[0] === 'pasted.md');
  check('pasted text file claims the paste', textPaste.defaultPrevented);

  // ── drop: the path that always worked keeps working ──
  clearChips(env);
  dropFile(env, env.file('package main\n', 'main.go', 'text/x-go'));
  await waitFor('dropped chip', () => env.chips().length === 1);
  check('dropped code file still attaches', env.chips()[0] === 'main.go');

  // ── a fenced text document cannot break out of its block ──
  clearChips(env);
  dropFile(env, env.file('before\n```\ninner\n```\nafter\n', 'fences.md', 'text/markdown'));
  await waitFor('fence chip', () => env.chips().length === 1);
  env.$('message-input').value = 'review';
  env.$('send-btn').click();
  await sleep(30);
  const fenced = env.sentOf('message').pop();
  check('embedded fences grow the outer fence',
    !!fenced && fenced.content.includes('````md') && fenced.content.includes('```\ninner\n```')
    && fenced.content.includes('fences.md'));

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
