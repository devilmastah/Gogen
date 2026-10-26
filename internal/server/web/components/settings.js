// Settings modal + all persisted preferences for the GoGen web UI:
// the tabbed settings overlay (last-used tab persisted), the
// server-backed feature toggles (board / subagents), the runtime
// config options, the MCP test flow, the providers list, the theme
// (system-preference detection + override), file-click behavior,
// the chat diff viewer mode, the Monaco editor preferences,
// desktop notifications, the "show reply model" toggle and the
// accent color.
//
// Server-backed values round-trip over the chat WebSocket: app.js
// keeps the ws dispatch (config / test_mcp / provider_test) and
// forwards the payloads to applyFeatureSettings /
// applyRuntimeSettings / applyProviderSettings / applyMCPSettings
// below; the per-bubble reply-model render hook stays in app.js and
// reads the preference through getShowReplyModelPref.
//
// Wiring: app.js calls initSettings(deps) once at startup.
//   deps.getWs()                     — the chat WebSocket (or null)
//   deps.getPane()                   — the active pane
//   deps.getModels()                 — the available-models catalog
//   deps.requestModels()             — force a model-catalog refetch
//   deps.switchMainPane(pane)        — switch the main screen
//   deps.showToast(message, kind)    — the toast stack
//   deps.applyReplyModelChips()      — re-sync the per-bubble model chips
//   deps.setSubagentModel(v)         — the settings subagent picker state
//   deps.setSubagentThinkingLevel(v) — the settings subagent picker state
//   deps.renderSubagentPicker()      — re-render the settings subagent picker

import { openModal, closeModal, setMonacoTheme, applyEditorPrefs } from '/editor.js';
import { requestBoardState, setBoardStartPrompt } from '/components/board.js';
import { requestAutomationsState, automationsPaneVisible } from '/components/automations.js';
import { icon } from '/components/icons.js';
import { SETTINGS_SCHEMA } from '/components/settings-schema.js';
import { renderSettings } from '/components/settings-render.js';
// Safe localStorage access (see storage.js): several preferences below are
// read at module top level, where an unguarded access would throw in a
// storage-blocked browser and abort this module's (and app.js's) evaluation.
import { storageGet, storageSet, storageRemove } from '/components/storage.js';

// Build the modal body (sidebar + panels + rows) from the schema
// (components/settings-schema.js) before any control is looked up by id
// below.
renderSettings();

let deps = null;

export function initSettings(d) {
    deps = d;
}

// === Settings modal ===
const settingsBtn = document.getElementById('settings-btn');
const settingsOverlay = document.getElementById('settings-overlay');
const themeSelect = document.getElementById('theme-select');

// Settings sections are tabbed (sidebar): showSettingsTab reveals
// one .settings-group panel at a time. The last-used tab persists
// per browser (localStorage); opening the modal from a screen with
// a natural settings home (chat/editor/board) auto-selects that tab
// instead — the screen the user is on wins on open.
const SETTINGS_TAB_STORAGE = 'gogen-settings-tab';
const SCREEN_TO_SETTINGS_TAB = { chat: 'chat', editor: 'editor', board: 'agent', automations: 'automations' };
let settingsTab = storageGet(SETTINGS_TAB_STORAGE) || 'chat';

export function showSettingsTab(tab) {
    const panel = document.getElementById('settings-tab-' + tab);
    if (!panel) tab = 'chat'; // stale localStorage value — fall back
    settingsTab = tab;
    storageSet(SETTINGS_TAB_STORAGE, tab);
    document.querySelectorAll('.settings-tab-btn').forEach((btn) => {
        const on = btn.dataset.tab === tab;
        btn.classList.toggle('active', on);
        btn.setAttribute('aria-selected', on ? 'true' : 'false');
    });
    document.querySelectorAll('.settings-group[role="tabpanel"]').forEach((p) => {
        p.hidden = p.id !== 'settings-tab-' + tab;
    });
}

export function openSettings() {
    const pane = document.querySelector('.main-tab.active')?.dataset.pane;
    showSettingsTab(SCREEN_TO_SETTINGS_TAB[pane] || settingsTab);
    openModal(settingsOverlay);
    // The subagent model picker (Agent tab) needs the model catalog;
    // fetch it on open — the reply refreshes the toolbar picker too
    // (shared list_models flow, server-cached).
    const ws = deps.getWs();
    if (document.getElementById('subagent-enabled-select')?.value === 'on'
        && ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'list_models' }));
    }
}

export function closeSettings() {
    closeModal(settingsOverlay);
}

export function isSettingsOpen() {
    return settingsOverlay.classList.contains('active');
}

settingsBtn.addEventListener('click', openSettings);
document.querySelector('.settings-sidebar')?.addEventListener('click', (e) => {
    const btn = e.target.closest('.settings-tab-btn');
    if (btn && btn.dataset.tab) showSettingsTab(btn.dataset.tab);
});
document.getElementById('settings-close-btn')?.addEventListener('click', () => {
    closeSettings();
});
settingsOverlay.addEventListener('click', (e) => {
    if (e.target === settingsOverlay) closeSettings();
});

