// Standalone harness for the reported symptom: switching panes in the
// sidebar session list while a turn is running on the ORIGINAL session.
// The pane-state functions below are copied VERBATIM from app.js (the
// multi-pane code) with DOM interactions routed to stubs, so the
// exact state machine the browser runs can be exercised deterministically.
// Run: node scripts/test_paneswitch.js
'use strict';

// ── minimal DOM stubs ─────────────────────────────────────────────────────
function makeEl(tag) {
  const el = {
    tagName: (tag || 'div').toUpperCase(),
    children: [],
    style: {},
    dataset: {},
    classSet: new Set(),
    _text: '',
    disabled: false,
    value: '',
    title: '',
    className: '',
    listeners: {},
    get classList() {
      const self = this;
      return {
        add: (...c) => c.forEach((x) => self.classSet.add(x)),
        remove: (...c) => c.forEach((x) => self.classSet.delete(x)),
        toggle: (c, force) => {
          const on = force === undefined ? !self.classSet.has(c) : !!force;
          if (on) self.classSet.add(c); else self.classSet.delete(c);
          return on;
        },
        contains: (c) => self.classSet.has(c),
      };
    },
    set textContent(v) { this._text = String(v); },
    get textContent() { return this._text; },
    set innerHTML(v) { this._text = String(v); this.children = []; },
    get innerHTML() { return this._text; },
    appendChild(c) { this.children.push(c); return c; },
    setAttribute(k, v) { this[k] = String(v); },
    prepend(c) { this.children.unshift(c); return c; },
    addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); },
    removeEventListener(t, fn) {
      const l = this.listeners[t] || [];
      const i = l.indexOf(fn);
      if (i >= 0) l.splice(i, 1);
    },
    focus() {},
    querySelector() { return null; },
    querySelectorAll() { return []; },
  };
  return el;
}

const els = {};
function getEl(id) {
  if (!els[id]) els[id] = makeEl('div');
  return els[id];
}
const documentStub = {
  getElementById: (id) => getEl(id),
  createElement: (tag) => makeEl(tag),
  addEventListener() {},
  querySelectorAll() { return []; },
};
const messagesDiv = getEl('messages');
const inputArea = getEl('input-area');
const inputProgress = getEl('input-progress');
const sessionInfoDiv = getEl('session-info');
const sessionListDiv = getEl('session-list');

let turnActive = false;
let _prevTurnActive = false;
let suppressTurnEnds = 0;
let pendingSessionResponse = false;
let pendingResendContent = null;
let resendAwaitingHistory = false;
let compacting = false;
let lastContextData = null;
let contextBaseUsed = 0;
let contextLimit = 0;
let contextEstAdded = 0;
let toolsStartedThisTurn = false;
let currentStreamDiv = null;
let currentStreamRaw = '';
let streamRafPending = false;
let streamLastRender = 0;
let currentThinkingDiv = null;
let currentThinkingSpan = null;
let currentThinkingRaw = '';
let thinkingRafPending = false;
let thinkingLastRender = 0;
let streamingToolCards = {};
let pendingToolCards = {};
let historyToolCallArgs = {};
// Last sessions payload from the server (cache for the unified sidebar
// session list — app.js renderSessionList).
let lastSessions = null;

// ── copied verbatim from app.js ──────────────────────────────────────────
function setTurnActive(active, opts) {
  const silent = !!(opts && opts.silent);
  const noMirror = !!(opts && opts.noMirror);
  const wasActive = _prevTurnActive;
  turnActive = !!active;
  if (getEl('cancel-btn')) getEl('cancel-btn').disabled = !turnActive;
  if (getEl('send-btn')) getEl('send-btn').disabled = false;
  getEl('input-area').classList.toggle('turn-active', turnActive);
  if (!active) {
    toolsStartedThisTurn = false;
  }
  if (!noMirror) {
    const p = activePane();
    if (p) p.turnActive = turnActive;
  }
  if (!noMirror && wasActive !== turnActive) {
    refreshSidebarSessions();
  }
  _prevTurnActive = turnActive;
  if (wasActive && !turnActive && suppressTurnEnds === 0 && !silent) {
    // sendNotification(...) omitted
  }
}

const panes = new Map(); // paneKey -> pane
let nextPaneKey = 0;
let activePaneKey = 0;

