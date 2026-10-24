// Approval queue + confirm modal for the GoGen web UI. Handles both file
// deletes (delete_approval) and dangerous shell commands the guard
// escalated to the user (command_approval).
//
// The server asks for approval before the agent deletes files or runs a
// blocked command. The modal is single-slot, so requests are QUEUED: a second session's
// approval request (e.g. a background pane) cannot orphan the first —
// overwriting the pending id would leave the first session's turn
// waiting forever on a channel that is never resolved. Each entry
// carries its sessionId so the response routes to the right session's
// runtime.
//
// While an approval is pending the composer is disabled (inputArea /
// send-btn) so the user cannot race the decision with a new turn.
//
// Wiring: app.js calls initDeleteApproval(deps) once at startup and
// forwards the ws 'delete_approval' payload to showDeleteApproval.
//   deps.getWs() — the chat WebSocket (or null)
import { openDialog } from '/components/dialog.js';
import { sendNotification } from '/components/settings.js';

const inputArea = document.getElementById('message-input');
const sendBtn = document.getElementById('send-btn');
const deleteOverlay = document.getElementById('delete-approval-overlay');
const approvalTitle = document.getElementById('delete-approval-title');
const deleteReason = document.getElementById('delete-approval-reason');
const deleteCommand = document.getElementById('delete-approval-command');
const deletePaths = document.getElementById('delete-approval-paths');
const allowBtn = document.getElementById('delete-allow-btn');

let deps = null;

export function initDeleteApproval(d) {
    deps = d;
}

let pendingDeleteApprovals = []; // {kind, approvalId, sessionId, reason, paths, command}

// renderApproval renders one queued request. kind selects the wording:
// "command" shows the blocked shell command the guard escalated for a
// decision; anything else is the historical file-delete approval.
function renderApproval(data) {
    const isCommand = data.kind === 'command';
    approvalTitle.textContent = isCommand ? 'Approve command execution?' : 'Approve file delete?';
    allowBtn.textContent = isCommand ? 'Allow command' : 'Allow delete';
    deleteCommand.style.display = isCommand ? '' : 'none';
    deletePaths.style.display = isCommand ? 'none' : '';
    if (isCommand) {
        deleteCommand.textContent = data.command || '';
        deleteReason.textContent = data.reason ? `Matched safety pattern: ${data.reason}` : 'This command matched a dangerous-command pattern.';
    } else {
        deleteReason.textContent = data.reason ? `Requested by: ${data.reason}` : 'The agent wants to delete files.';
        deletePaths.textContent = (data.paths || []).map(p => `- ${p}`).join('\n');
    }
}

// Resolve the front queued approval (Allow/Deny click or Esc — openDialog
// routes all three to the same callbacks): send the response, then present
// the next queued approval or hand the composer back. By the time we run,
// openDialog has already torn down its wiring and closed the overlay, so
// presenting the next approval re-arms everything from scratch (no stale
// handlers) and the last resolution just leaves it closed.
function resolveDeleteApproval(approved) {
    const ws = deps.getWs();
    if (!pendingDeleteApprovals.length || !ws || ws.readyState !== WebSocket.OPEN) {
        pendingDeleteApprovals = [];
        inputArea.disabled = false;
        sendBtn.disabled = false;
        return;
    }
    const current = pendingDeleteApprovals.shift();
    const responseType = current.kind === 'command' ? 'command_approval_response' : 'delete_approval_response';
    ws.send(JSON.stringify({
        type: responseType,
        approvalId: current.approvalId,
        approved: approved,
        sessionId: current.sessionId || undefined
    }));
    if (pendingDeleteApprovals.length) {
        // More approvals queued — show the next one.
        presentDeleteApproval();
    } else {
        inputArea.disabled = false;
        sendBtn.disabled = false;
    }
}

// Present the front queued approval: render it, freeze the composer and
// arm the dialog plumbing (Allow/Deny buttons + Esc). openDialog closes
// the overlay on every dismissal path before invoking the callbacks, so
// this runs again — freshly armed — for each queued approval. Backdrop
// clicks are NOT wired (backdrop: false): a stray outside click must not
// deny a delete the user meant to allow.
function presentDeleteApproval() {
    renderApproval(pendingDeleteApprovals[0]);
    inputArea.disabled = true;
    sendBtn.disabled = true;
    openDialog(deleteOverlay, {
        // Allow = confirm; Deny = cancel (Esc denies too — the safe action).
        confirm: 'delete-allow-btn',
        cancel: 'delete-deny-btn',
        backdrop: false,
        onConfirm: () => resolveDeleteApproval(true),
        onCancel: () => resolveDeleteApproval(false),
    });
}

// enqueueApproval pushes one request onto the shared queue and, when it is
// the front, presents it. A non-front request renders once the current one
// resolves (the overlay is single-slot).
function enqueueApproval(entry, body, tag) {
    const first = pendingDeleteApprovals.length === 0;
    pendingDeleteApprovals.push(entry);
    if (!first) {
        return;
    }
    presentDeleteApproval();
    // Always notify: an approval requires user action even when the tab
    // is backgrounded.
    sendNotification('GoGen — Approval needed', body, tag);
}

export function showDeleteApproval(data) {
    enqueueApproval({
        kind: 'delete',
        approvalId: data.approvalId,
        sessionId: data.sessionId || null,
        reason: data.reason,
        paths: data.paths || [],
    }, `File delete requested: ${(data.paths || []).join(', ')}`, 'gogen-delete-approval');
}

// showCommandApproval queues a blocked-command approval: the guard escalates
// a dangerous-pattern match to the user instead of hard-blocking it.
export function showCommandApproval(data) {
    enqueueApproval({
        kind: 'command',
        approvalId: data.approvalId,
        sessionId: data.sessionId || null,
        reason: data.reason,
        command: data.command || '',
    }, `Command needs approval: ${data.command || ''}`, 'gogen-command-approval');
}
