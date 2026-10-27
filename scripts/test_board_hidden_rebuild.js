'use strict';
// Regression: board_state broadcasts must not rebuild the board DOM while
// the board pane is hidden. The server re-broadcasts a full board_state on
// every mutation (agent board-tool calls included), so before the fix every
// broadcast wiped #board-columns and rebuilt every column and card — even
// in tabs where the board pane is display:none. The fix gates the rebuild
// on pane visibility (boardPaneVisible in components/board.js): hidden
// panes keep lastBoardState fresh and renderBoard() runs on pane switch
// (app.js switchMainPane).
//
// Loads the real index.html + app.js into jsdom (imports stripped, editor/
// marked/dompurify stubbed) with a controllable WebSocket stub, then:
//   1. enables the board feature (config push) — board pane NOT active
//   2. delivers a board_state while hidden → no DOM rebuild
//   3. switches to the board pane → renders from the stored state
//   4. delivers a board_state while visible → rebuilds
//   5. hides the pane again, delivers another board_state → DOM stays
//      stale, the state is stored, and switching back paints it
//
// Run: node scripts/test_board_hidden_rebuild.js
// APPJS env var selects the app.js copy to test (defaults to the real one).
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');

async function main() {
  const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');
  const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;
  const { document } = window;

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

  // Controllable WebSocket stub: app.js's connect() constructs it; the
  // instance is captured so the test can drive onopen/onmessage and read
  // the messages the app sends.
  const sockets = [];
  class FakeWS {
    constructor() {
      this.readyState = 0;
      this.sent = [];
      sockets.push(this);
    }
    static OPEN = 1;
    static CONNECTING = 0;
    send(msg) { this.sent.push(JSON.parse(msg)); }
    close() { this.readyState = 3; }
  }
  window.WebSocket = FakeWS;

  installEditorStubs(window);
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
  const evalJs = (code) => window.eval(code);
  const colsDiv = () => document.getElementById('board-columns');
  const cardCount = () => colsDiv().querySelectorAll('.board-card').length;
  const columnCount = () => colsDiv().querySelectorAll('.board-column').length;
  const boardActive = () => document.getElementById('board-pane').classList.contains('active');
  const boardState = (n) => ({
    type: 'board_state',
    boardState: {
      columns: ['backlog', 'ready', 'in_progress', 'in_review', 'blocked', 'done'],
      items: Array.from({ length: n }, (_, i) => ({
        id: String(i + 1),
        title: `Ticket ${i + 1}`,
        status: 'backlog',
      })),
    },
  });

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };

  // 1. Connect + enable the board feature (the tab becomes visible; the
  // board pane is NOT the active main pane — chat is).
  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  window.applyServerConfig({
    board: 'on', subagent: 'off', subagentMaxDepth: 1,
    subagentModel: '', subagentThinkingLevel: '',
    reasoningEfforts: [], model: 'm1',
  });
  check('board tab visible after config push', !document.getElementById('board-tab').hidden);
  check('board pane starts hidden (chat is the active pane)', !boardActive());

  // 2. Broadcast while the board pane is hidden: state stored, NO rebuild.
  recv(boardState(1));
  check('hidden board pane: no columns rebuilt', columnCount() === 0 && cardCount() === 0);

  // 3. Switch to the board pane: renders from the stored lastBoardState.
  evalJs("switchMainPane('board')");
  check('board pane active after switch', boardActive());
  check('switch-to-board renders the stored state (6 columns, 1 card)',
    columnCount() === 6 && cardCount() === 1);
  check('rendered card carries the stored title',
    Array.from(colsDiv().querySelectorAll('.board-card')).some((c) => (c.textContent || '').includes('Ticket 1')));

  // 4. Broadcast while the board pane is visible: rebuilds as before.
  recv(boardState(2));
  check('visible board pane: broadcast rebuilds (2 cards)', cardCount() === 2);

  // 5. Hide the pane, broadcast again: DOM stays stale, state is stored,
  // and switching back paints the newer state.
  evalJs("switchMainPane('chat')");
  recv(boardState(3));
  check('hidden again: broadcast does not rebuild (still 2 cards)', cardCount() === 2);
  evalJs("switchMainPane('board')");
  check('switch back renders the newer stored state (3 cards)', cardCount() === 3);

  if (failures === 0) {
    console.log('\nAll board hidden-rebuild checks passed.');
    process.exit(0);
  }
  console.error(`\n${failures} check(s) failed.`);
  process.exit(1);
}

main().catch((e) => { console.error(e); process.exit(1); });