function makePane() {
  const pane = {
    key: nextPaneKey++,
    id: null,
    label: '',
    turnActive: false,
    needsFreshHistory: false,
    ignoreTurnEndsFor: null,
    suppressTurnEnds: 0,
    pendingSessionResponse: false,
    pendingResendContent: null,
    resendAwaitingHistory: false,
    compacting: false,
    lastContextData: null,
    contextBaseUsed: 0,
    contextLimit: 0,
    contextEstAdded: 0,
    mode: 'act',
    thinkingLevel: 'off',
    model: '',
    _notifiedBusy: false,
  };
  panes.set(pane.key, pane);
  refreshSidebarSessions();
  return pane;
}

function activePane() {
  return panes.get(activePaneKey);
}

function findPaneBySession(id) {
  if (!id) return null;
  for (const pane of panes.values()) {
    if (pane.id === id) return pane;
  }
  return null;
}

function paneForMessage(data) {
  const sid = data.sessionId;
  if (sid) {
    const act = activePane();
    const activeInFlight = !!(act && pendingSessionResponse);
    if (data.type === 'response' && data.sessionAction) {
      if (activeInFlight) return act;
      for (const pane of panes.values()) {
        if (pane !== act && pane.pendingSessionResponse) return pane;
      }
      return act;
    }
    const p = findPaneBySession(sid);
    if (p) return p;
    if (activeInFlight) return act;
    for (const pane of panes.values()) {
      if (pane !== act && pane.pendingSessionResponse) return pane;
    }
    if (act && act.id === null) return act;
    return null;
  }
  return activePane();
}

function saveActivePaneState() {
  const pane = activePane();
  if (!pane) return;
  pane.turnActive = turnActive;
  pane.suppressTurnEnds = suppressTurnEnds;
  pane.pendingSessionResponse = pendingSessionResponse;
  pane.pendingResendContent = pendingResendContent;
  pane.resendAwaitingHistory = resendAwaitingHistory;
  pane.compacting = compacting;
  pane.lastContextData = lastContextData;
  pane.contextBaseUsed = contextBaseUsed;
  pane.contextLimit = contextLimit;
  pane.contextEstAdded = contextEstAdded;
}

function loadActivePaneState() {
  const pane = activePane();
  if (!pane) return;
  turnActive = !!pane.turnActive;
  suppressTurnEnds = pane.suppressTurnEnds;
  pendingSessionResponse = pane.pendingSessionResponse;
  pendingResendContent = pane.pendingResendContent;
  resendAwaitingHistory = pane.resendAwaitingHistory;
  compacting = pane.compacting;
  lastContextData = pane.lastContextData;
  contextBaseUsed = pane.contextBaseUsed;
  contextLimit = pane.contextLimit;
  contextEstAdded = pane.contextEstAdded;
  _prevTurnActive = turnActive;
  if (getEl('cancel-btn')) getEl('cancel-btn').disabled = !turnActive;
  getEl('input-area').classList.toggle('turn-active', turnActive);
  if (!turnActive) toolsStartedThisTurn = false;
}

function clearChat() {
  messagesDiv.innerHTML = '';
  streamRafPending = false;
  streamLastRender = 0;
  currentStreamDiv = null;
  currentStreamRaw = '';
  currentThinkingDiv = null;
  currentThinkingSpan = null;
  currentThinkingRaw = '';
  thinkingRafPending = false;
  streamingToolCards = {};
  pendingToolCards = {};
  historyToolCallArgs = {};
  toolsStartedThisTurn = false;
  setTurnActive(false, { silent: true, noMirror: true });
}