// === Feature settings: server-backed toggles (config WS message) ===
// Unlike the localStorage preferences above, these round-trip to the
// server: the toggle applies live to every session and is persisted
// to .gogen/gogen.conf. The select state follows the config push
// (applyFeatureSettings), so multiple tabs stay in sync.
const boardEnabledSelect = document.getElementById('board-enabled-select');
const subagentEnabledSelect = document.getElementById('subagent-enabled-select');
const reviewAgentEnabledSelect = document.getElementById('review-agent-enabled-select');
const subagentDepthInput = document.getElementById('subagent-depth-input');
const subagentLimitInput = document.getElementById('subagent-limit-input');
if (boardEnabledSelect) {
    boardEnabledSelect.addEventListener('change', () => {
        sendFeatureConfig({ board: boardEnabledSelect.value });
    });
}
if (reviewAgentEnabledSelect) {
    reviewAgentEnabledSelect.addEventListener('change', () => {
        sendFeatureConfig({ reviewAgent: reviewAgentEnabledSelect.value });
    });
}
if (subagentEnabledSelect) {
    subagentEnabledSelect.addEventListener('change', () => {
        sendFeatureConfig({ subagent: subagentEnabledSelect.value });
    });
}
const automationsEnabledSelect = document.getElementById('automations-enabled-select');
if (automationsEnabledSelect) {
    automationsEnabledSelect.addEventListener('change', () => {
        sendFeatureConfig({ automations: automationsEnabledSelect.value });
    });
}
if (subagentDepthInput) {
    subagentDepthInput.addEventListener('change', () => {
        const n = parseInt(subagentDepthInput.value, 10);
        if (!Number.isFinite(n) || n < 1) {
            subagentDepthInput.value = '1';
            return;
        }
        const clamped = Math.min(n, 10);
        subagentDepthInput.value = String(clamped);
        sendFeatureConfig({ subagentMaxDepth: clamped });
    });
}
if (subagentLimitInput) {
    subagentLimitInput.addEventListener('change', () => {
        const n = parseInt(subagentLimitInput.value, 10);
        if (!Number.isFinite(n) || n < 1) {
            subagentLimitInput.value = '1';
            return;
        }
        const clamped = Math.min(n, 32);
        subagentLimitInput.value = String(clamped);
        sendFeatureConfig({ subagentMaxConcurrent: clamped });
    });
}

// ── Feature settings (board / subagents) ──
// Server-backed: the settings-modal controls and the board tab
// visibility follow the config push (data.board / data.subagent /
// data.subagentMaxDepth), so every tab stays in sync without
// localStorage. Toggling board off while the board pane is the
// active view falls back to chat; the board data is untouched.
export function applyFeatureSettings(data) {
    const boardOn = data.board === 'on';
    const subagentOn = data.subagent === 'on';
    const reviewAgentOn = data.reviewAgent === 'on';
    const boardTab = document.getElementById('board-tab');
    const wasVisible = boardTab && !boardTab.hidden;
    if (boardTab && boardTab.hidden === boardOn) boardTab.hidden = !boardOn;
    const boardSel = document.getElementById('board-enabled-select');
    if (boardSel && boardSel.value !== (boardOn ? 'on' : 'off')) boardSel.value = boardOn ? 'on' : 'off';
    // The board agent prompt is meaningful only while the board
    // feature is on (like the subagent model picker).
    const boardPromptRow = document.getElementById('board-prompt-row');
    if (boardPromptRow) boardPromptRow.hidden = !boardOn;
    const subSel = document.getElementById('subagent-enabled-select');
    if (subSel && subSel.value !== (subagentOn ? 'on' : 'off')) subSel.value = subagentOn ? 'on' : 'off';
    const automationsOn = data.automations === 'on';
    const autoSel = document.getElementById('automations-enabled-select');
    if (autoSel && autoSel.value !== (automationsOn ? 'on' : 'off')) autoSel.value = automationsOn ? 'on' : 'off';
    // The automations TAB follows the flag exactly like the board tab:
    // shown only while the scheduler may fire. Turning it on requests the
    // snapshot; turning it off falls back to chat when the pane was active.
    const automationsTab = document.getElementById('automations-tab');
    if (automationsTab && automationsTab.hidden === automationsOn) automationsTab.hidden = !automationsOn;
    if (automationsOn && automationsPaneVisible()) requestAutomationsState();
    if (!automationsOn) {
        const automationsPane = document.getElementById('automations-pane');
        if (automationsPane && automationsPane.classList.contains('active')) deps.switchMainPane('chat');
    }
    const depthInput = document.getElementById('subagent-depth-input');
    if (depthInput && data.subagentMaxDepth > 0 && String(depthInput.value) !== String(data.subagentMaxDepth)) {
        depthInput.value = String(data.subagentMaxDepth);
    }
    const limitInput = document.getElementById('subagent-limit-input');
    if (limitInput && data.subagentMaxConcurrent > 0 && String(limitInput.value) !== String(data.subagentMaxConcurrent)) {
        limitInput.value = String(data.subagentMaxConcurrent);
    }
    if (boardOn && !wasVisible) requestBoardState();
    if (!boardOn) {
        const boardPane = document.getElementById('board-pane');
        if (boardPane && boardPane.classList.contains('active')) deps.switchMainPane('chat');
    }
    // The subagent model + effort pickers are meaningful only while
    // subagents are enabled.
    const subModelPicker = document.getElementById('subagent-model-picker');
    if (subModelPicker) subModelPicker.hidden = !subagentOn;
    const subThinkingPicker = document.getElementById('subagent-thinking-picker');
    if (subThinkingPicker) subThinkingPicker.hidden = !subagentOn;
    // The review-agent controls are meaningful only while the review
    // agent is on AND the board is on (the reviewer works on board
    // tickets; the trigger rides on the board manager).
    const reviewAgentRowsVisible = reviewAgentOn && boardOn;
    const reviewSel = document.getElementById('review-agent-enabled-select');
    if (reviewSel && reviewSel.value !== (reviewAgentOn ? 'on' : 'off')) reviewSel.value = reviewAgentOn ? 'on' : 'off';
    const reviewModelPicker = document.getElementById('review-agent-model-row');
    if (reviewModelPicker) reviewModelPicker.hidden = !reviewAgentRowsVisible;
    const reviewThinkingPicker = document.getElementById('review-agent-thinking-row');
    if (reviewThinkingPicker) reviewThinkingPicker.hidden = !reviewAgentRowsVisible;
    const reviewPromptRow = document.getElementById('board-review-prompt-row');
    if (reviewPromptRow) reviewPromptRow.hidden = !reviewAgentRowsVisible;
    const ws = deps.getWs();
    const models = deps.getModels();
    const needsModels = isSettingsOpen() && (!models || models.length === 0)
        && ws && ws.readyState === WebSocket.OPEN;
    if (needsModels && subagentOn && !subModelPicker.hidden) {
        // Just enabled while the modal is open: fetch the catalog so
        // the subagent picker is not stuck on "No models loaded".
        ws.send(JSON.stringify({ type: 'list_models' }));
    }
    if (needsModels && reviewAgentRowsVisible) {
        // Same for the review-agent model picker.
        ws.send(JSON.stringify({ type: 'list_models' }));
    }
}

