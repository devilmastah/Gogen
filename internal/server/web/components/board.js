// Kanban board tab for the GoGen web UI: renders board_state
// broadcasts, sends board_op messages, and owns the per-ticket
// "Start agent" popover (shared ModelThinkingPicker + prompt editor).
//
// Server-backed like the feature toggles: the tab renders from
// board_state broadcasts (server→client) and sends board_op
// messages (client→server) for list/add/move/comment/done. The
// server re-broadcasts after every mutation — including agent
// board-tool calls — so the board stays live while agents work.
//
// Wiring: app.js calls initBoard(deps) once at startup.
//   deps.getWs()                  — the chat WebSocket (or null)
//   deps.requestModels()          — fetch the model catalog (the start
//                                   popover needs it; no-op if loaded)
//   deps.onSessionListRequested() — refresh the sidebar session list
//                                   (a board-started agent appeared)
//   deps.onOpenAgent(sessionId)   — "Open agent" button: switch to the
//                                   chat tab with the session attached
//   deps.getModels()              — the model catalog array
//   deps.getPane()                — the active pane (the picker's
//                                   "Workspace default" row)
//   deps.relativeTime(value, now) — the ONE relative-age formatter the
//                                   sidebar/transcript use (app.js passes
//                                   it, like it does for sessions.js)

import { createPopover } from '/components/popover.js';
import { icon } from '/components/icons.js';
import { createModelThinkingPicker } from '/components/model-picker.js';
import { storageGet, storageSet } from '/components/storage.js';

let deps = null;

export function initBoard(d) {
    deps = d;
    initBoardTab();
}

// ── Board rendering ──

let lastBoardState = null;
let boardDragId = null;
// Touch fallback for moving cards (HTML5 drag-and-drop does not work
// on touch): tapping a card's ⇄ button enters "move mode" for that
// card; tapping a column header then moves it. boardMoveCancel()
// clears the mode (also called on every re-render, like the start
// popover).
let boardMoveId = null;
// Per-column widths from the drag handle on each column's right edge,
// persisted in localStorage so the layout survives reloads. Keyed by
// column name; a missing entry keeps the responsive CSS default.
let boardColumnWidths = {};
try {
    boardColumnWidths = JSON.parse(storageGet('board-col-widths') || '{}') || {};
} catch (_) {
    boardColumnWidths = {};
}

function saveBoardColumnWidths() {
    storageSet('board-col-widths', JSON.stringify(boardColumnWidths));
}

function boardMoveCancel() {
    boardMoveId = null;
    document.querySelectorAll('.board-card.move-source')
        .forEach((c) => c.classList.remove('move-source'));
    document.querySelectorAll('.board-card-move.active')
        .forEach((b) => {
            b.classList.remove('active');
            b.innerHTML = icon('swap');
            b.title = 'Move card to another column';
        });
    const cols = document.getElementById('board-columns');
    if (cols) cols.classList.remove('move-mode');
}
// Set while a board "Start agent" op is in flight; cleared when the
// ticket's board_state shows the agentSession link (the sidebar then
// refreshes so the new session row appears).
let pendingBoardStartId = null;
// Per-ticket "Start agent" popover state: the open popover's
// selections (model, edited prompt template, whether the prompt
// editor is expanded, the review-agent section's per-ticket review
// options). The popover element is a singleton appended
// to <body>, so a board re-render while the user is mid-choice
// closes it without losing anything — nothing is sent until Start.
let boardStartState = null; // { item, model, prompt, promptOpen, thinkingLevel, reviewOpen, reviewModel, reviewThinkingLevel }
let boardStartPicker = null; // shared ModelThinkingPicker for the popover
// Second picker for the popover's review-agent section (per-ticket
// review options for the session auto-started on in_review). Like the
// worker picker it is state-only: the selections ride the start op
// (an immediate review op would re-render the board and close this
// popover mid-choice).
let reviewPicker = null;
// Shared popover shell (components/popover.js) for the singleton
// popover element: outside-click/Escape dismissal, fixed
// positioning under the anchor (flipped above when there is no
// room below), re-anchor on scroll/resize.
let boardStartPopoverCtl = null;
// The anchor element (the card's Start button) the open popover is
// positioned under; re-read on scroll/resize to keep the popover
// glued to its card. Null while closed.
let boardStartAnchor = null;
// Effective board_start_prompt template from the last config push
// ("" = built-in default); the popover's prompt editor pre-fills
// from it.
let boardStartPromptValue = '';

export function setBoardStartPrompt(value) {
    boardStartPromptValue = value;
}

export function handleBoardState(data) {
    if (data.boardState) {
        lastBoardState = data.boardState;
        // Rebuild only while the board pane is the active view: the
        // server broadcasts on every mutation (agent board-tool calls
        // included), and rebuilding a display:none pane wastes work.
        // Hidden panes pick the state up via renderBoard() on pane
        // switch (app.js switchMainPane).
        if (boardPaneVisible()) renderBoard();
        if (pendingBoardStartId) {
            const item = (data.boardState.items || []).find((i) => i.id === pendingBoardStartId);
            if (item && item.agentSession) {
                pendingBoardStartId = null;
                deps.onSessionListRequested();
            }
        }
    }
}

export function boardTabVisible() {
    const t = document.getElementById('board-tab');
    return !!t && !t.hidden;
}

