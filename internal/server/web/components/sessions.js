// Sidebar session list for the GoGen web UI: the ONE unified list of
// open panes + saved sessions in the sidebar. Owns the row building
// (state dots, message counts, relative time, close/delete buttons),
// the delete confirmation modal, and the resume/export actions. The
// last server payload is cached so pane-state changes (focus, busy,
// label) re-render without a round-trip.
//
// Wiring: app.js calls initSessions(deps) once at startup.
//   deps.getWs()                       — the chat WebSocket (or null)
//   deps.getPane()                     — the active pane
//   deps.getPanes()                    — the open-panes Map
//   deps.getNestedSessions()           — the live subagent-records Map
//   deps.nestedParentIdOf(id, list)    — a nested child's parent id
//   deps.nestedRowWillRender(...)      — nested-row render check
//   deps.collectNestedRows(id, depth)  — nested child records under a parent (pure, no DOM)
//   deps.buildNestedRow(rec)           — build one nested (subagent) row element
//   deps.relativeTime(value)           — "3m ago" formatting
//   deps.focusPane(key)                — focus an open pane
//   deps.openSessionPane(id)           — attach a saved session as a pane
//   deps.closePane(key)                — close an open pane
//   deps.ensureConnected()             — ws-usable guard (toasts on failure)
//   deps.cancelActiveTurn()            — interrupt the active pane's turn
//   deps.showToast(message, kind)      — the toast stack
//   deps.isResendAwaitingHistory()     — resend in progress (session-change guard)
//   deps.isPendingSessionResponse()    — a session change is in progress
//   deps.setPendingSessionResponse(v)  — set the pending flag
//   deps.getSessionInfoDiv()           — the #session-info element
//   deps.getMessagesDiv()              — the #messages element
//   deps.getMessageRawStore()          — the per-message raw-text WeakMap

import { openDialog } from '/components/dialog.js';
import { icon } from '/components/icons.js';

let deps = null;

// Last sessions payload from the server. The sidebar's single
// session list is re-rendered from this cache whenever pane state
// (active focus, busy/responding, label) changes, avoiding a
// round-trip per state flip.
let lastSessions = null;
// The sidebar session list element (looked up in initSessions).
let sessionListDiv = null;
// Keyed row cache for the diff render: row key → { el, sig }. The
// container is NEVER cleared — rows are created once, left untouched
// while their signature is unchanged, moved when their order changes,
// and removed when they leave the list. That keeps the 30s
// relative-time refresher's in-place .session-row-time updates (and
// scroll/hover state) alive between renders instead of rebuilding the
// whole list on every state flip.
// Invariant: only this module adds/removes sessionListDiv children
// (the 30s refresher touches row textContent only) — anything else that
// mutates the list would desync this map from the DOM.
let rowEls = new Map();
// The "No sessions" placeholder (created lazily, shown/hidden by the diff).
let emptyEl = null;

export function initSessions(d) {
    deps = d;
    sessionListDiv = document.getElementById('session-list');
}

// The cached last-sessions payload, exposed so app.js's ws handlers
// (session_removed pruning, subagent events) can read/update it.
export function getLastSessions() {
    return lastSessions;
}

export function setLastSessions(sessions) {
    lastSessions = sessions;
}

export function requestSessionList() {
    const ws = deps.getWs();
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type: 'list_sessions' }));
}