// Sends a feature-setting change over the existing config WS message
// (the same channel the server uses to push config to us).
export function sendFeatureConfig(fields) {
    const ws = deps.getWs();
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    const pane = deps.getPane();
    ws.send(JSON.stringify(Object.assign({ type: 'config', sessionId: pane ? pane.id || '' : '' }, fields)));
}

// === Runtime config options: server-backed (config WS message) ===
// The settings-modal options below round-trip through the config
// message's ConfigFields mechanism: a change sends { type: 'config',
// configFields: [name], <prop>: value }, the server applies it (live
// or restart-staged) and pushes the current values back, which keeps
// every tab in sync. prop overrides the WSMessage property when it
// differs from the ConfigFields name (contextLimitConfig).
// Derived from the schema (components/settings-schema.js): every
// server-backed runtime control, in schema order. Adding a runtime setting
// is one schema entry — no second id/field list to keep in sync here.
const RUNTIME_CONTROLS = SETTINGS_SCHEMA
    .filter((e) => e.storage === 'server' && e.channel === 'runtime')
    .map((e) => ({ id: e.id, field: e.field, prop: e.prop }));

// Sends one or more runtime-config changes: { field: {prop, value} }.
export function sendRuntimeConfig(changes) {
    const ws = deps.getWs();
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    const payload = { type: 'config', configFields: [] };
    for (const field of Object.keys(changes)) {
        payload.configFields.push(field);
        const { prop, value } = changes[field];
        payload[prop] = value;
    }
    ws.send(JSON.stringify(payload));
}

function wireRuntimeControl(ctrl) {
    const el = document.getElementById(ctrl.id);
    if (!el) return;
    el.addEventListener('change', () => {
        let value = el.value;
        if (el.type === 'number') {
            // Fractional fields (e.g. #compact-threshold-input, step="0.05")
            // must be parsed as floats; integer-step fields stay integers.
            const step = parseFloat(el.step || '1');
            const n = Number.isFinite(step) && step % 1 === 0
                ? parseInt(el.value, 10)
                : parseFloat(el.value);
            if (!Number.isFinite(n)) return;
            value = n;
        }
        sendRuntimeConfig({ [ctrl.field]: { prop: ctrl.prop || ctrl.field, value } });
    });
}
RUNTIME_CONTROLS.forEach(wireRuntimeControl);

// "Reset to default" for the prompt templates: send the explicit
// empty value; the server resolves it back to the built-in default
// and the next push re-populates the textarea with it.
const PROMPT_RESET_BUTTONS = SETTINGS_SCHEMA
    .filter((e) => e.resetToDefault)
    .map((e) => ({ id: e.resetId, field: e.field }));
for (const b of PROMPT_RESET_BUTTONS) {
    const el = document.getElementById(b.id);
    if (!el) continue;
    el.addEventListener('click', () => sendRuntimeConfig({ [b.field]: { prop: b.field, value: '' } }));
}

