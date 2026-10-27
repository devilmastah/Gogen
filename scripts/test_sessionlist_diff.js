'use strict';
// Regression test for the KEYED DIFF render of the sidebar session list
// (components/sessions.js renderSessionList): the list is no longer wiped
// and rebuilt on every refresh — rows are keyed, left untouched while
// their signature is unchanged, moved on reorder, removed when they leave
// the list.
//
// The specific failure modes guarded here:
//   1. identical re-render → row elements are the SAME nodes (no rebuild),
//      and the 30s relative-time refresher's in-place textContent updates
//      survive the re-render
//   2. only the row whose state actually changed is rebuilt
//   3. a parent that REORDERS carries its nested (subagent) children with
//      it (the old append-to-container-end design orphaned children at the
//      list bottom on a moved parent)
//   4. rows that leave the list are removed from the DOM
//   5. id-less "creating…" panes get stable per-pane keys (two coexist)
//   6. findSessionRow / updateCurrentSessionLabel still work against the
//      diffed rows (in-place title patch keeps the same node)
//
// Loads the real index.html + app.js + components into jsdom (imports
// stripped, editor/marked/dompurify stubbed) with a controllable
// WebSocket stub.
//
// Run: node scripts/test_sessionlist_diff.js
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

  const evalJs = (code) => window.eval(code);
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

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };

  const rowOf = (id) => window.document.querySelector(`.session-row[data-session-id="${id}"]`);
  const rowOrder = () => [...window.document.querySelectorAll('#session-list .session-row')]
    .map((r) => r.dataset.sessionId);
  const ago = (ms) => new Date(Date.now() - ms).toISOString();
  // Pin a pane's output-recency stamp: open panes sort by
  // max(entry.updatedAt, pane.lastActivity), and makePane/turn_end stamp
  // lastActivity with Date.now() — pinning keeps the order assertions
  // deterministic (a real output event would stamp it the same way).
  const setActivity = (id, iso) => { evalJs(`findPaneBySession('${id}').lastActivity = Date.parse('${iso}')`); };

  // Adopt parent session A (the startup default pane has no id yet).
  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  recv({
    type: 'config', sessionId: 'A', sessionLabel: 'session A',
    mode: 'act', thinkingLevel: 'off', reasoningEfforts: [], model: 'm1',
    modelDescription: '', workingDir: '/tmp', globalMode: false,
  });
  check('parent pane A adopted', evalJs('activePane().id') === 'A');

  // Two saved sessions with distinct last-output stamps: B is newer.
  const tA = ago(600000); // 10m ago
  const tB = ago(120000); // 2m ago
  setActivity('A', tA); // override the startup Date.now() stamp
  const payloadAB = () => [
    { id: 'A', label: 'session A', messageCount: 2, updatedAt: tA },
    { id: 'B', label: 'session B', messageCount: 5, updatedAt: tB },
  ];
  recv({ type: 'sessions', sessions: payloadAB() });
  check('initial order is newest-output first (B, A)',
    JSON.stringify(rowOrder()) === JSON.stringify(['B', 'A']));

  // 1. Identical re-render: the row elements are the SAME nodes — no
  //    rebuild — and a 30s-refresher-style in-place time update survives.
  const rowA1 = rowOf('A');
  const rowB1 = rowOf('B');
  check('rows render with dataset.sessionId', !!rowA1 && !!rowB1);
  const timeB = rowB1 && rowB1.querySelector('.session-row-time');
  check('saved row B carries a .session-row-time node', !!timeB);
  if (timeB) timeB.textContent = 'REFRESHED-BY-30s-TICK';
  recv({ type: 'sessions', sessions: payloadAB() }); // identical payload
  check('identical re-render keeps row A the SAME node', rowOf('A') === rowA1);
  check('identical re-render keeps row B the SAME node', rowOf('B') === rowB1);
  const timeB2 = rowOf('B') && rowOf('B').querySelector('.session-row-time');
  check('30s refresher in-place update survives the re-render',
    !!timeB2 && timeB2.textContent === 'REFRESHED-BY-30s-TICK');

  // 2. Only the row whose state actually changed is rebuilt: A's turn
  //    starts (session_state) → A's row is a new node, B's is untouched.
  recv({ type: 'session_state', sessionId: 'A', turnActive: true });
  const rowA2 = rowOf('A');
  check('turn start rebuilds row A (new node)', rowA2 !== rowA1);
  check('turn start leaves row B the SAME node', rowOf('B') === rowB1);
  check('row A shows the amber responding state',
    !!rowA2.querySelector('.session-state-dot.amber'));
  recv({ type: 'turn_end', sessionId: 'A', content: 'done' });
  const rowA3 = rowOf('A');
  check('turn end rebuilds row A again (new node)', rowA3 !== rowA2);
  check('turn end still leaves row B the SAME node', rowOf('B') === rowB1);
  // turn_end is an output event: it stamped A's recency with "now". Pin it
  // back so the reorder assertions below stay deterministic.
  setActivity('A', tA);

  // 3. Nested children follow their parent when it REORDERS. C is a child
  //    of A; B is newest, so the order is B, A, C. Then A gets newer
  //    output: the order must become A, C, B — C stays under A (the old
  //    append-to-end design would have stranded C at the list bottom).
  recv({
    type: 'subagent_started', subagentId: 'C', subagentParent: 'A',
    subagentLabel: 'subagent: fix parser', subagentJob: 'fix parser',
  });
  const tA2 = ago(30000); // 30s ago
  recv({
    type: 'sessions',
    sessions: [
      { id: 'A', label: 'session A', messageCount: 3, updatedAt: tA },
      { id: 'B', label: 'session B', messageCount: 5, updatedAt: tB },
      { id: 'C', parentId: 'A', label: 'subagent: fix parser', messageCount: 1, active: true, updatedAt: tA2 },
    ],
  });
  check('child C renders nested (no flat row), order B, A, C',
    JSON.stringify(rowOrder()) === JSON.stringify(['B', 'A', 'C']));
  const rowC1 = rowOf('C');
  check('child C row exists', !!rowC1 && rowC1.classList.contains('nested'));
  const tA3 = ago(5000); // 5s ago — A (and its output) is newest again
  setActivity('A', tA3);
  recv({
    type: 'sessions',
    sessions: [
      { id: 'A', label: 'session A', messageCount: 4, updatedAt: tA3 },
      { id: 'B', label: 'session B', messageCount: 5, updatedAt: tB },
      { id: 'C', parentId: 'A', label: 'subagent: fix parser', messageCount: 1, active: true, updatedAt: tA2 },
    ],
  });
  check('reordered parent carries its child: order A, C, B',
    JSON.stringify(rowOrder()) === JSON.stringify(['A', 'C', 'B']));
  check('child C kept the SAME node across the parent reorder', rowOf('C') === rowC1);

  // 4. Rows that leave the list are removed from the DOM. B drops out of
  //    the payload; C stays (its live subagent_started record is still
  //    registered — the live source wins over the payload).
  recv({
    type: 'sessions',
    sessions: [
      { id: 'A', label: 'session A', messageCount: 4, updatedAt: tA3 },
    ],
  });
  check('row B removed from the DOM when it leaves the list', rowOf('B') === null);
  check('child C still renders from its live record', rowOf('C') !== null);
  check('no orphaned rows: exactly A + C remain',
    JSON.stringify(rowOrder()) === JSON.stringify(['A', 'C']));

  // 5. Id-less "creating…" panes get stable per-pane keys (the startup
  //    pane is key 0, so these are 1 and 2): two coexist as two distinct
  //    rows, and closing each removes exactly its row.
  evalJs('makePane()');
  evalJs('makePane()');
  const creatingCount = () => [...window.document.querySelectorAll('#session-list .session-row')]
    .filter((r) => (r.textContent || '').includes('creating…')).length;
  check('two id-less panes render two distinct creating… rows', creatingCount() === 2);
  evalJs('closePane(1)');
  check('closing one id-less pane removes exactly its row', creatingCount() === 1);
  evalJs('closePane(2)');
  check('closing the last id-less pane removes its row', creatingCount() === 0);

  // 6. findSessionRow / updateCurrentSessionLabel work against the diffed
  //    rows: the title patch is in-place and keeps the same node.
  const found = evalJs("findSessionRow('A')");
  check('findSessionRow finds row A by dataset.sessionId',
    !!found && found.dataset.sessionId === 'A');
  const rowA4 = rowOf('A');
  evalJs("updateCurrentSessionLabel('renamed A')");
  check('updateCurrentSessionLabel patches the title in place (same node)',
    rowOf('A') === rowA4
    && (rowOf('A').querySelector('.session-row-title').textContent || '') === 'renamed A');

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