function handleBackgroundMessage(pane, data) {
  const beforeTurn = pane.turnActive;
  const beforeLabel = pane.label;
  const beforeId = pane.id;
  switch (data.type) {
    case 'thinking':
    case 'waiting':
    case 'thinking_token':
    case 'stream':
    case 'stream_end':
    case 'tool_call_start':
    case 'tool_call_delta':
    case 'tool_call':
    case 'tool_execute':
    case 'tool_result':
    case 'term_opened':
    case 'term_output':
    case 'term_exit':
      pane.turnActive = true;
      break;
    case 'turn_end':
    case 'cancelled':
      pane.turnActive = false;
      if (pane.suppressTurnEnds > 0) pane.suppressTurnEnds--;
      break;
    case 'session_state':
      pane.turnActive = !!data.turnActive;
      pane.needsFreshHistory = !!pane.turnActive;
      break;
    case 'context':
      pane.lastContextData = data || pane.lastContextData;
      if (data.sessionLabel) pane.label = data.sessionLabel;
      if (data.usedSource !== 'estimated') {
        pane.contextBaseUsed = data.usedTokens || 0;
        pane.contextLimit = data.contextLimit || 0;
        pane.contextEstAdded = 0;
      }
      break;
    case 'config':
      pane.mode = data.mode || pane.mode;
      pane.thinkingLevel = data.thinkingLevel || pane.thinkingLevel;
      pane.model = data.model || pane.model;
      if (data.sessionLabel) pane.label = data.sessionLabel;
      break;
    case 'clear_chat':
      pane.pendingSessionResponse = false;
      break;
    case 'response':
      pane.pendingSessionResponse = false;
      pane.ignoreTurnEndsFor = null;
      if (data.sessionAction && data.sessionId && pane.id !== data.sessionId) {
        const other = findPaneBySession(data.sessionId);
        if (!other || other === pane) {
          const oldId = pane.id;
          pane.id = data.sessionId;
          if (oldId && !findPaneBySession(oldId) && wsStub && wsStub.readyState === 1) {
            wsStub.sent.push({ type: 'session_detach', sessionId: oldId });
          }
        }
      }
      break;
    case 'history':
      break;
    case 'delete_approval':
      break;
    default:
      break;
  }
  if (pane.turnActive !== beforeTurn || pane.label !== beforeLabel || pane.id !== beforeId) {
    refreshSidebarSessions();
  }
}

function focusPane(key) {
  if (key === activePaneKey) return;
  const pane = panes.get(key);
  if (!pane) return;
  saveActivePaneState();
  activePaneKey = key;
  clearChat();
  loadActivePaneState();
  if (sessionInfoDiv) sessionInfoDiv.textContent = pane.id || '';
  refreshSidebarSessions();
  if (pane.id && wsStub && wsStub.readyState === 1) {
    wsStub.sent.push({ type: 'session_attach', sessionId: pane.id });
  }
}

// renderSessionList as in app.js: one unified session list. Open panes
// (client state) are merged in FIRST so an open session's row always exists
// with its live label; saved sessions follow in server order. The focused
// pane's session is marked "current" and a running pane's session "busy".
function renderSessionList(list) {
  lastSessions = list || [];
  sessionListDiv.innerHTML = '';
  const act = activePane();
  const rows = [];
  const openIds = new Set();
  for (const pane of panes.values()) {
    const entry = pane.id ? (lastSessions.find((s) => s.id === pane.id) || null) : null;
    if (pane.id) openIds.add(pane.id);
    rows.push({ pane, entry });
  }
  for (const s of lastSessions) {
    // Nested (subagent) rows render under their parent (appendNestedRows
    // in app.js), never as flat rows.
    if (s.parentId) continue;
    if (!openIds.has(s.id)) rows.push({ pane: null, entry: s });
  }
  // A nested (subagent) child open as a pane renders under its parent, not
  // as a flat open-pane row; it falls back to a flat row only when the
  // parent's row is missing from this render.
  const rowIds = new Set();
  for (const r of rows) rowIds.add(r.pane ? r.pane.id : (r.entry ? r.entry.id : ''));
  const flatRows = rows.filter((r) => {
    const id = r.pane ? r.pane.id : (r.entry ? r.entry.id : '');
    if (!r.pane || !id) return true;
    const entry = lastSessions.find((s) => s.id === id);
    const parentId = (entry && entry.parentId) || '';
    return !parentId || !rowIds.has(parentId);
  });
  for (const r of flatRows) {
    const pane = r.pane;
    const entry = r.entry;
    const s = entry || {
      id: pane ? pane.id : '',
      label: pane ? pane.label : '',
      messageCount: null,
      active: true,
      updatedAt: '',
    };
    const isActivePane = !!pane && pane === act;
    const label = pane
      ? (pane.label || (entry && entry.label) || pane.id || 'New session…')
      : (s.label || s.id || '(unknown)');
    const row = makeEl('div');
    row.className = 'session-row' + (isActivePane ? ' current' : '') + (pane && pane.turnActive ? ' busy' : '');
    row.dataset.sessionId = pane ? pane.id : (entry ? entry.id : '');
    row.title = label;
    const content = makeEl('div');
    content.className = 'session-row-content';
    const title = makeEl('div');
    title.className = 'session-row-title';
    title.textContent = label;
    const meta = makeEl('div');
    meta.className = 'session-row-meta';
    // Uniform colored-dot indicator per state (matches app.js): the dot's
    // class carries the state color; the label text renders next to it.
    const parts = [];
    let stateDot = '';
    if (entry && entry.messageCount != null) parts.push(`${entry.messageCount} msgs`);
    if (pane && pane.turnActive) {
      parts.push('responding'); stateDot = 'amber';
    } else if (pane && !pane.id) {
      parts.push('creating…'); stateDot = 'gray';
    } else if (isActivePane) {
      parts.push('active'); stateDot = 'green';
    } else if (pane && pane.id) {
      // Open as a background pane in this tab — clicking the row focuses
      // it. Uniform indicator (dot + "open" label).
      parts.push('open'); stateDot = 'blue';
    } else if (s.active) {
      parts.push('resume to continue');
    }
    const rel = (typeof relativeTime === 'function') ? relativeTime(s.updatedAt) : null;
    if (rel) parts.push(rel);
    meta.textContent = parts.join(' · ');
    if (stateDot) {
      const group = makeEl('span');
      group.className = 'session-state';
      const dot = makeEl('span');
      dot.className = 'session-state-dot ' + stateDot;
      dot.setAttribute('aria-label', stateDot);
      group.appendChild(dot);
      meta.appendChild(group);
    }
    content.appendChild(title);
    content.appendChild(meta);
    content.onclick = () => {
      if (pane) {
        focusPane(pane.key);
      } else {
        openSessionPane(s.id);
      }
    };
    row.appendChild(content);
    sessionListDiv.appendChild(row);
  }
}