// True while the board pane is the active main pane (the server
// re-broadcasts board_state on every mutation — including agent
// board-tool calls — so this gates the DOM rebuild: hidden panes
// keep lastBoardState fresh but skip the rebuild until shown).
export function boardPaneVisible() {
    const p = document.getElementById('board-pane');
    return !!p && p.classList.contains('active');
}

export function requestBoardState() {
    const s = deps.getWs();
    if (!s || s.readyState !== WebSocket.OPEN) return;
    s.send(JSON.stringify({ type: 'board_op', boardOp: { action: 'list' } }));
}

export function sendBoardOp(op) {
    const s = deps.getWs();
    if (!s || s.readyState !== WebSocket.OPEN) return;
    s.send(JSON.stringify({ type: 'board_op', boardOp: op }));
}

export function renderBoard() {
    // A board_state re-render rebuilds every card; the floating
    // popover's anchor would be gone. Close it — nothing is sent
    // until Start, so no input is lost.
    closeBoardStartPopover();
    // Same for tap-to-move mode: the source card's DOM is gone, and
    // the board may have changed under us.
    boardMoveCancel();
    const colsDiv = document.getElementById('board-columns');
    if (!colsDiv) return;
    colsDiv.innerHTML = '';
    const emptyHint = document.getElementById('board-empty');
    const snap = lastBoardState;
    if (!snap) {
        if (emptyHint) emptyHint.hidden = true;
        return;
    }
    const byColumn = new Map();
    for (const col of snap.columns) byColumn.set(col, []);
    for (const item of snap.items || []) {
        const list = byColumn.get(item.status) || byColumn.get('backlog');
        if (list) list.push(item);
    }
    for (const col of snap.columns) {
        colsDiv.appendChild(buildBoardColumn(col, byColumn.get(col) || []));
    }
    if (emptyHint) emptyHint.hidden = (snap.items || []).length !== 0;
}

function buildBoardColumn(name, items) {
    const col = document.createElement('div');
    col.className = 'board-column';
    col.dataset.column = name;
    // A persisted width overrides the responsive CSS default; resizeColumn
    // writes it back on drag.
    const storedWidth = boardColumnWidths[name];
    if (storedWidth) {
        col.style.flex = `0 0 ${storedWidth}px`;
        col.style.width = `${storedWidth}px`;
        col.style.maxWidth = 'none';
    }
    const header = document.createElement('div');
    header.className = 'board-column-header';
    const title = document.createElement('span');
    title.className = 'board-column-title';
    title.textContent = name.replace('_', ' ');
    const count = document.createElement('span');
    count.className = 'board-column-count';
    count.textContent = String(items.length);
    header.append(title, count);
    col.appendChild(header);
    // Tap-to-move target (touch fallback): in move mode the header
    // is highlighted by .board-columns.move-mode; tapping it moves
    // the selected card here.
    header.addEventListener('click', (e) => {
        e.stopPropagation();
        if (!boardMoveId) return;
        if (name !== currentCardColumn(boardMoveId)) {
            sendBoardOp({ action: 'move', id: boardMoveId, column: name });
        }
        boardMoveCancel();
    });
    const body = document.createElement('div');
    body.className = 'board-column-body';
    for (const item of items) body.appendChild(buildBoardCard(item));
    // Drop target for the whole column (cards are dragged onto it).
    col.addEventListener('dragover', (e) => {
        e.preventDefault();
        col.classList.add('drag-over');
    });
    col.addEventListener('dragleave', () => col.classList.remove('drag-over'));
    col.addEventListener('drop', (e) => {
        e.preventDefault();
        col.classList.remove('drag-over');
        if (boardDragId && name !== currentCardColumn(boardDragId)) {
            sendBoardOp({ action: 'move', id: boardDragId, column: name });
        }
        boardDragId = null;
    });
    col.appendChild(body);
    attachColumnResize(col, name);
    return col;
}

function currentCardColumn(id) {
    const snap = lastBoardState;
    if (!snap) return null;
    const item = (snap.items || []).find((i) => i.id === id);
    return item ? item.status : null;
}