// Applies the runtime-config values from the server push to the
// controls, updates the secret placeholders, and renders the
// restart banner. Values not present in the push are left as-is.
export function applyRuntimeSettings(data) {
    for (const ctrl of RUNTIME_CONTROLS) {
        const el = document.getElementById(ctrl.id);
        if (!el) continue;
        const v = data[ctrl.prop || ctrl.field];
        if (v === undefined) continue;
        if (el.type === 'number') {
            if (String(el.value) !== String(v)) el.value = String(v);
        } else if (el.value !== String(v)) {
            el.value = String(v);
        }
    }
    const searchKeyEl = document.getElementById('web-search-api-key-input');
    if (searchKeyEl) searchKeyEl.placeholder = data.webSearchApiKeySet ? 'API key (blank keeps stored)' : 'API key';
    const authTokenEl = document.getElementById('web-auth-token-input');
    if (authTokenEl) authTokenEl.placeholder = data.webAuthTokenSet ? 'Token set (blank keeps stored)' : 'Auth token';
    // Status display: the host/token/row controls are meaningless only when
    // the reporter is explicitly off — 'auto' (the default) probes, so they
    // must stay visible there too. The runtime channel has no generic row
    // gating, so the gate follows the pushed value here.
    const statusOn = data.statusDisplay !== 'off';
    for (const id of ['status-display-host-row', 'status-display-token-row', 'status-display-source-row']) {
        const row = document.getElementById(id);
        if (row) row.hidden = !statusOn;
    }
    const statusTokenEl = document.getElementById('status-display-token-input');
    if (statusTokenEl) statusTokenEl.placeholder = data.statusDisplayTokenSet ? 'Token set (blank keeps stored)' : 'Token';
    const banner = document.getElementById('restart-banner');
    if (banner) {
        if (Array.isArray(data.restartRequired) && data.restartRequired.length > 0) {
            // Icon + text nodes (no innerHTML: the list is server data).
            const warnIcon = document.createElement('span');
            warnIcon.className = 'banner-icon';
            warnIcon.innerHTML = icon('alert');
            banner.replaceChildren(
                warnIcon,
                ' Restart gogen for these to take effect: ' + data.restartRequired.join(', ')
            );
            banner.hidden = false;
        } else {
            banner.hidden = true;
        }
    }
    // Subagent default model: the server pushes the current value
    // ("" = inherit, included even when empty so a clear by another
    // tab syncs here too).
    if (data.subagentModel !== undefined) {
        deps.setSubagentModel(data.subagentModel || '');
        if (isSettingsOpen()) {
            // Model-aware effort options follow the pushed model.
            deps.renderSubagentPicker();
        }
    }
    // Subagent reasoning effort: the server pushes the current
    // value ("" = inherit, pointer-carried so a clear by another
    // tab syncs here too), like the model above.
    if (data.subagentThinkingLevel !== undefined) {
        deps.setSubagentThinkingLevel(data.subagentThinkingLevel || '');
        if (isSettingsOpen()) deps.renderSubagentPicker();
    }
    // Board start prompt template (resolved by the server: empty
    // config → built-in default): the "Start agent" popover's pen
    // editor pre-fills from it.
    if (data.boardStartPrompt !== undefined) {
        setBoardStartPrompt(data.boardStartPrompt || '');
    }
    // Review agent default model + reasoning effort: the server
    // pushes the current values ("" = cascade / inherit, pointer-carried
    // so a clear by another tab syncs here too), like the subagent
    // picker above.
    if (data.reviewAgentModel !== undefined) {
        deps.setReviewAgentModel(data.reviewAgentModel || '');
        if (isSettingsOpen()) deps.renderReviewAgentPicker();
    }
    if (data.reviewAgentThinkingLevel !== undefined) {
        deps.setReviewAgentThinkingLevel(data.reviewAgentThinkingLevel || '');
        if (isSettingsOpen()) deps.renderReviewAgentPicker();
    }
}

// === MCP test: server-backed (test_mcp WS message) ===
// The configured server list follows the config push (applyMCPSettings);
// row Test buttons probe a saved server by name (stored command/args/
// env resolved server-side), the form probes a typed command/args/env
// before adding it to the config file by hand. Never registers anything.
const mcpServerListEl = document.getElementById('mcp-server-list');
const mcpTestNameInput = document.getElementById('mcp-test-name');
const mcpTestCommandInput = document.getElementById('mcp-test-command');
const mcpTestArgsInput = document.getElementById('mcp-test-args');
const mcpTestEnvInput = document.getElementById('mcp-test-env');
const mcpTestBtn = document.getElementById('mcp-test-btn');
const mcpTestResult = document.getElementById('mcp-test-result');

function renderMCPServerList(servers) {
    if (!mcpServerListEl || !Array.isArray(servers)) return;
    mcpServerListEl.innerHTML = '';
    if (servers.length === 0) {
        mcpServerListEl.textContent = 'No MCP servers configured (mcp_servers in the config file).';
        mcpServerListEl.className = 'settings-note';
        return;
    }
    mcpServerListEl.className = 'mcp-server-list';
    for (const s of servers) {
        const row = document.createElement('div');
        row.className = 'provider-row';
        const info = document.createElement('span');
        info.className = 'provider-row-info';
        info.textContent = s.name + '  ·  ' + s.command + (s.args && s.args.length ? '  ' + s.args.join(' ') : '') + (s.envSet ? '  ·  env •••' : '') + (s.host ? '  ·  this process' : '');
        row.appendChild(info);
        const actions = document.createElement('span');
        actions.className = 'provider-row-actions';
        if (!s.host) {
            const testBtn = document.createElement('button');
            testBtn.type = 'button';
            testBtn.textContent = 'Test';
            testBtn.title = 'Test connectivity and list tools';
            testBtn.addEventListener('click', () => {
                showMCPTest('Testing ' + s.name + '…', '');
                sendMCPTest({ name: s.name });
            });
            actions.appendChild(testBtn);
        }
        row.appendChild(actions);
        mcpServerListEl.appendChild(row);
    }
}