// refreshSidebarSessions as in app.js: re-render from the cached payload or
// (no payload yet) render nothing.
function refreshSidebarSessions() {
  if (lastSessions) {
    renderSessionList(lastSessions);
  } else {
    lastSessions = [];
  }
}

function openSessionPane(id) {
  if (!wsStub || wsStub.readyState !== 1) return;
  if (!id) return;
  const existing = findPaneBySession(id);
  if (existing) {
    focusPane(existing.key);
    return;
  }
  saveActivePaneState();
  const pane = makePane();
  pane.id = id;
  activePaneKey = pane.key;
  refreshSidebarSessions();
  clearChat();
  loadActivePaneState();
  if (sessionInfoDiv) sessionInfoDiv.textContent = id;
  pendingSessionResponse = false;
  wsStub.sent.push({ type: 'session_attach', sessionId: id });
}

function newSession() {
  if (!wsStub || wsStub.readyState !== 1) return;
  if (pendingSessionResponse) return;
  saveActivePaneState();
  const pane = makePane();
  activePaneKey = pane.key;
  clearChat();
  loadActivePaneState();
  pendingSessionResponse = true;
  wsStub.sent.push({ type: 'session_new' });
}
// ── end copy ─────────────────────────────────────────────────────────────

const wsStub = { readyState: 1, sent: [] };