function buildBoardCard(item) {
    const card = document.createElement('div');
    card.className = 'board-card';
    if (item.status === 'done') card.classList.add('board-card-done');
    card.draggable = true;
    card.dataset.itemId = item.id;
    card.addEventListener('dragstart', (e) => {
        boardDragId = item.id;
        if (e.dataTransfer) e.dataTransfer.setData('text/plain', item.id);
    });
    card.addEventListener('dragend', () => { boardDragId = null; });
    // Remove button: hover-revealed trashcan with a two-step inline
    // confirm (click once → "Remove?" highlighted; click again →
    // delete; Esc/click elsewhere cancels). No modal needed for a
    // card, mirroring the session rows' hover-reveal ✕.
    const removeBtn = document.createElement('button');
    removeBtn.type = 'button';
    removeBtn.className = 'board-card-remove';
    removeBtn.title = 'Remove card';
    removeBtn.textContent = '🗑';
    let removeArmed = false;
    removeBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        if (!removeArmed) {
            removeArmed = true;
            removeBtn.classList.add('armed');
            removeBtn.textContent = 'Remove?';
            return;
        }
        sendBoardOp({ action: 'remove', id: item.id });
        removeArmed = false;
        removeBtn.classList.remove('armed');
        removeBtn.textContent = '🗑';
    });
    card.addEventListener('click', (e) => {
        if (e.target.closest('.board-card-detail') || e.target === removeBtn) return;
        // A tap while move mode is active is not a detail toggle: it
        // cancels the pending move (or, on the source card itself,
        // does nothing extra — its ⇄ button became ✕).
        if (boardMoveId) {
            boardMoveCancel();
            return;
        }
        // Clicking anywhere else cancels a pending remove confirm.
        if (removeArmed) {
            removeArmed = false;
            removeBtn.classList.remove('armed');
            removeBtn.textContent = '🗑';
            return;
        }
        detail.hidden = !detail.hidden;
    });
    card.appendChild(removeBtn);
    const title = document.createElement('div');
    title.className = 'board-card-title';
    title.textContent = `#${item.id} ${item.title}`;
    card.appendChild(title);
    const meta = document.createElement('div');
    meta.className = 'board-card-meta';
    const frags = [];
    if (item.priority) {
        const prio = document.createElement('span');
        prio.className = 'board-prio prio-' + item.priority;
        prio.textContent = item.priority;
        frags.push(prio);
    }
    if (item.assignee) {
        const who = document.createElement('span');
        who.className = 'board-assignee';
        who.textContent = item.assignee;
        frags.push(who);
    }
    if (item.status === 'in_review' && item.reviewSession) {
        const badge = document.createElement('span');
        badge.className = 'board-review-badge';
        badge.textContent = item.reviewRounds > 1 ? `review ×${item.reviewRounds}` : 'review';
        badge.title = 'A review agent is running for this ticket';
        frags.push(badge);
    }
    for (const f of frags) meta.appendChild(f);
    // Touch-only move button (shown via @media (hover: none)):
    // enters move mode for this card; the mode is cancelled by
    // tapping the button again, another card, or a re-render.
    const moveBtn = document.createElement('button');
    moveBtn.type = 'button';
    moveBtn.className = 'board-card-move';
    moveBtn.title = 'Move card to another column';
    moveBtn.innerHTML = icon('swap');
    moveBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        if (boardMoveId === item.id) {
            boardMoveCancel();
            return;
        }
        boardMoveCancel();
        boardMoveId = item.id;
        card.classList.add('move-source');
        moveBtn.classList.add('active');
        moveBtn.innerHTML = icon('x');
        moveBtn.title = 'Cancel — tap a column header to move the card';
        const cols = document.getElementById('board-columns');
        if (cols) cols.classList.add('move-mode');
    });
    meta.appendChild(moveBtn);
    card.appendChild(meta);
    // Blocked cards surface the most recent block reason under the meta row.
    const blockReason = boardBlockReason(item);
    if (blockReason) {
        const reason = document.createElement('div');
        reason.className = 'board-card-block-reason';
        reason.textContent = blockReason;
        card.appendChild(reason);
    }
    // Start / open agent button: the first click opens the per-ticket
    // "Start agent" popover (model picker + pen-icon prompt editor)
    // instead of starting immediately; once the ticket's board_state
    // carries agentSession, the button becomes "Open agent" and
    // switches to the chat tab with the session attached. Hidden for
    // done cards; disabled while another actor (an agent via the
    // board tool) holds the ticket.
    const startBtn = document.createElement('button');
    startBtn.type = 'button';
    startBtn.className = 'board-card-start';
    if (item.status === 'done') {
        startBtn.hidden = true;
    } else if (item.agentSession) {
        startBtn.textContent = 'Open agent';
        startBtn.title = 'Open the agent session working on this ticket';
        startBtn.addEventListener('click', (e) => {
            e.stopPropagation();
            deps.onOpenAgent(item.agentSession);
        });
    } else {
        startBtn.innerHTML = icon('play') + ' Start';
        if (item.assignee) {
            startBtn.disabled = true;
            startBtn.title = 'Claimed by ' + item.assignee;
        } else {
            startBtn.title = 'Start an agent for this ticket';
            startBtn.addEventListener('click', (e) => {
                e.stopPropagation();
                openBoardStartPopover(item, startBtn);
            });
        }
    }
    card.appendChild(startBtn);
    // Click expands the detail (description + activity + inline actions).
    const detail = buildBoardCardDetail(item);
    card.appendChild(detail);
    return card;
}