export function showMCPTest(text, kind) {
    if (!mcpTestResult) return;
    mcpTestResult.textContent = text;
    mcpTestResult.className = 'settings-note' + (kind ? ' ' + kind : '');
}

function sendMCPTest(req) {
    const ws = deps.getWs();
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type: 'test_mcp', mcpTest: req }));
}

// "KEY=VALUE KEY2=VALUE2" → { KEY: VALUE, KEY2: VALUE2 } (invalid
// parts silently dropped).
function parseEnvInput(raw) {
    const env = {};
    for (const part of String(raw || '').trim().split(/\s+/)) {
        if (!part) continue;
        const eq = part.indexOf('=');
        if (eq <= 0) continue;
        env[part.slice(0, eq)] = part.slice(eq + 1);
    }
    return env;
}

export function applyMCPSettings(data) {
    if (Array.isArray(data.mcpServers)) {
        renderMCPServerList(data.mcpServers);
    }
}

if (mcpTestBtn) {
    mcpTestBtn.addEventListener('click', () => {
        showMCPTest('Testing…', '');
        const rawArgs = mcpTestArgsInput.value.trim();
        sendMCPTest({
            name: mcpTestNameInput.value.trim(),
            command: mcpTestCommandInput.value.trim(),
            args: rawArgs ? rawArgs.split(/\s+/) : [],
            env: parseEnvInput(mcpTestEnvInput.value),
        });
    });
}

// === Providers: server-backed list (config WS message) ===
// The list follows the config push (applyProviderSettings); the
// add/edit/delete/test actions round-trip through provider_save /
// provider_delete / test_provider. Keys are never pushed — only the
// apiKeySet flag — and the storage warning renders the actual config
// file path from the server.
const providerListEl = document.getElementById('provider-list');
const providerWarningPath = document.getElementById('provider-config-path');
const providerNameInput = document.getElementById('provider-name');
const providerBaseURLInput = document.getElementById('provider-base-url');
const providerKeyInput = document.getElementById('provider-api-key');
const providerModelInput = document.getElementById('provider-model');
const providerAddBtn = document.getElementById('provider-add-btn');
const providerTestBtn = document.getElementById('provider-test-btn');
const providerTestResult = document.getElementById('provider-test-result');

function sendProviderOp(type, fields) {
    const ws = deps.getWs();
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type, providerOp: fields }));
}

export function showProviderTest(text, kind) {
    if (!providerTestResult) return;
    providerTestResult.textContent = text;
    providerTestResult.className = 'settings-note' + (kind ? ' ' + kind : '');
}

function renderProviderList(providers) {
    if (!providerListEl || !Array.isArray(providers)) return;
    providerListEl.innerHTML = '';
    for (const p of providers) {
        const row = document.createElement('div');
        row.className = 'provider-row';
        const info = document.createElement('span');
        info.className = 'provider-row-info';
        info.textContent = p.name + (p.baseUrl ? '  ·  ' + p.baseUrl : '') + (p.apiKeySet ? '  ·  key •••' : '');
        row.appendChild(info);
        const actions = document.createElement('span');
        actions.className = 'provider-row-actions';
        const testBtn = document.createElement('button');
        testBtn.type = 'button';
        testBtn.textContent = 'Test';
        testBtn.title = 'Test connectivity and list models';
        testBtn.addEventListener('click', () => {
            showProviderTest('Testing ' + p.name + '…', '');
            sendProviderOp('test_provider', { name: p.name, baseUrl: p.baseUrl, model: p.model });
        });
        actions.appendChild(testBtn);
        const editBtn = document.createElement('button');
        editBtn.type = 'button';
        editBtn.textContent = 'Edit';
        editBtn.addEventListener('click', () => {
            providerNameInput.value = p.name;
            providerBaseURLInput.value = p.baseUrl || '';
            providerKeyInput.value = '';
            providerModelInput.value = p.model || '';
            providerKeyInput.placeholder = p.apiKeySet ? 'API key (blank keeps stored)' : 'API key';
            providerNameInput.focus();
        });
        actions.appendChild(editBtn);
        const delBtn = document.createElement('button');
        delBtn.type = 'button';
        delBtn.textContent = 'Delete';
        delBtn.disabled = !p.deletable;
        if (p.deletable) {
            delBtn.title = 'Remove this provider';
            delBtn.addEventListener('click', () => {
                if (!confirm('Delete provider "' + p.name + '"? Its models will disappear from the picker.')) return;
                showProviderTest('', '');
                sendProviderOp('provider_delete', { name: p.name });
            });
        } else {
            delBtn.title = 'The default profile is built from the legacy config fields';
        }
        actions.appendChild(delBtn);
        row.appendChild(actions);
        providerListEl.appendChild(row);
    }
}

// Last-seen provider list (JSON): a change means a provider was
// added/edited/deleted, so the model catalog must be re-fetched —
// the picker's availableModels is otherwise a stale snapshot.
let lastProvidersJson = null;