export function renderSessionList(sessions) {
    if (!sessionListDiv) return;
    // Cache the payload so pane-state changes (focus, busy, label)
    // can re-render the list without a server round-trip.
    lastSessions = sessions || [];
    const list = lastSessions;
    // ONE list of SESSIONS, not of panes. Rows come from the server's
    // saved-session payload (one row per top-level session); an open
    // pane overlays onto its session's row by id — focused/open/
    // responding are ATTRIBUTES of the row (dot + highlight), never
    // its position. Ordering is one key for every row: last OUTPUT
    // time = max(server updatedAt, pane.lastActivity). Activating or
    // focusing a session therefore never reorders the list; only new
    // output does. Ties keep the server's recency order (stable sort).
    // Panes missing from the payload (fresh id adoption / list lag)
    // and id-less "creating…" panes still get fallback rows so they
    // never disappear.
    const act = deps.getPane();
    const paneById = new Map();
    const idlessPanes = [];
    for (const pane of deps.getPanes().values()) {
        if (pane.id) paneById.set(pane.id, pane);
        else idlessPanes.push(pane);
    }
    const activityOf = (r) => Math.max(
        r.entry && r.entry.updatedAt ? (Date.parse(r.entry.updatedAt) || 0) : 0,
        r.pane && r.pane.lastActivity ? r.pane.lastActivity : 0,
    );
    const rows = [];
    for (const s of list) {
        // Nested (subagent) rows render under their parent below,
        // never as flat rows.
        if (s.parentId) continue;
        rows.push({ pane: paneById.get(s.id) || null, entry: s });
    }
    const seen = new Set(rows.map((r) => (r.entry ? r.entry.id : '')));
    for (const [id, pane] of paneById) {
        if (!seen.has(id)) rows.push({ pane, entry: null });
    }
    for (const pane of idlessPanes) rows.push({ pane, entry: null });
    rows.sort((a, b) => activityOf(b) - activityOf(a));
    // A nested (subagent) child open as a pane renders under its
    // parent via the nested descriptors below, NOT as a flat open-pane
    // row — opening a subagent must not make its row jump out of
    // the parent. The child falls back to its flat open-pane row
    // only when the parent's row is missing from this render (a
    // parent session still unknown to this tab / the store).
    const rowIds = new Set();
    for (const r of rows) rowIds.add(r.pane ? r.pane.id : (r.entry ? r.entry.id : ''));
    const flatRows = rows.filter((r) => {
        const id = r.pane ? r.pane.id : (r.entry ? r.entry.id : '');
        if (!r.pane || !id) return true;
        const parentId = deps.nestedParentIdOf(id, list);
        // Skip the flat row when the child's row renders nested
        // under a parent — including via a recursively nested
        // ancestor (depth >= 2). Only a missing ancestor chain
        // falls back to the flat open-pane row.
        return !parentId || !deps.nestedRowWillRender(parentId, list, rowIds, new Set());
    });
    // Build the ORDERED row descriptors: each flat row followed by its
    // nested (subagent) children, recursively. Nested rows join the same
    // list the diff places — the old code appended them to the container
    // end, which is only correct on a wiped list; a parent that reorders
    // must carry its children with it.
    const descriptors = [];
    for (const r of flatRows) {
        const id = r.pane ? r.pane.id : (r.entry ? r.entry.id : '');
        descriptors.push({
            key: r.pane ? (r.pane.id || 'pane:' + r.pane.key) : id,
            sig: flatRowSig(r.pane, r.entry, act),
            build: () => buildSessionRow(r.pane, r.entry, act),
        });
        if (id) {
            for (const rec of deps.collectNestedRows(id, 0)) {
                descriptors.push({
                    key: rec.id,
                    sig: nestedRowSig(rec),
                    build: () => deps.buildNestedRow(rec),
                });
            }
        }
    }
    if (!descriptors.length) {
        // Empty state: drop every row, show the placeholder.
        for (const rec of rowEls.values()) rec.el.remove();
        rowEls.clear();
        if (!emptyEl) {
            emptyEl = document.createElement('div');
            emptyEl.className = 'session-list-empty';
            emptyEl.textContent = 'No sessions';
        }
        sessionListDiv.appendChild(emptyEl);
        return;
    }
    if (emptyEl && emptyEl.parentNode === sessionListDiv) emptyEl.remove();
    // Diff pass 1: remove rows that left the list. Done BEFORE placement
    // so pass 2's appendChild is a no-op for rows already in the right
    // spot — a DOM move happens only for genuine reorders.
    const wanted = new Set(descriptors.map((d) => d.key));
    for (const [key, rec] of rowEls) {
        if (!wanted.has(key)) {
            rec.el.remove();
            rowEls.delete(key);
        }
    }
    // Diff pass 2: create rows that are new or whose signature changed,
    // then place every row in order (appendChild moves existing nodes).
    for (const d of descriptors) {
        let rec = rowEls.get(d.key);
        if (!rec || rec.sig !== d.sig) {
            if (rec) rec.el.remove(); // stale node out BEFORE the new one in
            rec = { el: d.build(), sig: d.sig };
            rowEls.set(d.key, rec);
        }
        sessionListDiv.appendChild(rec.el);
    }
}