// buildBoardCardDetail builds a card's expanded panel: the description, the
// activity tail, the inline comment box, and the edit/block/done actions
// (the forms hidden until their button is clicked).
function buildBoardCardDetail(item) {
    const detail = document.createElement('div');
    detail.className = 'board-card-detail';
    detail.hidden = true;
    // Read-only description and context, swapped out for the edit form's
    // fields while editing so the text is never shown twice.
    const descEl = document.createElement('div');
    descEl.className = 'board-card-desc';
    if (item.description) {
        descEl.textContent = item.description;
    } else {
        descEl.classList.add('board-card-desc-empty');
        descEl.textContent = 'No description';
    }
    detail.appendChild(descEl);

    let contextEl = null;
    if (item.context) {
        contextEl = document.createElement('div');
        contextEl.className = 'board-card-context';
        const label = document.createElement('span');
        label.className = 'board-card-context-label';
        label.textContent = 'Context';
        const body = document.createElement('div');
        body.className = 'board-card-context-body';
        body.textContent = item.context;
        contextEl.append(label, body);
        detail.appendChild(contextEl);
    }

    // Inline edit form: it REPLACES the read-only fields in place (and hides
    // the priority chip) rather than stacking a second copy below them.
    let editForm;
    let openEditForm;
    const setEditing = (editing) => {
        editForm.hidden = !editing;
        descEl.hidden = editing;
        if (contextEl) contextEl.hidden = editing;
        editBtn.classList.toggle('active', editing);
        const card = editBtn.closest('.board-card');
        if (card) card.classList.toggle('board-editing', editing);
    };
    const closeEdit = () => setEditing(false);
    const built = buildBoardEditForm(item, closeEdit);
    editForm = built.el;
    openEditForm = built.open;
    editForm.hidden = true;
    detail.appendChild(editForm);

    if (item.activity && item.activity.length) {
        detail.appendChild(buildBoardActivity(item.activity));
    }

    // Comment box: a note on the ticket (the server's comment op).
    const commentRow = document.createElement('div');
    commentRow.className = 'board-card-comment';
    const commentInput = document.createElement('textarea');
    commentInput.className = 'board-card-input';
    commentInput.rows = 2;
    commentInput.placeholder = 'Add a comment…';
    const commentBtn = boardDetailButton('Comment', () => {
        const text = commentInput.value.trim();
        if (!text) {
            commentInput.focus();
            return;
        }
        sendBoardOp({ action: 'comment', id: item.id, text });
        commentInput.value = '';
    });
    commentRow.append(commentInput, commentBtn);
    detail.appendChild(commentRow);

    // Action row: Edit / Block / Done (Done hidden on done cards).
    const actions = document.createElement('div');
    actions.className = 'board-card-actions';
    const editBtn = boardDetailButton('Edit', () => {
        if (editForm.hidden) {
            setEditing(true);
            openEditForm();
        } else {
            closeEdit();
        }
    });
    actions.appendChild(editBtn);
    const blockForm = buildBoardBlockForm(item);
    blockForm.hidden = true;
    const blockBtn = boardDetailButton('Block', () => {
        blockForm.hidden = !blockForm.hidden;
        blockBtn.classList.toggle('active', !blockForm.hidden);
    });
    actions.appendChild(blockBtn);
    if (item.status !== 'done') {
        actions.appendChild(boardDetailButton('Done', () => {
            sendBoardOp({ action: 'done', id: item.id });
        }));
    }
    detail.appendChild(actions);
    detail.appendChild(blockForm);
    return detail;
}

// boardDetailButton builds a small action button for a card's detail panel.
// It stops propagation so a click never toggles the card's expanded state.
function boardDetailButton(label, onClick) {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'board-card-action';
    btn.textContent = label;
    btn.addEventListener('click', (e) => {
        e.stopPropagation();
        onClick();
    });
    return btn;
}

// autoSizeTextarea grows a textarea to exactly fit its content (border-box),
// so an editing field matches the height of the text it replaces instead of
// showing its own scrollbar.
function autoSizeTextarea(el) {
    el.style.height = 'auto';
    const style = window.getComputedStyle(el);
    const border = (parseFloat(style.borderTopWidth) || 0) + (parseFloat(style.borderBottomWidth) || 0);
    el.style.height = (el.scrollHeight + border) + 'px';
}

// buildBoardEditForm builds the inline edit form (title required; description,
// context, priority) that stands in for the read-only fields while editing.
// It returns the element plus an open() hook that re-fits the growing
// textareas to their text (a hidden textarea reports no height, so the fit
// must run after the form is shown). Save sends a full-replace update op;
// both Save and Cancel hand the view back via close (Cancel restores the
// original values first). Escape anywhere in the form cancels.
function buildBoardEditForm(item, close) {
    const form = document.createElement('div');
    form.className = 'board-card-edit';
    const title = document.createElement('input');
    title.type = 'text';
    title.className = 'board-card-input';
    title.value = item.title || '';
    title.placeholder = 'Title (required)';
    title.autocomplete = 'off';
    const desc = document.createElement('textarea');
    desc.className = 'board-card-input board-card-edit-auto';
    desc.rows = 1;
    desc.value = item.description || '';
    desc.placeholder = 'Description / acceptance criteria';
    desc.addEventListener('input', () => autoSizeTextarea(desc));
    // The {context} the started agent receives (merged ahead of the ticket's
    // activity log).
    const ctx = document.createElement('textarea');
    ctx.className = 'board-card-input board-card-edit-auto';
    ctx.rows = 1;
    ctx.value = item.context || '';
    ctx.placeholder = 'Context for the agent (optional)';
    ctx.addEventListener('input', () => autoSizeTextarea(ctx));
    const prio = document.createElement('select');
    prio.className = 'board-card-input';
    for (const [value, label] of [['', 'priority…'], ['low', 'low'], ['medium', 'medium'], ['high', 'high'], ['urgent', 'urgent']]) {
        const opt = document.createElement('option');
        opt.value = value;
        opt.textContent = label;
        prio.appendChild(opt);
    }
    prio.value = item.priority || '';
    const save = boardDetailButton('Save', () => {
        const trimmed = title.value.trim();
        if (!trimmed) {
            title.focus();
            return;
        }
        sendBoardOp({
            action: 'update',
            id: item.id,
            title: trimmed,
            description: desc.value.trim(),
            priority: prio.value,
            context: ctx.value.trim(),
        });
        close();
    });
    save.classList.add('primary');
    const cancel = boardDetailButton('Cancel', () => {
        title.value = item.title || '';
        desc.value = item.description || '';
        ctx.value = item.context || '';
        prio.value = item.priority || '';
        close();
    });
    form.addEventListener('keydown', (e) => {
        if (e.key === 'Escape') {
            e.stopPropagation();
            cancel.click();
        }
    });
    const row = document.createElement('div');
    row.className = 'board-card-actions';
    row.append(save, cancel);
    form.append(title, desc, ctx, prio, row);
    return { el: form, open: () => { autoSizeTextarea(desc); autoSizeTextarea(ctx); } };
}