export function applyProviderSettings(data) {
    if (Array.isArray(data.providers)) {
        const json = JSON.stringify(data.providers);
        if (json !== lastProvidersJson) {
            lastProvidersJson = json;
            // Re-request the aggregated catalog so the new provider's
            // models appear in the picker (and deleted ones drop).
            deps.requestModels();
        }
        renderProviderList(data.providers);
    }
    if (data.configFilePath && providerWarningPath) {
        providerWarningPath.textContent = data.configFilePath;
    }
}

function testProviderForm() {
    const name = providerNameInput.value.trim();
    const baseUrl = providerBaseURLInput.value.trim();
    const model = providerModelInput.value.trim();
    showProviderTest('Testing…', '');
    // Testing a saved provider by name uses its stored key; the
    // add-form test carries the typed endpoint (key included).
    sendProviderOp('test_provider', { name, baseUrl, apiKey: providerKeyInput.value, model });
}

if (providerAddBtn) {
    providerAddBtn.addEventListener('click', () => {
        const name = providerNameInput.value.trim();
        if (!name) {
            deps.showToast('Provider name is required', 'error');
            return;
        }
        const baseUrl = providerBaseURLInput.value.trim();
        const apiKey = providerKeyInput.value;
        const model = providerModelInput.value.trim();
        sendProviderOp('provider_save', { name, baseUrl, apiKey, model });
        providerKeyInput.value = '';
        providerKeyInput.placeholder = 'API key (blank keeps stored)';
        showProviderTest('', '');
    });
}
if (providerTestBtn) {
    providerTestBtn.addEventListener('click', testProviderForm);
}

// === Theme: detect system preference, allow override ===
const prefersDark = window.matchMedia('(prefers-color-scheme: dark)');
const savedTheme = storageGet('gogen-theme') || 'auto';

function resolveTheme(preference) {
    if (preference === 'auto' || !preference) return prefersDark.matches ? 'dark' : 'light';
    return preference;
}

function applyTheme(preference) {
    const resolved = resolveTheme(preference);
    document.documentElement.classList.toggle('light', resolved === 'light');
    storageSet('gogen-theme', preference);
    // Keep select in sync
    if (themeSelect.value !== preference) themeSelect.value = preference;
    // Address-bar color follows the theme.
    const themeMeta = document.querySelector('meta[name="theme-color"]');
    // Keep in sync with --bg in styles.css (:root / :root.light).
    if (themeMeta) themeMeta.setAttribute('content', resolved === 'light' ? '#f7f8fa' : '#131418');
    // Switch Monaco theme to match
    setMonacoTheme(resolved === 'light');
}

// Initialize
themeSelect.value = savedTheme;
applyTheme(savedTheme);

// Listen for OS theme changes (only affects "auto" mode)
prefersDark.addEventListener('change', () => {
    const pref = storageGet('gogen-theme') || 'auto';
    if (pref === 'auto') applyTheme('auto');
});

themeSelect.addEventListener('change', () => {
    applyTheme(themeSelect.value);
});

// === Chat reading width: full width (default) or a centered column ===
// Purely presentational and per-browser (localStorage), like the theme
// above. Toggling a class on #messages lets styles.css cap each transcript
// item to a readable column; the CSS collapses to full width once the
// viewport is narrower than the column, so no JS width math is needed.
const chatWidthSelect = document.getElementById('chat-width-select');
const savedChatWidth = storageGet('gogen_chat_width') || 'full';

function applyChatWidth(value) {
    const msgs = document.getElementById('messages');
    if (msgs) msgs.classList.toggle('chat-column', value === 'comfortable');
}

if (chatWidthSelect) {
    chatWidthSelect.value = savedChatWidth;
    chatWidthSelect.addEventListener('change', () => {
        storageSet('gogen_chat_width', chatWidthSelect.value);
        applyChatWidth(chatWidthSelect.value);
    });
    applyChatWidth(savedChatWidth);
}
// Cross-tab sync (mirrors the other local preferences).
window.addEventListener('storage', (e) => {
    if (e.key === 'gogen_chat_width') {
        const val = e.newValue || 'full';
        if (chatWidthSelect && chatWidthSelect.value !== val) chatWidthSelect.value = val;
        applyChatWidth(val);
    }
});

// === File-click behavior: persist and apply ===
const fileClickSelect = document.getElementById('file-click-behavior');
const savedFileClick = storageGet('gogen_file_click_behavior') || 'open';
fileClickSelect.value = savedFileClick;
fileClickSelect.addEventListener('change', () => {
    storageSet('gogen_file_click_behavior', fileClickSelect.value);
});
// Listen for storage changes from other tabs
window.addEventListener('storage', (e) => {
    if (e.key === 'gogen_file_click_behavior') {
        const val = e.newValue || 'open';
        if (fileClickSelect.value !== val) fileClickSelect.value = val;
    }
});

// === Chat diff viewer: static tokenizer pre (default) or full Monaco editor ===
const diffViewerSelect = document.getElementById('chat-diff-viewer');
const savedDiffViewer = storageGet('gogen_chat_diff_viewer') || 'tokenizer';
diffViewerSelect.value = savedDiffViewer;
diffViewerSelect.addEventListener('change', () => {
    storageSet('gogen_chat_diff_viewer', diffViewerSelect.value);
});
window.addEventListener('storage', (e) => {
    if (e.key === 'gogen_chat_diff_viewer') {
        const val = e.newValue || 'tokenizer';
        if (diffViewerSelect.value !== val) diffViewerSelect.value = val;
    }
});