// Signature of everything buildSessionRow displays: pane identity, live
// pane state, and the saved entry's fields. A row whose signature is
// unchanged is left completely untouched — its nodes (including the 30s
// refresher's .session-row-time updates) survive the re-render.
// MUST cover every field buildSessionRow reads; a missed field means a
// silently stale row.
function flatRowSig(pane, entry, act) {
    // Same fallback shape buildSessionRow uses for id-less/entry-less rows.
    const s = entry || {
        id: pane ? pane.id : '',
        label: pane ? pane.label : '',
        messageCount: null,
        active: true,
        updatedAt: '',
    };
    return [
        pane ? pane.key : '',
        pane ? (pane.id || '') : '',
        pane ? (pane.label || '') : '',
        pane ? (pane.turnActive ? 1 : 0) : '',
        pane ? (pane === act ? 1 : 0) : '',
        s.id,
        s.label,
        s.messageCount,
        s.updatedAt,
        s.active ? 1 : 0,
    ].join('\u0000');
}

// Signature of everything buildNestedSessionRow (app.js) displays: the
// child's open-pane overlay state plus the record's live/persisted
// fields. Same contract as flatRowSig: cover every field the builder
// reads (over-inclusion only costs a rare extra rebuild).
function nestedRowSig(rec) {
    const act = deps.getPane();
    let paneKey = '';
    let paneLabel = '';
    let paneTurn = '';
    let isCurrent = '';
    for (const pane of deps.getPanes().values()) {
        if (pane.id === rec.id) {
            paneKey = pane.key;
            paneLabel = pane.label || '';
            paneTurn = pane.turnActive ? 1 : 0;
            isCurrent = pane === act ? 1 : 0;
            break;
        }
    }
    return [
        paneKey,
        paneLabel,
        paneTurn,
        isCurrent,
        rec.running ? 1 : 0,
        rec.success ? 1 : 0,
        // A nested row's "interrupted" and "idle" states are neither failed nor
        // done, so each needs its own signature bit: without them a status
        // change from interrupted (or idle) to failed — the same success=0,
        // running=0, active=0 — would leave the stale label in place.
        rec.interrupted ? 1 : 0,
        rec.idle ? 1 : 0,
        rec.active ? 1 : 0,
        rec.label,
        (rec.summary || '').slice(0, 80),
        rec.job || '',
    ].join('\u0000');
}