// buildBoardBlockForm builds the inline block-reason form (Confirm sends a
// block op; a reason is required).
function buildBoardBlockForm(item) {
    const form = document.createElement('div');
    form.className = 'board-card-block';
    const reason = document.createElement('textarea');
    reason.className = 'board-card-input';
    reason.rows = 2;
    reason.placeholder = 'Why is this blocked?';
    const go = boardDetailButton('Confirm block', () => {
        const text = reason.value.trim();
        if (!text) {
            reason.focus();
            return;
        }
        sendBoardOp({ action: 'block', id: item.id, reason: text });
        reason.value = '';
    });
    go.classList.add('primary');
    form.append(reason, go);
    return form;
}

// buildBoardActivity renders the tail of a ticket's activity log (last 10
// entries) with a relative timestamp and per-kind styling.
function buildBoardActivity(activity) {
    const box = document.createElement('div');
    box.className = 'board-card-activity';
    for (const act of activity.slice(-10)) {
        const row = document.createElement('div');
        row.className = 'board-activity-row kind-' + boardActivityKind(String(act.text || ''));
        const time = document.createElement('span');
        time.className = 'board-activity-time';
        time.textContent = deps.relativeTime(act.at);
        time.title = act.at ? new Date(act.at).toLocaleString() : '';
        const who = document.createElement('span');
        who.className = 'board-activity-who';
        who.textContent = (act.by || '?') + ':';
        const text = document.createElement('span');
        text.className = 'board-activity-text';
        text.textContent = ' ' + (act.text || '');
        row.append(time, who, text);
        box.appendChild(row);
    }
    return box;
}

// boardActivityKind classifies an activity entry for styling: block reasons,
// column moves, review/agent-session events, generated transitions, and
// everything else (comments).
function boardActivityKind(text) {
    const t = text.trim();
    if (t.startsWith('blocked:')) return 'block';
    if (t.startsWith('moved to ')) return 'move';
    if (t.startsWith('review ') || t.startsWith('auto-review')) return 'review';
    if (t.startsWith('agent session')) return 'agent';
    if (t === 'created' || t === 'claimed' || t === 'marked done' || t.startsWith('edited')) return 'system';
    return 'comment';
}

// boardBlockReason returns the most recent block reason for a blocked
// ticket ("" when the card is not blocked or no reason is logged).
function boardBlockReason(item) {
    if (item.status !== 'blocked') return '';
    const acts = item.activity || [];
    for (let i = acts.length - 1; i >= 0; i--) {
        const text = String(acts[i].text || '');
        if (text.startsWith('blocked: ')) return text.slice('blocked: '.length);
    }
    return '';
}

// attachColumnResize adds the drag handle on a column's right edge. Widths
// are clamped, persisted in localStorage (boardColumnWidths), and reset per
// column by double-clicking the handle.
function attachColumnResize(col, name) {
    const handle = document.createElement('div');
    handle.className = 'board-col-resize';
    handle.title = 'Drag to resize — double-click to reset';
    handle.addEventListener('pointerdown', (e) => {
        e.preventDefault();
        e.stopPropagation();
        const startX = e.clientX;
        const startWidth = col.getBoundingClientRect().width;
        try { handle.setPointerCapture(e.pointerId); } catch (_) { /* unsupported */ }
        document.body.classList.add('board-resizing');
        const onMove = (ev) => {
            const width = Math.max(160, Math.min(560, Math.round(startWidth + (ev.clientX - startX))));
            col.style.flex = `0 0 ${width}px`;
            col.style.width = `${width}px`;
            col.style.maxWidth = 'none';
        };
        const onUp = () => {
            handle.removeEventListener('pointermove', onMove);
            handle.removeEventListener('pointerup', onUp);
            handle.removeEventListener('pointercancel', onUp);
            document.body.classList.remove('board-resizing');
            boardColumnWidths[name] = Math.round(col.getBoundingClientRect().width);
            saveBoardColumnWidths();
        };
        handle.addEventListener('pointermove', onMove);
        handle.addEventListener('pointerup', onUp);
        handle.addEventListener('pointercancel', onUp);
    });
    handle.addEventListener('dblclick', (e) => {
        e.preventDefault();
        e.stopPropagation();
        delete boardColumnWidths[name];
        saveBoardColumnWidths();
        col.style.flex = '';
        col.style.width = '';
        col.style.maxWidth = '';
    });
    col.appendChild(handle);
}