// The chat diff viewer is either a static line-numbered <pre>
// (default) or a full Monaco editor — a user setting
// (gogen_chat_diff_viewer). Reads storage directly so it works
// regardless of when the settings UI initializes.
export function diffViewerMode() {
    return storageGet('gogen_chat_diff_viewer') === 'monaco' ? 'monaco' : 'tokenizer';
}

// === Editor preferences: persist and apply to the live Monaco editor ===
const editorMinimapSelect = document.getElementById('editor-minimap');
const editorWordwrapSelect = document.getElementById('editor-wordwrap');
const editorStickySelect = document.getElementById('editor-sticky');
const editorFontsizeInput = document.getElementById('editor-fontsize');

function clampFontSize(v) {
    // Mirror getEditorPrefs() clamping so the control can never show a
    // value the editor won't actually apply.
    const min = parseInt(editorFontsizeInput?.min || '8', 10);
    const max = parseInt(editorFontsizeInput?.max || '32', 10);
    let n = parseInt(v, 10);
    if (!Number.isFinite(n)) n = 13;
    return Math.max(min, Math.min(max, n));
}

function initEditorPrefControl(control, key, dflt) {
    if (!control) return;
    const saved = storageGet(key) || dflt;
    control.value = saved;
    control.addEventListener('change', () => {
        if (control.type === 'number') {
            // Keep the visible value in sync with what the editor will
            // actually apply (getEditorPrefs clamps to [min, max]).
            control.value = String(clampFontSize(control.value));
        }
        storageSet(key, control.value);
        applyEditorPrefs();
    });
}
initEditorPrefControl(editorMinimapSelect, 'gogen_editor_minimap', 'off');
initEditorPrefControl(editorWordwrapSelect, 'gogen_editor_wordwrap', 'on');
initEditorPrefControl(editorStickySelect, 'gogen_editor_sticky', 'on');
initEditorPrefControl(editorFontsizeInput, 'gogen_editor_fontsize', '13');
// Keep editor prefs in sync across tabs (mirrors the other settings).
window.addEventListener('storage', (e) => {
    let changed = false;
    if (e.key === 'gogen_editor_minimap' && editorMinimapSelect) {
        editorMinimapSelect.value = e.newValue || 'off';
        changed = true;
    } else if (e.key === 'gogen_editor_wordwrap' && editorWordwrapSelect) {
        editorWordwrapSelect.value = e.newValue || 'on';
        changed = true;
    } else if (e.key === 'gogen_editor_sticky' && editorStickySelect) {
        editorStickySelect.value = e.newValue || 'on';
        changed = true;
    } else if (e.key === 'gogen_editor_fontsize' && editorFontsizeInput) {
        editorFontsizeInput.value = String(clampFontSize(e.newValue || '13'));
        changed = true;
    }
    if (changed) applyEditorPrefs();
});

// === Desktop notifications ===
const notificationsSelect = document.getElementById('notifications-select');
const savedNotifications = storageGet('gogen_notifications') || 'off';
notificationsSelect.value = savedNotifications;
notificationsSelect.addEventListener('change', () => {
    setNotificationPref(notificationsSelect.value);
});
window.addEventListener('storage', (e) => {
    if (e.key === 'gogen_notifications') {
        const val = e.newValue || 'off';
        if (notificationsSelect.value !== val) notificationsSelect.value = val;
    }
});

// === Show reply model (Settings → Chat): per-bubble model chip ===
const showReplyModelSelect = document.getElementById('show-reply-model');
const savedShowReplyModel = storageGet('gogen_show_reply_model') || 'off';
if (showReplyModelSelect) {
    showReplyModelSelect.value = savedShowReplyModel;
    showReplyModelSelect.addEventListener('change', () => {
        storageSet('gogen_show_reply_model', showReplyModelSelect.value);
        deps.applyReplyModelChips();
    });
}
window.addEventListener('storage', (e) => {
    if (e.key === 'gogen_show_reply_model') {
        const val = e.newValue || 'off';
        if (showReplyModelSelect && showReplyModelSelect.value !== val) {
            showReplyModelSelect.value = val;
        }
        deps.applyReplyModelChips();
    }
});

// "Show reply model" (Settings → Chat): true when the per-bubble
// model chip preference is on. app.js's per-bubble render hook
// reads the preference through this (it stays in app.js).
export function getShowReplyModelPref() {
    return (storageGet('gogen_show_reply_model') || 'off') === 'on';
}

// === Accent color: persist and apply ===
const accentInput = document.getElementById('accent-color-input');

// The soft accent variant is derived in CSS via color-mix, but the
// FILLED variant (--accent-bg, primary buttons) must keep white label
// text at WCAG AA (4.5:1) for ANY accent the user picks — a fixed
// per-theme fill cannot follow an arbitrary hue. So JS computes it:
// the accent's hue darkened until white text passes (see accentBgFor).
// Without a custom accent the CSS fallbacks in styles.css apply.
function applyAccentColor(hex) {
    if (!hex) {
        document.documentElement.style.removeProperty('--user-accent');
        document.documentElement.style.removeProperty('--user-accent-bg');
        return;
    }
    document.documentElement.style.setProperty('--user-accent', hex);
    const bg = accentBgFor(hex);
    if (bg) document.documentElement.style.setProperty('--user-accent-bg', bg);
}

