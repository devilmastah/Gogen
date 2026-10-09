'use strict';
// Regression test for clickable tool-card URLs:
//   - a web_fetch (or download_file) tool card renders its `url` arg as a
//     real <a class="url-link"> that opens in a new tab (target="_blank",
//     rel="noopener noreferrer") — on BOTH the live streaming path
//     (tool_call_start/delta/tool_call) and the history-replay path;
//   - a non-http(s) `url` value (e.g. a javascript: scheme from a hostile
//     or buggy model) must render as plain text, never an executable href;
//   - clicking the URL must NOT toggle the card's collapse (the header
//     click does), and file-path links keep working.
//
// Loads the real index.html + app.js into jsdom (imports stripped,
// editor/marked/dompurify stubbed) with a controllable WebSocket stub.
// Run: node scripts/test_webfetch_url_link.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('/tmp/gogen-jsdom/node_modules/jsdom');
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function main() {
  const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');
  const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;

  window.matchMedia = window.matchMedia || (() => ({ matches: false, addEventListener() {}, removeEventListener() {} }));
  window.Notification = { permission: 'denied', requestPermission: () => Promise.resolve('denied') };
  window.navigator.clipboard = { writeText: async () => {} };
  class FakeTerminal {
    constructor() { this._cbs = {}; }
    open() {} write() {} fit() {}
    onData(cb) { this._cbs.data = cb; }
    onResize() { return { dispose() {} }; }
    dispose() {} reset() {} focus() {}
  }
  window.Terminal = FakeTerminal;
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
  window.escapeHtml = window.escapeHtml || ((s) => String(s == null ? '' : s));
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };

  loadAppJs(window);

  const ws = sockets[0];
  if (!ws) throw new Error('app.js did not construct a WebSocket');
  window.addEventListener('error', (e) => { console.error('WINDOW ERROR:', e.error && e.error.stack || e.message); });
  const recv = (obj) => {
    try {
      ws.onmessage({ data: JSON.stringify(obj) });
    } catch (err) {
      console.error('RECV ERROR:', err && err.stack || err);
      throw err;
    }
  };
  const doc = window.document;
  const messagesDiv = doc.getElementById('messages');
  const cards = () => [...messagesDiv.querySelectorAll('.tool-card')];
  const click = (el) => el.dispatchEvent(new window.MouseEvent('click', { bubbles: true, cancelable: true }));

  let failures = 0;
  const check = (desc, ok, extra) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}${ok || !extra ? '' : ' — ' + extra}`);
    if (!ok) failures++;
  };

  // Adopt session A (idle) with one message.
  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  recv({
    type: 'config', sessionId: 'A', sessionLabel: 'A-label',
    mode: 'act', thinkingLevel: 'off', reasoningEfforts: [], model: 'm1',
    modelDescription: '', workingDir: '/tmp', globalMode: false,
  });
  recv({
    type: 'history', sessionId: 'A',
    history: [
      { role: 'user', content: 'question one', index: 0, createdAt: '2024-01-01T00:00:00Z' },
    ],
  });
  await sleep(200);

  // ── Live streaming path: three tool calls in one round. ──
  const streamCall = (index, id, tool, args) => {
    const raw = JSON.stringify(args);
    recv({ type: 'tool_call_start', sessionId: 'A', index, tool });
    recv({ type: 'tool_call_delta', sessionId: 'A', index, toolCallId: id, tool, argsDelta: raw, argsPos: raw.length });
    recv({ type: 'tool_call', sessionId: 'A', index, toolCallId: id, tool, args });
    recv({ type: 'tool_result', sessionId: 'A', toolCallId: id, tool, result: 'ok', success: true });
  };

  streamCall(0, 't1', 'web_fetch', { url: 'https://example.com/article?x=1' });
  streamCall(1, 't2', 'web_fetch', { url: 'javascript:alert(1)' });
  streamCall(2, 't3', 'download_file', { url: 'https://files.example.com/a.txt', path: 'a.txt' });
  await sleep(50); // let the rAF compact pass (if any) settle

  check('three live tool cards rendered', cards().length === 3, `got ${cards().length}`);

  const [fetchCard, badCard, dlCard] = cards();
  const fetchLink = fetchCard && fetchCard.querySelector('.tool-args a.url-link');
  check('web_fetch url renders as a url-link anchor', !!fetchLink,
    fetchCard && fetchCard.querySelector('.tool-call-header').textContent.slice(0, 120));
  check('url-link href is the fetched URL', !!fetchLink && fetchLink.getAttribute('href') === 'https://example.com/article?x=1',
    fetchLink && fetchLink.getAttribute('href'));
  check('url-link opens in a new tab', !!fetchLink && fetchLink.target === '_blank',
    fetchLink && `target=${fetchLink.target}`);
  const rel = (el) => (el && el.getAttribute('rel')) || '';
  check('url-link rel carries noopener (no reverse tabnabbing)',
    !!fetchLink && rel(fetchLink).split(/\s+/).includes('noopener') && rel(fetchLink).split(/\s+/).includes('noreferrer'),
    `rel=${rel(fetchLink)}`);
  check('url-link text shows the URL', !!fetchLink && fetchLink.textContent === 'https://example.com/article?x=1');

  check('non-http url value is NOT an anchor (plain text only)',
    !!badCard && badCard.querySelector('.tool-args a') === null
    && /javascript:alert\(1\)/.test(badCard.querySelector('.tool-args').textContent),
    badCard && badCard.querySelector('.tool-call-header').textContent.slice(0, 120));

  const dlLink = dlCard && dlCard.querySelector('.tool-args a.url-link');
  const dlFileLink = dlCard && dlCard.querySelector('.tool-args .file-link');
  check('download_file url is clickable too', !!dlLink && dlLink.getAttribute('href') === 'https://files.example.com/a.txt');
  check('download_file path stays a file link', !!dlFileLink && dlFileLink.textContent === 'a.txt');

  // ── Click behavior: the URL must not toggle the card collapse. ──
  const header = fetchCard.querySelector('.tool-call-header');
  const toggle = fetchCard.querySelector('.tool-toggle');
  check('card starts expanded', !!toggle && !toggle.classList.contains('collapsed'));
  click(fetchLink);
  await sleep(20);
  check('clicking the url-link does NOT collapse the card',
    !!toggle && !toggle.classList.contains('collapsed'));
  click(header);
  await sleep(20);
  check('clicking the header DOES collapse the card',
    !!toggle && toggle.classList.contains('collapsed'));

  // ── History replay path: a restored web_fetch card gets the same link. ──
  recv({
    type: 'history', sessionId: 'A',
    history: [
      { role: 'user', content: 'fetch that page', index: 0, createdAt: '2024-01-01T00:00:00Z' },
      {
        role: 'assistant', content: '', index: 1, createdAt: '2024-01-01T00:00:01Z',
        toolCalls: [
          { id: 'h1', name: 'web_fetch', args: { url: 'https://replay.example.com/doc' } },
          { id: 'h2', name: 'read_file', args: { path: 'internal/server/web/app.js' } },
        ],
      },
      { role: 'tool', toolCallId: 'h1', content: 'page body', index: 2, createdAt: '2024-01-01T00:00:02Z' },
      { role: 'tool', toolCallId: 'h2', content: 'file body', index: 3, createdAt: '2024-01-01T00:00:03Z' },
    ],
  });
  await sleep(300);

  const replayCards = cards();
  check('replay rebuilt the pane with two tool cards', replayCards.length === 2, `got ${replayCards.length}`);
  const replayFetchLink = replayCards[0] && replayCards[0].querySelector('.tool-args a.url-link');
  check('replayed web_fetch url is a new-tab link',
    !!replayFetchLink
    && replayFetchLink.getAttribute('href') === 'https://replay.example.com/doc'
    && replayFetchLink.target === '_blank'
    && rel(replayFetchLink).split(/\s+/).includes('noopener'),
    replayCards[0] && replayCards[0].querySelector('.tool-call-header').textContent.slice(0, 120));
  const replayFileLink = replayCards[1] && replayCards[1].querySelector('.tool-args .file-link');
  check('replayed read_file path is still a file link',
    !!replayFileLink && replayFileLink.textContent === 'internal/server/web/app.js');

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