function initBoardTab() {
    const addBtn = document.getElementById('board-add-btn');
    if (!addBtn) return;
    addBtn.addEventListener('click', () => {
        const form = document.getElementById('board-add-form');
        if (form) form.hidden = !form.hidden;
    });
    const form = document.getElementById('board-add-form');
    if (!form) return;
    form.addEventListener('submit', (e) => {
        e.preventDefault();
        const title = document.getElementById('board-add-title');
        const desc = document.getElementById('board-add-desc');
        const prio = document.getElementById('board-add-prio');
        if (!title || !title.value.trim()) return;
        sendBoardOp({
            action: 'add',
            title: title.value.trim(),
            description: desc ? desc.value.trim() : '',
            priority: prio ? prio.value : '',
        });
        title.value = '';
        if (desc) desc.value = '';
        if (prio) prio.value = '';
        form.hidden = true;
    });
}

// ── "Start agent" popover (per-ticket model + effort + prompt) ──
// Clicking a card's "▶ Start" opens a small floating toolbar-like
// popover instead of starting immediately: a model picker (reusing
// the shared renderModelList), a model-aware reasoning-effort chip
// row (Inherit = the active pane's live level, the pre-existing
// behavior; Off; the model's accepted values), plus a pen icon
// that expands the prompt editor (textarea prefilled with the
// effective board_start_prompt template). "Start" sends board_op
// {action:'start', id, model, prompt, thinkingLevel}; the server
// stores all three on the ticket so the popover pre-fills on the
// next start.

// The singleton popover element + its shared popover shell
// (components/popover.js), built lazily on first open.
function ensureBoardStartPopover() {
    let pop = document.getElementById('board-start-popover');
    if (pop) return pop;
    pop = buildBoardStartPopover();
    boardStartPopoverCtl = createPopover({
        el: pop,
        getAnchor: () => boardStartAnchor,
        fixed: true,
        // The popover is a singleton: a board re-render while the
        // user is mid-choice closes it without losing anything —
        // nothing is sent until Start.
        onClose: () => {
            boardStartState = null;
            boardStartAnchor = null;
        },
    });
    return pop;
}

export function openBoardStartPopover(item, anchor) {
    const pop = ensureBoardStartPopover();
    if (deps.getModels().length <= 1) {
        // Catalog not fetched yet: request it (the reply refreshes
        // the catalog; the popover re-renders on the next open or
        // the row click).
        deps.requestModels();
    }
    boardStartState = {
        item,
        model: item.model || '',
        prompt: item.prompt || '',
        // '' = Inherit the active pane's live level (the pre-existing
        // behavior); the ticket's stored override pre-fills here.
        thinkingLevel: item.thinkingLevel || '',
        // Review-agent section: the ticket's stored per-ticket review
        // options pre-fill here (the review session auto-started when
        // the ticket enters in_review runs with them).
        reviewOpen: false,
        reviewModel: item.reviewModel || '',
        reviewThinkingLevel: item.reviewThinkingLevel || '',
        promptOpen: false,
    };
    boardStartAnchor = anchor;
    pop.querySelector('.board-start-filter').value = '';
    pop.querySelector('.board-start-title').textContent = `#${item.id} ${item.title}`;
    // The popover is a singleton: the previous attempt disabled its
    // Start button on click (and a failed start resyncs the board
    // without rebuilding the popover), so re-arm it for this open.
    const goBtn = pop.querySelector('.board-card-start');
    if (goBtn) goBtn.disabled = false;
    // The shared shell shows the popover and positions it below
    // the anchor (flipped above when there is no room below,
    // clamped to the viewport either way; it closes on its own
    // if the anchor has scrolled out of view) and owns the
    // outside-click/Escape dismissal and scroll/resize re-anchor.
    boardStartPopoverCtl.open();
    boardStartPicker.render();
    reviewPicker.render();
    syncBoardStartReviewSection();
    syncBoardStartPromptEditor();
}

export function closeBoardStartPopover() {
    if (boardStartPopoverCtl) boardStartPopoverCtl.close();
}

// Re-runs the picker's model-aware effort guard and re-renders while
// the popover is open: a late list_models reply can make a previously
// unknown model's accepted values knowable, at which point a stale
// effort must reset (the server re-validates at start).
export function refreshBoardStartPicker() {
    if (boardStartPopoverCtl && boardStartPopoverCtl.isOpen()) {
        boardStartPicker.refresh();
        reviewPicker.refresh();
    }
}

