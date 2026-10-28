'use strict';
// Regression checks for the kanban card detail's inline actions, the
// resizable columns, and the Tier-2 card rendering (review badge, block
// reason, done styling). Loads the REAL index.html + app.js +
// web/components/*.js into jsdom (imports stripped, editor/marked/dompurify
// stubbed, no server) and asserts:
//   - clicking a card expands the detail with Comment/Edit/Block/Done
//     buttons backed by the board_op update/comment/block/done messages,
//   - the edit form sends a full-replace update op,
//   - the block form requires a reason and sends it,
//   - each column exposes a resize handle that sets a width on drag and
//     resets on double-click,
//   - a blocked card shows its block reason, a done card is marked, and an
//     in-review card shows the review badge,
//   - an empty board shows the empty-state hint.
// Run: node scripts/test_board_card_actions.js
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

  const sent = [];
  window.WebSocket = class {
    constructor() { this.readyState = 1; }
    static OPEN = 1;
    send(data) { sent.push(JSON.parse(data)); }
    close() {}
  };
  window.HTMLElement.prototype.scrollIntoView = function () {};

  installEditorStubs(window);
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };

  loadAppJs(window);

  // The harness eval's app.js, which connects: grab the WS instance to drive
  // inbound messages (the direct handleBoardState export also works, but the
  // message path exercises the real dispatcher).
  window.applyServerConfig({ board: 'on', subagent: 'off', subagentMaxDepth: 1, reasoningEfforts: [], model: 'm1' });
  window.switchMainPane('board');

  const boardState = (items) => ({
    type: 'board_state',
    boardState: {
      columns: ['backlog', 'ready', 'in_progress', 'in_review', 'blocked', 'done'],
      items,
    },
  });
  const recv = (obj) => window.handleBoardState(obj);
  const colsDiv = () => document.getElementById('board-columns');
  const cardByID = (id) => colsDiv().querySelector(`.board-card[data-item-id="${id}"]`);
  const click = (el) => el.dispatchEvent(new window.MouseEvent('click', { bubbles: true, cancelable: true }));
  const actionBtn = (root, label) => Array.from(root.querySelectorAll('.board-card-action'))
    .find((b) => b.textContent === label);
  const op = (action) => sent.map((m) => m.boardOp).filter(Boolean).find((o) => o.action === action);
  const pointerEvent = (type, clientX) => {
    const ev = new window.Event(type, { bubbles: true, cancelable: true });
    Object.defineProperty(ev, 'clientX', { value: clientX });
    Object.defineProperty(ev, 'pointerId', { value: 1 });
    return ev;
  };

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };

  // ── Card detail actions ──
  recv(boardState([{
    id: '1', title: 'Fix parser', status: 'backlog', priority: 'low',
    description: 'make go test pass', context: 'the parser lives in internal/parser',
  }]));
  const card = cardByID('1');
  check('card rendered', !!card);
  click(card);
  const detail = card.querySelector('.board-card-detail');
  check('detail expands on click', !!detail && !detail.hidden);
  const contextEl = detail.querySelector('.board-card-context');
  check('card shows the context block', !!contextEl && contextEl.textContent.includes('internal/parser'));
  for (const label of ['Comment', 'Edit', 'Block', 'Done']) {
    check(`detail has the ${label} action`, !!actionBtn(detail, label));
  }

  // Edit swaps the read-only description for the form in place (it must not
  // stack a second copy of the text below the original).
  const descEl = detail.querySelector('.board-card-desc');
  check('description shown before editing', !!descEl && !descEl.hidden);
  click(actionBtn(detail, 'Edit'));
  const editForm = detail.querySelector('.board-card-edit');
  check('edit form opens', !!editForm && !editForm.hidden);
  check('read-only description is hidden while editing', descEl.hidden);
  check('read-only context hidden while editing', contextEl.hidden);
  check('edit form replaces the read-only fields in place',
    descEl.nextElementSibling === contextEl && contextEl.nextElementSibling === editForm);
  check('priority chip hidden while editing', card.classList.contains('board-editing'));
  check('edit fields are the content-fitted textareas',
    editForm.querySelectorAll('textarea.board-card-edit-auto').length === 2);

  // Cancel restores the read-only view without sending anything.
  click(actionBtn(detail, 'Cancel'));
  check('cancel closes the form and restores the description', editForm.hidden && !descEl.hidden);
  check('cancel sends no update op', !op('update'));

  // Escape anywhere in the form also cancels.
  click(actionBtn(detail, 'Edit'));
  editForm.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
  check('Escape cancels the edit form', editForm.hidden && !descEl.hidden);

  // Edit + Save sends a full-replace update op (including context) and
  // closes the form.
  click(actionBtn(detail, 'Edit'));
  const fields = editForm.querySelectorAll('textarea');
  editForm.querySelector('input').value = 'Fix parser properly';
  fields[0].value = 'updated body';
  fields[1].value = 'extra context for the agent';
  editForm.querySelector('select').value = 'urgent';
  click(actionBtn(detail, 'Save'));
  let update = op('update');
  check('edit sends an update op with all fields',
    !!update && update.id === '1' && update.title === 'Fix parser properly'
      && update.description === 'updated body' && update.priority === 'urgent'
      && update.context === 'extra context for the agent');
  check('save closes the form and restores the read-only fields',
    editForm.hidden && !descEl.hidden && !contextEl.hidden);

  // Comment: sends text from the comment box.
  detail.querySelector('.board-card-comment textarea').value = 'looks good to me';
  click(actionBtn(detail, 'Comment'));
  const comment = op('comment');
  check('comment op carries the text', !!comment && comment.id === '1' && comment.text === 'looks good to me');

  // Block: form opens, requires a reason, sends it.
  click(actionBtn(detail, 'Block'));
  const blockForm = detail.querySelector('.board-card-block');
  check('block form opens', !!blockForm && !blockForm.hidden);
  blockForm.querySelector('textarea').value = 'waiting on upstream';
  click(actionBtn(detail, 'Confirm block'));
  const block = op('block');
  check('block op carries the reason', !!block && block.id === '1' && block.reason === 'waiting on upstream');

  // Done: sends the done op.
  click(actionBtn(detail, 'Done'));
  check('done op sent', !!op('done'));

  // ── Column resize ──
  const col = colsDiv().querySelector('.board-column');
  const handle = col && col.querySelector('.board-col-resize');
  check('column exposes a resize handle', !!handle);
  if (handle) {
    handle.dispatchEvent(pointerEvent('pointerdown', 100));
    handle.dispatchEvent(pointerEvent('pointermove', 180));
    check('drag sets a clamped column width', col.style.width === '160px');
    handle.dispatchEvent(pointerEvent('pointerup', 180));
    handle.dispatchEvent(pointerEvent('dblclick', 180));
    check('double-click resets the column width', col.style.width === '' && col.style.flex === '');
  }

  // ── Tier 2 card rendering ──
  recv(boardState([
    { id: '2', title: 'blocked one', status: 'blocked', activity: [{ by: 'user', text: 'blocked: upstream outage' }] },
    { id: '3', title: 'finished one', status: 'done' },
    { id: '4', title: 'under review', status: 'in_review', reviewSession: 'sess-9', reviewRounds: 2 },
  ]));
  const blockedReason = cardByID('2').querySelector('.board-card-block-reason');
  check('blocked card shows its block reason',
    !!blockedReason && blockedReason.textContent.includes('upstream outage'));
  check('done card carries the done styling', cardByID('3').classList.contains('board-card-done'));
  const badge = cardByID('4').querySelector('.board-review-badge');
  check('in-review card shows the review badge with rounds',
    !!badge && badge.textContent.includes('×2'));

  // ── Empty state ──
  recv(boardState([]));
  check('empty board reveals the empty-state hint', !document.getElementById('board-empty').hidden);
  recv(boardState([{ id: '5', title: 'back', status: 'backlog' }]));
  check('non-empty board hides the empty-state hint', document.getElementById('board-empty').hidden);

  // ── Activity timestamps use the ONE relative-time policy ──
  // The board used to carry its own copy (boardRelTime) that rounded
  // differently and said "just now" for anything under 45 s — so the same
  // event read "just now" on the board and "30s ago" in the sidebar, and
  // "2m ago" on the board vs "1m ago" in the sidebar after 90 s.
  const isoAgo = (secs) => new Date(Date.now() - secs * 1000).toISOString();
  recv(boardState([{
    id: '7', title: 'aged', status: 'backlog',
    activity: [
      { at: isoAgo(30), by: 'gompa', text: 'created' },
      { at: isoAgo(90), by: 'gompa', text: 'commented' },
    ],
  }]));
  const agedCard = cardByID('7');
  click(agedCard);
  const stamps = Array.from(agedCard.querySelectorAll('.board-activity-time')).map((el) => el.textContent);
  check('activity times rendered', stamps.length === 2);
  check('30s-old activity uses the sidebar policy ("30s ago", not "just now")',
    stamps[0] === window.formatRelativeTime(new Date(Date.now() - 30 * 1000)));
  check('30s-old activity is not the old board-local "just now"', stamps[0] !== 'just now');
  check('90s-old activity floors like the sidebar ("1m ago", not "2m ago")', stamps[1] === '1m ago');

  if (failures === 0) {
    console.log('\nAll board card-action checks passed.');
    process.exit(0);
  }
  console.error(`\n${failures} check(s) failed.`);
  process.exit(1);
}

main().catch((e) => { console.error(e); process.exit(1); });