// Build one sidebar row for an open pane (pane != null) or a saved
// session (pane == null). act is the active pane used to mark the
// current row; it is passed in so re-renders never re-derive it
// inconsistently with the caller's snapshot.
function buildSessionRow(pane, entry, act) {
    const s = entry || {
        id: pane ? pane.id : '',
        label: pane ? pane.label : '',
        messageCount: null,
        active: true,
        updatedAt: '',
    };
    const isActivePane = !!pane && pane === act;
    // A fresh session has an id but no label yet — show the
    // "New session" placeholder as its title until the first
    // turn derives a real label. The raw id stays available in
    // the row tooltip and in dataset.sessionId (delete/attach).
    const paneTitle = pane ? (pane.label || (entry && entry.label) || '') : '';
    const label = pane
        ? (paneTitle || 'New session…')
        : (s.label || s.id || '(unknown)');
    const row = document.createElement('div');
    row.className = 'session-row' + (isActivePane ? ' current' : '') + (pane && pane.turnActive ? ' busy' : '');
    row.dataset.sessionId = pane ? pane.id : (entry ? entry.id : '');
    row.title = !paneTitle && pane && pane.id ? `${label} (${pane.id})` : label;
    const content = document.createElement('div');
    content.className = 'session-row-content';
    const title = document.createElement('div');
    title.className = 'session-row-title';
    title.textContent = label;
    const meta = document.createElement('div');
    meta.className = 'session-row-meta';
    const frags = [];
    // Every pane state gets a uniform colored-dot indicator: the
    // dot carries the state color and the label stays muted. The
    // status indicator is pushed first so it always renders ahead
    // of the message count and relative time in the meta row.
    //   active              → green   (this pane is focused)
    //   responding          → amber   (a turn is running)
    //   creating…           → gray    (session id not assigned yet)
    //   open                → blue    (background pane in this tab)
    //   resume to continue  → violet  (live elsewhere / headless)
    let stateLabel = '';
    let stateClass = '';
    if (pane && pane.turnActive) {
        stateLabel = 'responding';
        stateClass = 'amber';
    } else if (pane && !pane.id) {
        stateLabel = 'creating…';
        stateClass = 'gray';
    } else if (isActivePane) {
        stateLabel = 'active';
        stateClass = 'green';
    } else if (pane && pane.id) {
        // Open as a background pane in this tab — clicking the
        // row focuses it.
        stateLabel = 'open';
        stateClass = 'blue';
    } else if (s.active) {
        // Live in-memory runtime elsewhere (another tab, or a
        // headless turn running): reopening attaches to it.
        stateLabel = 'resume to continue';
        stateClass = 'violet';
    }
    if (stateLabel) {
        const group = document.createElement('span');
        group.className = 'session-state';
        const dot = document.createElement('span');
        dot.className = 'session-state-dot ' + stateClass;
        dot.title = stateLabel;
        dot.setAttribute('aria-label', stateLabel);
        const label = document.createElement('span');
        label.className = 'session-state-label';
        label.textContent = stateLabel;
        group.appendChild(dot);
        group.appendChild(label);
        frags.push(group);
    }
    if (entry && entry.messageCount != null) frags.push(`${entry.messageCount} msgs`);
    const rel = deps.relativeTime(s.updatedAt);
    if (rel) {
        // Tag the relative time so the 30s tick can refresh it in
        // place (session rows are otherwise re-rendered only on
        // pane-state changes or new sessions payloads, leaving the
        // "3m ago" labels to go stale).
        const t = document.createElement('span');
        t.className = 'session-row-time';
        t.dataset.updated = s.updatedAt || '';
        t.textContent = rel;
        frags.push(t);
    }
    for (let i = 0; i < frags.length; i++) {
        if (i > 0) meta.appendChild(document.createTextNode(' · '));
        if (typeof frags[i] === 'string') {
            meta.appendChild(document.createTextNode(frags[i]));
        } else {
            meta.appendChild(frags[i]);
        }
    }
    content.appendChild(title);
    content.appendChild(meta);
    content.onclick = () => {
        if (pane) {
            // Open pane rows focus the pane (no-op when already
            // active).
            deps.focusPane(pane.key);
        } else {
            // Saved rows attach the session as a new pane.
            deps.openSessionPane(s.id);
        }
    };
    row.appendChild(content);
    // Close/delete button (hidden by default, shown on row
    // hover): an OPEN session's ✕ closes the pane — the session
    // is detached (it stays saved and its row reappears as a
    // saved session); a saved session's ✕ deletes it.
    const closeBtn = document.createElement('button');
    closeBtn.className = 'session-row-del';
    closeBtn.innerHTML = icon('x');
    if (pane) {
        closeBtn.title = 'Close session (stays saved)';
        closeBtn.onclick = (e) => {
            e.stopPropagation();
            deps.closePane(pane.key);
        };
    } else {
        closeBtn.title = 'Delete this session';
        closeBtn.onclick = (e) => {
            e.stopPropagation();
            deleteSession(s.id, s.label);
        };
    }
    row.appendChild(closeBtn);
    return row;
}

/**
 * Update the current session's row title in the sidebar without
 * re-rendering the entire list. The current session lives in the
 * unified session list (the active pane's row).
 */
export function updateCurrentSessionLabel(label) {
    if (!label || !sessionListDiv) return;
    const pane = deps.getPane();
    if (pane) pane.label = label;
    if (!pane || !pane.id) return;
    // Keep the cached server entry in sync so a later re-render
    // keeps the new label even before the server echoes it.
    if (lastSessions) {
        const entry = lastSessions.find((s) => s.id === pane.id);
        if (entry) entry.label = label;
    }
    const row = findSessionRow(pane.id);
    if (!row) return;
    if (row.classList.contains('nested')) {
        // Nested rows carry a job tooltip and a colored-dot state;
        // rebuild the row instead of overwriting the title with the
        // bare label.
        refreshSidebarSessions();
        return;
    }
    const titleEl = row.querySelector('.session-row-title');
    if (titleEl.textContent === label) return; // already up-to-date
    titleEl.textContent = label;
    row.title = label;
}