function buildBoardStartPopover() {
    const pop = document.createElement('div');
    pop.id = 'board-start-popover';
    pop.className = 'board-start-popover';

    const title = document.createElement('div');
    title.className = 'board-start-title';
    const closeBtn = document.createElement('button');
    closeBtn.type = 'button';
    closeBtn.className = 'board-start-close';
    closeBtn.innerHTML = icon('x');
    closeBtn.title = 'Close';
    closeBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        closeBoardStartPopover();
    });
    const head = document.createElement('div');
    head.className = 'board-start-head';
    head.append(title, closeBtn);
    pop.appendChild(head);

    const filter = document.createElement('input');
    filter.type = 'text';
    filter.className = 'board-start-filter';
    filter.placeholder = 'Filter models…';
    filter.autocomplete = 'off';
    filter.addEventListener('input', () => boardStartPicker.render());
    const list = document.createElement('div');
    list.className = 'board-start-model-list';
    pop.append(filter, list);

    // Reasoning-effort chips: MODEL-AWARE (the selected ticket
    // model's accepted values from the catalog; the active pane's
    // model values for "Workspace default"; the default set as a
    // last resort — the server validates against the final model
    // at start time as the backstop). Leading "Inherit" chip = the
    // empty value (the active pane's live level, the pre-existing
    // behavior); "Off" = no reasoning_effort sent.
    const thinkingRow = document.createElement('div');
    thinkingRow.className = 'board-start-thinking';
    const thinkingLabel = document.createElement('span');
    thinkingLabel.className = 'board-start-thinking-label';
    thinkingLabel.textContent = 'Reasoning effort';
    const thinkingGrid = document.createElement('div');
    thinkingGrid.className = 'tb-thinking-grid board-start-thinking-grid';
    thinkingRow.append(thinkingLabel, thinkingGrid);
    pop.appendChild(thinkingRow);

    // The shared ModelThinkingPicker (with the settings subagent
    // picker): the "Workspace default" row is the empty value
    // (the server uses the workspace default model when the op
    // carries none), and while it is selected the pane's current
    // model row is highlighted to show what the default resolves
    // to. Selections stay in boardStartState and go out with the
    // start op — nothing is persisted on change.
    boardStartPicker = createModelThinkingPicker({
        listEl: list,
        filterEl: filter,
        chipsEl: thinkingGrid,
        getState: () => boardStartState,
        getModels: () => deps.getModels(),
        getPane: () => deps.getPane(),
        defaultRow: { label: 'Workspace default', title: 'Use the workspace default model' },
        inheritChipTitle: "Inherit the active pane's reasoning effort",
        paneCurrentRow: true,
    });

    // Review-agent section: the ticket's per-ticket review options for
    // the review session auto-started when this ticket moves into
    // in_review (feature flag "Board review agent" must be on; without
    // it the section is inert). Collapsed by default; its selections
    // ride the start op and are persisted on the ticket by the server
    // (an immediate review-options op would re-render the board and
    // close this popover mid-choice).
    const reviewSection = document.createElement('div');
    reviewSection.className = 'board-start-review-section';
    const reviewHead = document.createElement('div');
    reviewHead.className = 'board-start-review-head';
    const reviewToggle = document.createElement('button');
    reviewToggle.type = 'button';
    reviewToggle.className = 'board-start-pen board-start-review-toggle';
    reviewToggle.textContent = 'Review agent';
    reviewToggle.title = 'Per-ticket model and reasoning effort for the review session started when the ticket enters in_review';
    reviewToggle.addEventListener('click', (e) => {
        e.stopPropagation();
        if (!boardStartState) return;
        boardStartState.reviewOpen = !boardStartState.reviewOpen;
        syncBoardStartReviewSection();
    });
    reviewHead.append(reviewToggle);
    const reviewBody = document.createElement('div');
    reviewBody.className = 'board-start-review-body';
    reviewBody.hidden = true;
    const reviewFilter = document.createElement('input');
    reviewFilter.type = 'text';
    reviewFilter.className = 'board-start-filter board-start-review-filter';
    reviewFilter.placeholder = 'Filter models…';
    reviewFilter.autocomplete = 'off';
    reviewFilter.addEventListener('input', () => reviewPicker.render());
    // Dedicated classes (NOT the worker grid's): the review list and
    // chips live in the same popover, and selectors like
    // `.board-start-thinking-grid .tb-thinking-chip` must keep matching
    // the worker picker only (the web-harness regression tests query
    // them).
    const reviewList = document.createElement('div');
    reviewList.className = 'board-start-review-model-list';
    const reviewChips = document.createElement('div');
    reviewChips.className = 'tb-thinking-grid board-start-review-thinking-grid';
    const reviewNote = document.createElement('p');
    reviewNote.className = 'board-start-review-note';
    reviewNote.textContent = 'For the review session auto-started when the ticket enters in_review (needs the "Board review agent" setting). Empty model = the workspace default review model.';
    reviewBody.append(reviewFilter, reviewList, reviewChips, reviewNote);
    reviewSection.append(reviewHead, reviewBody);
    pop.appendChild(reviewSection);

    // The review picker: same shared factory, no pane-current row (the
    // cascade resolves ticket override → workspace default), no
    // change callbacks — the selections stay local until Start.
    reviewPicker = createModelThinkingPicker({
        listEl: reviewList,
        filterEl: reviewFilter,
        chipsEl: reviewChips,
        getState: () => boardStartState,
        getModels: () => deps.getModels(),
        getPane: () => deps.getPane(),
        defaultRow: { label: 'Workspace default review model', title: 'Use the workspace default review model' },
        inheritChipTitle: 'Inherit the workspace reasoning effort',
        stripPaneCurrent: true,
    });

    // Prompt editor: hidden until the pen icon is clicked.
    const promptSection = document.createElement('div');
    promptSection.className = 'board-start-prompt-section';
    promptSection.hidden = true;
    const promptHead = document.createElement('div');
    promptHead.className = 'board-start-prompt-head';
    const promptLabel = document.createElement('span');
    promptLabel.textContent = 'Prompt for the agent';
    const resetBtn = document.createElement('button');
    resetBtn.type = 'button';
    resetBtn.className = 'board-start-reset';
    resetBtn.textContent = 'Reset to template';
    resetBtn.title = 'Use the configured board agent prompt template';
    resetBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        if (!boardStartState) return;
        // Empty = the configured template; the server clears the
        // ticket's stored override on the next start.
        boardStartState.prompt = '';
        syncBoardStartPromptEditor();
    });
    promptHead.append(promptLabel, resetBtn);
    const promptInput = document.createElement('textarea');
    promptInput.className = 'board-start-prompt-input';
    promptInput.rows = 8;
    promptInput.spellcheck = false;
    promptInput.addEventListener('input', () => {
        if (!boardStartState) return;
        boardStartState.prompt = promptInput.value;
        promptPreview.textContent = renderBoardStartPreview(boardStartState.item, promptInput.value);
    });
    const promptPreview = document.createElement('div');
    promptPreview.className = 'board-start-preview';
    promptSection.append(promptHead, promptInput, promptPreview);
    pop.appendChild(promptSection);

    const actions = document.createElement('div');
    actions.className = 'board-start-actions';
    const penBtn = document.createElement('button');
    penBtn.type = 'button';
    penBtn.className = 'board-start-pen';
    penBtn.innerHTML = icon('pen');
    penBtn.title = 'Edit the prompt';
    penBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        if (!boardStartState) return;
        boardStartState.promptOpen = !boardStartState.promptOpen;
        syncBoardStartPromptEditor();
    });
    const goBtn = document.createElement('button');
    goBtn.type = 'button';
    goBtn.className = 'board-card-start';
    goBtn.innerHTML = icon('play') + ' Start';
    goBtn.addEventListener('click', (e) => {
        e.stopPropagation();
        if (!boardStartState) return;
        goBtn.disabled = true;
        pendingBoardStartId = boardStartState.item.id;
        sendBoardOp({
            action: 'start',
            id: boardStartState.item.id,
            model: boardStartState.model,
            prompt: boardStartState.prompt,
            thinkingLevel: boardStartState.thinkingLevel,
            reviewModel: boardStartState.reviewModel,
            reviewThinkingLevel: boardStartState.reviewThinkingLevel,
        });
        closeBoardStartPopover();
    });
    actions.append(penBtn, goBtn);
    pop.appendChild(actions);

    document.body.appendChild(pop);
    return pop;
}

