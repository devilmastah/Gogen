'use strict';
// Regression test for the nested (subagent) sidebar mechanics: a child row
// gets the same ✕ actions as a normal session row — close when the child
// is open as a pane (session_close, stays saved, returns to a nested row),
// delete when it is closed (session_delete behind the confirm modal) — and
// a deleted child stops rendering under its parent.
//
// Loads the real index.html + app.js into jsdom (imports stripped, editor/
// marked/dompurify stubbed) with a controllable WebSocket stub, then:
//   1. adopts a parent session A, receives subagent_started for child C
//   2. asserts C renders as a nested row with a ✕ button
//   3. opens C as a pane (session_attach); the nested row stays put
//   4. ✕ on the open child closes the pane (session_close), row returns
//   5. ✕ on the closed child opens the confirm modal; confirming sends
//      session_delete and the row disappears once the fresh sessions
//      payload arrives
//   6. a child deleted elsewhere (session_removed) also stops rendering
//
// Run: node scripts/test_nested_sidebar.js
// APPJS env var selects the app.js copy to test (defaults to the real one).
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
const APPJS = process.env.APPJS || path.join(ROOT, 'internal/server/web/app.js');

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
  const tick = () => new Promise((r) => setTimeout(r, 0));
  const nestedRows = () => [...window.document.querySelectorAll('.session-row.nested')];
  const sent = (type, sessionId) => ws.sent.some((m) => m.type === type && (!sessionId || m.sessionId === sessionId));

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };

  // 1. Connect and adopt the parent session A.
  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  recv({
    type: 'config', sessionId: 'A', sessionLabel: 'parent-session',
    mode: 'act', thinkingLevel: 'off', reasoningEfforts: [], model: 'm1',
    modelDescription: '', workingDir: '/tmp', globalMode: false,
  });
  check('parent pane A adopted', evalJs('activePane().id') === 'A');

  // 2. The subagent spawns: live event + sessions payload render the child
  //    as a nested row under its parent, with a ✕ button.
  recv({
    type: 'subagent_started', subagentId: 'C', subagentParent: 'A',
    subagentLabel: 'subagent: fix parser', subagentJob: 'fix parser',
  });
  recv({
    type: 'sessions',
    sessions: [
      { id: 'A', label: 'parent-session', messageCount: 2, updatedAt: '' },
      { id: 'C', parentId: 'A', label: 'subagent: fix parser', messageCount: 1, active: true, updatedAt: '' },
    ],
  });
  let rows = nestedRows();
  check('child renders as one nested row under its parent', rows.length === 1 && rows[0].dataset.sessionId === 'C');
  check('nested row has a ✕ button', !!rows[0] && !!rows[0].querySelector('.session-row-del'));

  // 3. Clicking the nested row opens the child as a pane; the row stays
  //    nested under the parent (it does not jump to the flat section).
  rows[0].querySelector('.session-row-content').click();
  check('opening the child sends session_attach', sent('session_attach', 'C'));
  rows = nestedRows();
  check('open child still renders as a nested row', rows.length === 1 && rows[0].dataset.sessionId === 'C');

  // 4. ✕ on the OPEN child closes the pane (session_close, stays saved).
  rows[0].querySelector('.session-row-del').click();
  check('✕ on open child sends session_close', sent('session_close', 'C'));
  check('✕ on open child does NOT send session_delete', !sent('session_delete', 'C'));
  rows = nestedRows();
  check('closed child returns to its nested row', rows.length === 1 && rows[0].dataset.sessionId === 'C');

  // 5. ✕ on the CLOSED child opens the delete confirm modal; confirming
  //    sends session_delete for the child.
  rows[0].querySelector('.session-row-del').click();
  const filenameEl = window.document.getElementById('session-delete-filename');
  check('delete confirm modal names the child', !!filenameEl && (filenameEl.textContent || '').includes('subagent: fix parser'));
  window.document.getElementById('session-delete-confirm-btn').click();
  await tick();
  check('confirming sends session_delete for the child', sent('session_delete', 'C'));

  // The server replies (tagged with the pane's session id) and the client
  // asks for a fresh list; the fresh payload no longer lists C, so its
  // nested row disappears.
  recv({ type: 'response', content: 'Deleted session C.', sessionId: 'A' });
  check('delete reply triggers a fresh list request', sent('list_sessions'));
  recv({
    type: 'sessions',
    sessions: [
      { id: 'A', label: 'parent-session', messageCount: 2, updatedAt: '' },
    ],
  });
  rows = nestedRows();
  check('deleted child stops rendering under its parent', rows.length === 0);

  // 6. Restart scenario: a child with NO live events (subagent_started/
  //    finished are not replayed after a restart) renders from the payload
  //    fallback. Registered-but-idle (active, no turn) must show done —
  //    never the red failed dot.
  recv({
    type: 'sessions',
    sessions: [
      { id: 'A', label: 'parent-session', messageCount: 2, updatedAt: '' },
      { id: 'E', parentId: 'A', label: 'subagent: restarted', messageCount: 1, active: true, updatedAt: '' },
    ],
  });
  rows = nestedRows();
  check('restart child (no live events) renders from the payload', rows.length === 1 && rows[0].dataset.sessionId === 'E');
  const metaE = rows[0] ? (rows[0].querySelector('.session-row-meta').textContent || '') : '';
  check('registered-but-idle restart child shows done (not failed)',
    metaE.includes('done') && !metaE.includes('failed') && !metaE.includes('responding'));
  check('live-registered done child shows the green done dot',
    !!rows[0] && !!rows[0].querySelector('.session-state-dot.green'));

  // 6b. The persisted outcome drives the fallback: a child that REALLY
  //     failed stays failed after a restart (subagentStatus is persisted
  //     on the child's snapshot and carried in the sessions payload).
  recv({
    type: 'sessions',
    sessions: [
      { id: 'A', label: 'parent-session', messageCount: 2, updatedAt: '' },
      { id: 'F', parentId: 'A', label: 'subagent: failed job', messageCount: 1, active: false, subagentStatus: 'failed', subagentSummary: 'boom', updatedAt: '' },
    ],
  });
  rows = nestedRows();
  const metaF = rows[0] ? (rows[0].querySelector('.session-row-meta').textContent || '') : '';
  check('failed payload child (subagentStatus failed) shows failed',
    metaF.includes('failed') && metaF.includes('boom'));
  check('failed-but-inactive child shows NO colored dot', !!rows[0] && !rows[0].querySelector('.session-state-dot'));

  // 6c. Recursive nesting: grandchildren (depth >= 2) render under their
  //     child row, mirroring the server's cascade.
  recv({
    type: 'sessions',
    sessions: [
      { id: 'A', label: 'parent-session', messageCount: 2, updatedAt: '' },
      { id: 'G', parentId: 'A', label: 'subagent: mid', messageCount: 2, active: false, subagentStatus: 'success', updatedAt: '' },
      { id: 'H', parentId: 'G', label: 'subagent: deep', messageCount: 1, active: false, subagentStatus: 'success', updatedAt: '' },
    ],
  });
  rows = nestedRows();
  check('grandchild renders nested under its child row',
    rows.length === 2 && rows[0].dataset.sessionId === 'G' && rows[1].dataset.sessionId === 'H');
  // G/H are settled (success status, active:false, not open): text status
  // only — no colored dot.
  check('settled closed child shows NO colored dot',
    !!rows[0] && !rows[0].querySelector('.session-state-dot'));

  // 6d. A child that never reached a terminal state — interrupted
  //     (interrupt_agent leaves it continuable) or killed with its process —
  //     records status "running" (legacy records: ""). With NO live runtime
  //     both must read INTERRUPTED, never done: unfinished work used to render
  //     a green "done" row after a restart.
  recv({
    type: 'sessions',
    sessions: [
      { id: 'A', label: 'parent-session', messageCount: 2, updatedAt: '' },
      { id: 'I', parentId: 'A', label: 'subagent: killed', messageCount: 3, active: false, subagentStatus: 'running', updatedAt: '' },
    ],
  });
  rows = nestedRows();
  const metaI = rows[0] ? (rows[0].querySelector('.session-row-meta').textContent || '') : '';
  check('killed child (status running, runtime gone) shows interrupted',
    metaI.includes('interrupted') && !metaI.includes('done') && !metaI.includes('failed'));
  check('interrupted child shows NO colored dot',
    !!rows[0] && !rows[0].querySelector('.session-state-dot'));

  // A legacy record with no recorded status at all reads the same way.
  recv({
    type: 'sessions',
    sessions: [
      { id: 'A', label: 'parent-session', messageCount: 2, updatedAt: '' },
      { id: 'J', parentId: 'A', label: 'subagent: legacy', messageCount: 1, active: false, updatedAt: '' },
    ],
  });
  rows = nestedRows();
  const metaJ = rows[0] ? (rows[0].querySelector('.session-row-meta').textContent || '') : '';
  check('legacy child (no recorded status) shows interrupted',
    metaJ.includes('interrupted') && !metaJ.includes('done'));

  // A LIVE child between turns (status running, still active) is NOT
  // interrupted: "running" only means interrupted once its runtime is gone.
  recv({
    type: 'sessions',
    sessions: [
      { id: 'A', label: 'parent-session', messageCount: 2, updatedAt: '' },
      { id: 'K', parentId: 'A', label: 'subagent: live idle', messageCount: 4, active: true, subagentStatus: 'running', updatedAt: '' },
    ],
  });
  rows = nestedRows();
  const metaK = rows[0] ? (rows[0].querySelector('.session-row-meta').textContent || '') : '';
  check('live child (status running, active) is not interrupted',
    metaK.includes('done') && !metaK.includes('interrupted'));

  // 7. Cross-tab deletion: a child deleted elsewhere (session_removed
  //    broadcast) also stops rendering, and its open pane is replaced.
  recv({
    type: 'subagent_started', subagentId: 'D', subagentParent: 'A',
    subagentLabel: 'subagent: other job', subagentJob: 'other job',
  });
  recv({
    type: 'sessions',
    sessions: [
      { id: 'A', label: 'parent-session', messageCount: 2, updatedAt: '' },
      { id: 'D', parentId: 'A', label: 'subagent: other job', messageCount: 1, active: true, turnActive: true, updatedAt: '' },
    ],
  });
  rows = nestedRows();
  check('second child renders nested', rows.length === 1 && rows[0].dataset.sessionId === 'D');
  check('running payload child (turnActive) shows responding',
    !!rows[0] && (rows[0].querySelector('.session-row-meta').textContent || '').includes('responding'));
  check('running child shows the amber responding dot', !!rows[0] && !!rows[0].querySelector('.session-state-dot.amber'));
  rows[0].querySelector('.session-row-content').click();
  check('second child opened as a pane', sent('session_attach', 'D'));
  recv({ type: 'session_removed', sessionId: 'D' });
  rows = nestedRows();
  check('child removed elsewhere stops rendering', rows.length === 0);
  check('orphaned pane re-keys with session_new', sent('session_new'));

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