// ── the ws.onmessage dispatcher (relevant branches, verbatim from app.js) ──
function dispatch(data) {
  const msgPane = paneForMessage(data);
  if (!msgPane) return 'dropped';
  if (msgPane !== activePane()) {
    handleBackgroundMessage(msgPane, data);
    return 'background:' + (msgPane.label || msgPane.id || msgPane.key);
  }
  if (data.type === 'thinking') {
    setTurnActive(true);
    toolsStartedThisTurn = false;
    streamingToolCards = {};
  } else if (data.type === 'waiting') {
    setTurnActive(true);
  } else if (data.type === 'thinking_token') {
    setTurnActive(true);
  } else if (data.type === 'stream') {
    setTurnActive(true);
    if (!currentStreamDiv) { currentStreamDiv = makeEl('div'); currentStreamRaw = ''; }
    currentStreamRaw += data.content || '';
  } else if (data.type === 'stream_end') {
    currentStreamDiv = null;
    currentStreamRaw = '';
  } else if (data.type === 'tool_call_start') {
    setTurnActive(true);
    streamingToolCards[data.index] = true;
  } else if (data.type === 'tool_call_delta') {
    if (!streamingToolCards[data.index]) streamingToolCards[data.index] = true;
  } else if (data.type === 'tool_call') {
    streamingToolCards[data.index] = true;
    toolsStartedThisTurn = true;
  } else if (data.type === 'tool_execute') {
  } else if (data.type === 'tool_result') {
  } else if (data.type === 'cancelled') {
    if (suppressTurnEnds > 0) return 'cancelled-suppressed';
    setTurnActive(false);
    refreshSidebarSessions();
  } else if (data.type === 'turn_end') {
    if (suppressTurnEnds > 0) {
      suppressTurnEnds--;
      return 'turn_end-suppressed';
    }
    const pane = activePane();
    if (pane && pane.ignoreTurnEndsFor && data.sessionId === pane.ignoreTurnEndsFor) {
      pane.ignoreTurnEndsFor = null;
      return 'turn_end-ignored';
    }
    setTurnActive(false);
    pendingSessionResponse = false;
    const tp = pane;
    if (tp && tp.needsFreshHistory) {
      tp.needsFreshHistory = false;
      if (tp.id && wsStub && wsStub.readyState === 1) {
        wsStub.sent.push({ type: 'session_attach', sessionId: tp.id });
      }
    }
    refreshSidebarSessions();
  } else if (data.type === 'clear_chat') {
    clearChat();
  } else if (data.type === 'session_state') {
    const pane = activePane();
    if (data.sessionId && pane.id === null) {
      pane.id = data.sessionId;
    }
    pane.turnActive = !!data.turnActive;
    setTurnActive(pane.turnActive, { silent: true });
    if (pane.turnActive) {
      pane.needsFreshHistory = true;
    } else {
      pane.needsFreshHistory = false;
    }
    refreshSidebarSessions();
  } else if (data.type === 'sessions') {
    renderSessionList(data.sessions || []);
  } else if (data.type === 'history') {
    const histPane = activePane();
    if (histPane.needsFreshHistory || !turnActive) {
      // Mid-turn attach: the snapshot lacks the in-flight reply, and live
      // events for it may already be rendering (the attach's deep-cloned
      // snapshot can arrive after the first stream batches for a large
      // session). Rebuilding would WIPE the visible reply — the turn seems
      // to stop until the turn_end refetch. Keep the live content.
      if (histPane.needsFreshHistory
        && (currentStreamDiv
          || currentThinkingDiv
          || Object.keys(streamingToolCards).length > 0)) {
        return 'history-skipped-live';
      }
      clearChat();
      const pane = activePane();
      if (pane && pane.turnActive) {
        setTurnActive(true, { silent: true });
      }
    }
  } else if (data.type === 'config') {
    const pane = activePane();
    const oldId = pane.id;
    if (data.sessionId && pane.id !== data.sessionId) {
      if (pane.id === null || pendingSessionResponse) {
        pane.id = data.sessionId;
        pane.needsFreshHistory = false;
        refreshSidebarSessions();
        if (oldId && !findPaneBySession(oldId) && wsStub && wsStub.readyState === 1) {
          wsStub.sent.push({ type: 'session_detach', sessionId: oldId });
        }
      }
    }
    pane.mode = data.mode || pane.mode;
    pane.thinkingLevel = data.thinkingLevel || pane.thinkingLevel;
    pane.model = data.model || pane.model;
    if (data.sessionLabel) pane.label = data.sessionLabel;
  } else if (data.type === 'response') {
    const rp = activePane();
    let rekeySkipped = false;
    if (rp && data.sessionId && rp.id !== data.sessionId) {
      const other = findPaneBySession(data.sessionId);
      if ((rp.id === null || data.sessionAction) && (!other || other === rp)) {
        const oldId = rp.id;
        rp.id = data.sessionId;
        refreshSidebarSessions();
        if (oldId && !findPaneBySession(oldId) && wsStub && wsStub.readyState === 1) {
          wsStub.sent.push({ type: 'session_detach', sessionId: oldId });
        }
      } else {
        rekeySkipped = true;
      }
    }
    rp.ignoreTurnEndsFor = null;
    if (pendingSessionResponse) {
      pendingSessionResponse = false;
    }
  } else if (data.type === 'context') {
    lastContextData = data || lastContextData;
  }
  return 'active';
}