export function findSessionRow(id) {
    if (!sessionListDiv || !id) return null;
    for (const row of sessionListDiv.querySelectorAll('.session-row')) {
        if (row.dataset.sessionId === id) return row;
    }
    return null;
}

// Re-render the sidebar session list from the cached payload. Used
// when pane state the list shows (focus, busy/responding, label)
// changes without a server round-trip; falls back to asking the
// server when no payload has arrived yet.
export function refreshSidebarSessions() {
    if (lastSessions) {
        renderSessionList(lastSessions);
    } else {
        requestSessionList();
    }
}

export async function deleteSession(id, label) {
    if (!deps.ensureConnected()) return;
    if (!id) return;
    if (deps.isResendAwaitingHistory()) {
        deps.showToast('Resend already in progress', 'info');
        return;
    }
    if (deps.isPendingSessionResponse()) {
        deps.showToast('Session change already in progress', 'info');
        return;
    }
    const displayName = label || id;
    const confirmed = await showSessionDeleteModal(displayName);
    if (!confirmed) return;
    // The modal await above yields to the event loop for as long as the
    // user takes to decide, so the socket may have closed in the meantime.
    // Re-check BEFORE mutating anything: send() on a non-open socket throws
    // an uncaught InvalidStateError, and setting the pending flag first
    // would block the next session change ("Session change already in
    // progress") until the next reconnect/turn_end cleared it. This check
    // is airtight, not just best-effort: everything from here to the send
    // below runs synchronously in one microtask, and the socket's
    // readyState can only change while a task runs (the close event, a
    // reconnect replacing the socket) — neither can preempt this block —
    // so OPEN here still holds at the send.
    if (!deps.ensureConnected()) return;
    // Deleting the active pane's session interrupts its turn; a
    // background session's delete leaves the active turn running.
    const active = deps.getPane();
    if (active && active.id === id) {
        deps.cancelActiveTurn();
    }
    deps.setPendingSessionResponse(true);
    deps.getWs().send(JSON.stringify({ type: 'session_delete', sessionId: id }));
    // A deleted nested (subagent) child must stop rendering under
    // its parent: drop its live-event record AND the cached payload
    // entry now (a closed child has no attached pane, so no
    // session_removed broadcast comes back to this tab; the server
    // still reports the deletion to the parent agent, and the
    // fresh list round-trip after the reply confirms the purge).
    if (deps.getNestedSessions().delete(id)) {
        if (lastSessions) {
            const idx = lastSessions.findIndex((s) => s.id === id);
            if (idx >= 0) lastSessions.splice(idx, 1);
        }
        refreshSidebarSessions();
    }
}

// Custom confirm modal for session deletion (matches the other dialogs).
// Esc cancels; the safe default (Cancel) is focused on open.
function showSessionDeleteModal(displayName) {
    return new Promise((resolve) => {
        const overlay = document.getElementById('session-delete-overlay');
        const filenameEl = document.getElementById('session-delete-filename');
        if (!overlay) { resolve(window.confirm(`Delete session "${displayName}"? This cannot be undone.`)); return; }
        filenameEl.textContent = `Session "${displayName}" and its message history will be permanently deleted.`;
        // Delete = confirm (the destructive action), Cancel = cancel; Esc
        // and a backdrop click cancel. openDialog wires buttons, Esc,
        // backdrop and the teardown; the safe default (Cancel) is focused
        // on open (first focusable, via openModal).
        openDialog(overlay, {
            confirm: 'session-delete-confirm-btn',
            cancel: 'session-delete-cancel-btn',
            onConfirm: () => resolve(true),
            onCancel: () => resolve(false),
        });
    });
}

export function resumeSession(id) {
    if (!deps.ensureConnected()) return;
    if (!id) return;
    if (deps.isResendAwaitingHistory()) {
        deps.showToast('Resend already in progress', 'info');
        return;
    }
    if (deps.isPendingSessionResponse()) {
        deps.showToast('Session change already in progress', 'info');
        return;
    }
    // The old session's turn keeps running headless (continuation
    // design — /new behaves the same): resume only re-keys this
    // pane, it does NOT cancel the turn. Record the old session id
    // so its turn_end (which may arrive before the reply re-keys the
    // pane) cannot clear our pending flag mid-resume or block the
    // resumed session's convergence; once the reply re-keys the
    // pane, the old session's events are dropped by paneForMessage.
    const p = deps.getPane();
    if (p && p.id) p.ignoreTurnEndsFor = p.id;
    deps.setPendingSessionResponse(true);
    deps.getWs().send(JSON.stringify({ type: 'session_resume', sessionId: id }));
}