// Syncs the review section with the current state: collapsed until the
// toggle is used; the toggle stays highlighted while a per-ticket review
// option is armed (chosen and kept after collapsing).
function syncBoardStartReviewSection() {
    if (!boardStartState) return;
    const pop = document.getElementById('board-start-popover');
    const body = pop.querySelector('.board-start-review-body');
    const toggle = pop.querySelector('.board-start-review-toggle');
    if (!body || !toggle) return;
    body.hidden = !boardStartState.reviewOpen;
    toggle.classList.toggle('active', boardStartState.reviewOpen
        || boardStartState.reviewModel !== ''
        || boardStartState.reviewThinkingLevel !== '');
}

// Syncs the popover's prompt editor with the current state: the
// textarea shows the edited template, or the effective configured
// template when none was chosen, with a live rendered preview.
function syncBoardStartPromptEditor() {
    if (!boardStartState) return;
    const pop = document.getElementById('board-start-popover');
    const promptSection = pop.querySelector('.board-start-prompt-section');
    const promptInput = pop.querySelector('.board-start-prompt-input');
    const promptPreview = pop.querySelector('.board-start-preview');
    const penBtn = pop.querySelector('.board-start-pen');
    promptSection.hidden = !boardStartState.promptOpen;
    // Pen stays highlighted while a custom prompt is armed (edited
    // and kept after collapsing the editor).
    penBtn.classList.toggle('active', boardStartState.promptOpen || boardStartState.prompt !== '');
    penBtn.title = boardStartState.promptOpen ? 'Hide the prompt editor' : 'Edit the prompt';
    if (boardStartState.promptOpen) {
        const text = boardStartState.prompt || boardStartPromptValue;
        promptInput.value = text;
        promptPreview.textContent = renderBoardStartPreview(boardStartState.item, text);
    }
}

// Client-side placeholder substitution for the popover's prompt
// PREVIEW only — the authoritative render happens server-side
// (TicketPrompt), so the {context} approximation here is cosmetic.
function renderBoardStartPreview(item, template) {
    const priority = item.priority || 'none';
    return String(template || '')
        .replaceAll('{id}', item.id)
        .replaceAll('{title}', item.title || '')
        .replaceAll('{description}', item.description || '')
        .replaceAll('{priority}', priority)
        .replaceAll('{context}', boardStartCombinedContext(item));
}

// Mirrors the server's ticketContextBlock: the ticket's explicit context
// followed by the activity-derived block (cosmetic preview only).
function boardStartCombinedContext(item) {
    const parts = [];
    const explicit = String(item.context || '').trim();
    if (explicit) parts.push(explicit);
    const log = boardStartContext(item.activity || []);
    if (log) parts.push(log);
    return parts.join('\n\n');
}

// Mirrors the server's activityContext: the content-bearing
// activity entries (comments, block reasons), skipping the
// generated status-transition noise.
function boardStartContext(activity) {
    const skip = new Set(['created', 'claimed', 'marked done']);
    const rows = [];
    for (const act of activity) {
        const text = String(act.text || '').trim();
        if (!text || skip.has(text) || text.startsWith('moved to ')) continue;
        rows.push(text.length > 300 ? text.slice(0, 300) + '…' : text);
    }
    const last = rows.slice(-5);
    if (last.length === 0) return '';
    return 'Ticket log context:\n' + last.map((e) => '- ' + e).join('\n');
}