// ── row snapshot helper ──────────────────────────────────────────────────
function rows() {
  return sessionListDiv.children
    .filter((c) => c.tagName === 'DIV' && c.children.length > 0 && c.children[0] && c.children[0].children)
    .map((row) => {
      const content = row.children[0];
      const title = content.children[0];
      const meta = content.children[1];
      // The state dot lives inside a .session-state group (app.js wraps
      // dot + label); search descendants for the colored dot span.
      function hasStateDot(el) {
        if (!el || !el.children) return false;
        if (el.children.some((c) => String(c.className || '').startsWith('session-state-dot'))) return true;
        return el.children.some((c) => hasStateDot(c));
      }
      return {
        key: row.title,
        className: row.className,
        title: title ? title.textContent : '',
        meta: meta ? meta.textContent : '',
        metaDot: !!(meta && hasStateDot(meta)),
      };
    });
}
function rowMeta(idOrLabel) {
  const r = rows().find((r) => r.title === idOrLabel || r.key === idOrLabel);
  return r ? r.meta : '(no row)';
}

let failures = 0;
function assert(cond, msg) {
  if (cond) {
    console.log('ok - ' + msg);
  } else {
    failures++;
    console.error('FAIL - ' + msg);
  }
}

// ── the reported scenario ─────────────────────────────────────────────────
// Setup: initial pane = A.
const initialPane = makePane();
activePaneKey = initialPane.key;
const sidA = 'SESS-A';

console.log('— connect handshake (default session A) —');
dispatch({ type: 'session_state', sessionId: sidA, turnActive: false });
dispatch({ type: 'config', sessionId: sidA, mode: 'act', sessionLabel: 'session A' });
dispatch({ type: 'sessions', sessions: [{ id: sidA, label: 'session A', messageCount: 1 }] });
dispatch({ type: 'config', sessionId: sidA, mode: 'act', sessionLabel: 'session A' });

// User sends a message in A; the turn starts and runs.
console.log('— message in A; turn starts (running) —');
wsStub.sent.push({ type: 'message', content: 'turn A', sessionId: sidA });
dispatch({ type: 'waiting', sessionId: sidA });
assert(turnActive === true && initialPane.turnActive === true, 'A is busy after turn starts');
assert(rowMeta('session A').includes('responding'), 'A row shows the responding indicator');

// User clicks "New": a second pane opens (B), A stays open in the background.
console.log('— user clicks New → pane B created; A becomes a background pane —');
newSession();
assert(panes.size === 2, 'two panes after New: ' + panes.size);
const paneB = activePane();
// Server replies to session_new:
dispatch({ type: 'response', sessionId: 'SESS-B', sessionAction: 'clear_chat' });
dispatch({ type: 'clear_chat', sessionId: 'SESS-B' });
dispatch({ type: 'history', sessionId: 'SESS-B', history: [] });
dispatch({ type: 'config', sessionId: 'SESS-B', mode: 'act', sessionLabel: 'session B' });
dispatch({ type: 'sessions', sessions: [
  { id: sidA, label: 'session A', messageCount: 1 },
  { id: 'SESS-B', label: 'session B', messageCount: 0 },
] });
assert(paneB.id === 'SESS-B', 'pane B adopted SESS-B');

// A's turn keeps streaming in the background.
console.log('— A streams in the background —');
dispatch({ type: 'thinking', sessionId: sidA });
dispatch({ type: 'stream', sessionId: sidA, content: 'hello ' });
dispatch({ type: 'stream', sessionId: sidA, content: 'world' });
assert(initialPane.turnActive === true, 'background pane A still marked busy');
assert(rowMeta('session A').includes('responding'), 'A row still shows the responding indicator (turn NOT stopped)');
assert(panes.get(activePaneKey) === paneB, 'active pane is still B');

// User clicks A's row in the session list → focus A (running).
console.log('— user clicks A row (focus A, still running) —');
focusPane(initialPane.key);
assert(activePane() === initialPane, 'A is the active pane again');
assert(wsStub.sent.some((m) => m.type === 'session_attach' && m.sessionId === sidA), 'session_attach(A) sent');
// Server attach reply for A (turn still running):
dispatch({ type: 'session_state', sessionId: sidA, turnActive: true });
dispatch({ type: 'history', sessionId: sidA, history: [{ role: 'user', content: 'turn A' }] });
dispatch({ type: 'config', sessionId: sidA, mode: 'act', sessionLabel: 'session A' });
assert(turnActive === true, 'A busy again after focus');
// A's live stream continues on the focused pane:
dispatch({ type: 'stream', sessionId: sidA, content: ' continuing' });
assert(turnActive === true, 'streaming while focused on A');