export function exportChat() {
    const lines = [];
    const pane = deps.getPane();
    const sessionId = (deps.getSessionInfoDiv().textContent || 'session').replace(/[^\w.-]+/g, '_');
    const label = (pane && pane.label) || '';
    const date = new Date().toISOString().slice(0, 10);
    lines.push(`# GoGen chat${label ? ` — ${label}` : ''} (${sessionId})`);
    lines.push(`_Exported ${new Date().toLocaleString()}_`);
    lines.push('');
    for (const el of deps.getMessagesDiv().querySelectorAll('.message, .tool-card')) {
        appendExportEntry(lines, el);
    }
    const labelSlug = label.toLowerCase().replace(/[^\w.-]+/g, '_').replace(/^_+|_+$/g, '');
    const base = labelSlug ? `${labelSlug}-${sessionId}` : sessionId;
    const blob = new Blob([lines.join('\n')], { type: 'text/markdown;charset=utf-8' });
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = `gogen-chat-${base}-${date}.md`;
    a.click();
    URL.revokeObjectURL(a.href);
    deps.showToast('Chat exported', 'success');
}

// Append the markdown lines for one chat element (a tool call card or
// a user/assistant message) to the export buffer. Returns false when
// the element is skipped (system/thinking/thought cards, or an
// element with no exportable body).
function appendExportEntry(lines, el) {
    if (el.classList.contains('system') || el.classList.contains('thinking') || el.classList.contains('thinking-block')) {
        return false;
    }
    if (el.classList.contains('thought-card')) return false;
    if (el.classList.contains('tool-card')) {
        // Tool call card: name, args, result, and (for patch_file /
        // show_diff) the raw diff text from the fallback <pre>.
        const nameEl = el.querySelector('.tool-name');
        const name = nameEl ? nameEl.textContent.trim() : 'tool';
        const statusEl = el.querySelector('.tool-status-bar');
        const status = statusEl ? statusEl.textContent.trim() : '';
        const argsText = (el.querySelector('.tool-args')?.textContent || '').trim();
        let resultText = (el.querySelector('.tool-result-body')?.textContent || '').trim();
        // The status bar is a child of the result body; drop its
        // label from the text so it is not duplicated.
        if (status && resultText.startsWith(status)) {
            resultText = resultText.slice(status.length).trim();
        }
        const copyLabel = el.querySelector('.tool-result-copy')?.textContent || '';
        if (copyLabel && resultText.startsWith(copyLabel)) {
            resultText = resultText.slice(copyLabel.length).trim();
        }
        const diffText = (el.querySelector('.monaco-tool-host .diff-fallback')?.textContent || '').trim();
        if (!argsText && !resultText && !diffText) return false;
        lines.push(`### tool: ${name}${status ? ` — ${status}` : ''}`);
        if (argsText) {
            lines.push('');
            lines.push('```');
            lines.push(argsText);
            lines.push('```');
        }
        if (diffText) {
            lines.push('');
            lines.push('```diff');
            lines.push(diffText);
            lines.push('```');
        }
        if (resultText) {
            lines.push('');
            lines.push(resultText);
        }
        lines.push('');
        return true;
    }
    const role = el.classList.contains('user') ? 'user'
        : el.classList.contains('assistant') ? 'assistant' : null;
    if (!role) return false;
    const raw = deps.getMessageRawStore().get(el);
    const body = (raw != null ? raw : el.textContent || '').trim();
    if (!body) return false;
    const ts = el.dataset.createdAt ? new Date(el.dataset.createdAt) : null;
    const stamp = (ts && !Number.isNaN(ts.getTime())) ? ` — ${ts.toLocaleString()}` : '';
    lines.push(`## ${role}${stamp}`);
    lines.push('');
    lines.push(body);
    lines.push('');
    return true;
}