// Returns the filled-accent color for the given hex: same hue, darkened
// (in HSL lightness) until white text reaches 4.5:1 contrast. Starts at
// the accent's own lightness (capped at 50% so it never lightens) — a
// dark enough accent is used as-is. Black always passes, so the loop
// always converges; the final return is unreachable except for
// malformed input that slipped past the hex check.
function accentBgFor(hex) {
    const m = /^#?([0-9a-f]{6})$/i.exec(String(hex || '').trim());
    if (!m) return null;
    const r0 = parseInt(m[1].slice(0, 2), 16) / 255;
    const g0 = parseInt(m[1].slice(2, 4), 16) / 255;
    const b0 = parseInt(m[1].slice(4, 6), 16) / 255;
    const [h, s, l] = rgbToHsl(r0, g0, b0);
    for (let light = Math.min(l, 0.5); light >= 0; light -= 0.02) {
        const [r, g, b] = hslToRgb(h, s, light);
        if (contrastAgainstWhite(r, g, b) >= 4.5) return toHex(r, g, b);
    }
    return '#000000';
}

function rgbToHsl(r, g, b) {
    const max = Math.max(r, g, b), min = Math.min(r, g, b);
    const l = (max + min) / 2;
    if (max === min) return [0, 0, l];
    const d = max - min;
    const s = l > 0.5 ? d / (2 - max - min) : d / (max + min);
    let h;
    if (max === r) h = (g - b) / d + (g < b ? 6 : 0);
    else if (max === g) h = (b - r) / d + 2;
    else h = (r - g) / d + 4;
    return [h * 60, s, l];
}

function hslToRgb(h, s, l) {
    h = (((h % 360) + 360) % 360) / 360;
    if (s === 0) return [l, l, l];
    const q = l < 0.5 ? l * (1 + s) : l + s - l * s;
    const p = 2 * l - q;
    const f = (t) => {
        if (t < 0) t += 1;
        if (t > 1) t -= 1;
        if (t < 1 / 6) return p + (q - p) * 6 * t;
        if (t < 1 / 2) return q;
        if (t < 2 / 3) return p + (q - p) * (2 / 3 - t) * 6;
        return p;
    };
    return [f(h + 1 / 3), f(h), f(h - 1 / 3)];
}

// WCAG contrast of white text on the given sRGB color (0..1 channels).
function contrastAgainstWhite(r, g, b) {
    const lin = (c) => (c <= 0.04045 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4));
    const L = 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
    return 1.05 / (L + 0.05);
}

function toHex(r, g, b) {
    const h = (v) => Math.round(v * 255).toString(16).padStart(2, '0');
    return '#' + h(r) + h(g) + h(b);
}

const savedAccent = storageGet('gogen-accent-color') || '';
if (savedAccent) {
    accentInput.value = savedAccent;
    applyAccentColor(savedAccent);
}

accentInput.addEventListener('input', function () {
    const hex = accentInput.value;
    applyAccentColor(hex);
    storageSet('gogen-accent-color', hex);
});

window.addEventListener('storage', function (e) {
    if (e.key === 'gogen-accent-color') {
        const val = e.newValue || '';
        if (accentInput.value !== val) accentInput.value = val;
        applyAccentColor(val);
    }
});

// === Accent color: reset to theme default ===
document.getElementById('accent-reset-btn').addEventListener('click', function () {
    storageRemove('gogen-accent-color');
    document.documentElement.style.removeProperty('--user-accent');
    document.documentElement.style.removeProperty('--user-accent-bg');
    // Reset picker to the current theme's default accent
    const isLight = document.documentElement.classList.contains('light');
    // Theme default accents — keep in sync with --accent fallbacks in styles.css.
    accentInput.value = isLight ? '#2563eb' : '#7aa2f7';
    accentInput.blur();
});

export function getNotificationPref() {
    return storageGet('gogen_notifications') || 'off';
}

// Persists the notification preference, keeps the settings select in
// sync and requests permission when turning on (the command palette's
// toggle command goes through this too).
export function setNotificationPref(value) {
    storageSet('gogen_notifications', value);
    if (notificationsSelect.value !== value) notificationsSelect.value = value;
    if (value !== 'off') {
        requestNotificationPermission();
    }
}

function requestNotificationPermission() {
    if (!('Notification' in window)) return;
    if (Notification.permission === 'granted') return;
    if (Notification.permission === 'denied') return;
    Notification.requestPermission();
}

/**
 * Send a desktop notification if the user has enabled them.
 * @param {string} title
 * @param {string} [body]
 * @param {string} [tag] - dedup tag (prevents stacking)
 */
export function sendNotification(title, body, tag) {
    const pref = getNotificationPref();
    if (pref === 'off') return;
    if (!('Notification' in window)) return;
    if (Notification.permission !== 'granted') return;

    // "background" mode: only notify when the tab is not focused
    if (pref === 'background' && document.hasFocus()) return;

    try {
        const opts = { tag: tag || 'gogen-notification' };
        if (body) opts.body = body;
        new Notification(title, opts);
    } catch (_) {
        // Service worker or permission issue — silently ignore.
    }
}

// Restore notification permission on load if preference is non-off
if (getNotificationPref() !== 'off') {
    requestNotificationPermission();
}