// THE REPORTED SWITCH: user clicks B's row while A's turn is STILL running.
console.log('— THE REPORTED SWITCH: click B row while A is running —');
focusPane(paneB.key);
assert(activePane() === paneB, 'B is the active pane');
// B's attach reply:
dispatch({ type: 'session_state', sessionId: 'SESS-B', turnActive: false });
dispatch({ type: 'history', sessionId: 'SESS-B', history: [] });
dispatch({ type: 'config', sessionId: 'SESS-B', mode: 'act', sessionLabel: 'session B' });
// A's turn keeps streaming in the background:
dispatch({ type: 'stream', sessionId: sidA, content: ' more' });
dispatch({ type: 'thinking_token', sessionId: sidA, content: '...' });
assert(initialPane.turnActive === true, 'background pane A STILL marked busy after the switch');
assert(rowMeta('session A').includes('responding'), 'A row shows the responding indicator after the switch (turn kept running)');

// A's turn finishes:
dispatch({ type: 'turn_end', sessionId: sidA });
assert(initialPane.turnActive === false, 'A idle after its turn_end');
const aRowAfter = rows().find((r) => r.title === 'session A');
assert(aRowAfter && aRowAfter.metaDot && aRowAfter.meta.includes('open'),
  'A row shows the blue open indicator after turn_end');

// Nothing must have cancelled A: no cancel message, no detach of A.
const sentCancels = wsStub.sent.filter((m) => m.type === 'cancel');
const sentDetaches = wsStub.sent.filter((m) => m.type === 'session_detach' && m.sessionId === sidA);
assert(sentCancels.length === 0, 'no cancel message was sent during the switches: ' + JSON.stringify(wsStub.sent));
assert(sentDetaches.length === 0, 'no session_detach of A was sent during the switches');

// ── regression: mid-turn attach history must NOT wipe the live reply ──
// The reported symptom: switching to the running session made its reply
// seem to stop. Mechanism: the attach's history snapshot is INCOMPLETE
// (lacks the in-flight reply) and, for a large session, can arrive AFTER
// live stream events started rendering; the history handler then rebuilt
// from the snapshot and WIPED the visible reply. The fix keeps the live
// content and lets the turn_end convergence refetch paint the full history.
{
  focusPane(initialPane.key); // A becomes the active pane again
  // A turn is running; the attach replies: session_state latches
  // needsFreshHistory, the live stream renders FIRST (the large-session
  // race), then the (incomplete) history snapshot arrives.
  dispatch({ type: 'session_state', sessionId: sidA, turnActive: true });
  dispatch({ type: 'stream', sessionId: sidA, content: 'live reply ' });
  let cleared = 0;
  const origClearChat = clearChat;
  clearChat = function () { cleared++; return origClearChat.apply(null, arguments); };
  let result;
  try {
    result = dispatch({ type: 'history', sessionId: sidA, history: [{ role: 'user', content: 'q' }] });
  } finally {
    clearChat = origClearChat;
  }
  assert(result === 'history-skipped-live', 'mid-turn history with live stream is skipped (not wiped): ' + result);
  assert(cleared === 0, 'mid-turn history did not clearChat (live reply preserved)');
  // The turn keeps running and its reply keeps streaming:
  dispatch({ type: 'stream', sessionId: sidA, content: 'more ' });
  assert(turnActive === true && initialPane.turnActive === true, 'turn still running after the skipped history');
  // turn_end still triggers the convergence re-attach that paints the full
  // transcript:
  dispatch({ type: 'turn_end', sessionId: sidA });
  assert(wsStub.sent.some((m) => m.type === 'session_attach' && m.sessionId === sidA),
    'turn_end after skipped history still re-attaches (convergence refetch)');
}

if (failures === 0) {
  console.log('\nAll pane-switch checks passed — the client state machine keeps the original running session alive.');
  process.exit(0);
} else {
  console.error(`\n${failures} check(s) failed.`);
  process.exit(1);
}
