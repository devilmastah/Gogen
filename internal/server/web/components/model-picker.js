// Shared model + reasoning-effort picker components for the GoGen web UI.
//
// The toolbar model popover, the Agent-settings subagent picker and the
// board "Start agent" popover all render the same controls: a model list
// (grouped by provider, search-filtered, current-model check) and
// MODEL-AWARE reasoning-effort chips. This module owns that rendering so
// the three stay identical; app.js wires each instance to its own state
// and message channel through the factory configs.

import { icon } from '/components/icons.js';

// Labels for the default reasoning_effort levels (off/low/medium/high).
// Values outside the defaults (e.g. a model's "max" from models.dev)
// derive their label by title-casing.
const THINKING_LABELS = { off: 'Off', low: 'L', medium: 'M', high: 'H' };

// The default reasoning_effort set, used whenever the catalog has no
// per-model data.
export const DEFAULT_THINKING_EFFORTS = ['low', 'medium', 'high'];

// Compact token counts for chips/tooltips ("128k", "8k", "—").
export function formatTokenCount(n, zeroText = '—') {
    if (!n || n <= 0) return zeroText;
    if (n < 1000) return String(n);
    const whole = Math.floor(n / 1000);
    const frac = Math.floor((n % 1000) / 100);
    return frac === 0 ? `${whole}k` : `${whole}.${frac}k`;
}

export function thinkingLabel(value) {
    return THINKING_LABELS[value] || (value ? value[0].toUpperCase() + value.slice(1) : value);
}

// Full (title-cased) form of a reasoning-effort value — the
// thinkingLabel fallback, for the model/thinking pickers where
// the toolbar chips' L/M/H short forms are too cryptic.
export function thinkingFullLabel(value) {
    return value ? value[0].toUpperCase() + value.slice(1) : value;
}

// Non-interactive placeholder row for the model list when there is
// nothing to show (catalog not loaded yet, or the filter matched
// nothing).
function appendEmptyModelRow(listEl, text) {
    const empty = document.createElement('div');
    empty.className = 'tb-model-row';
    empty.textContent = text;
    empty.style.color = 'var(--fg-muted)';
    empty.style.fontSize = '0.82em';
    empty.style.cursor = 'default';
    listEl.appendChild(empty);
}

// Group header row: the registered provider profile that serves the
// models below it (models without a provider tag belong to the
// implicit default profile).
function appendModelGroupHeader(listEl, name) {
    const header = document.createElement('div');
    header.className = 'tb-model-group';
    header.textContent = name || 'default';
    listEl.appendChild(header);
}

// selectorFor returns the selection value for a catalog row: the bare
// model id for the default profile, the qualified "provider/id" form for
// non-default groups. Model ids are not unique across providers, so the
// owning profile must travel with the selection (the server resolves the
// qualified form against the catalog).
function selectorFor(m) {
    const provider = m.provider || 'default';
    return (provider !== 'default') ? provider + '/' + m.id : m.id;
}

// buildModelRow renders one model row: description tooltip,
// context-limit chip, and the current-model check. onSelect receives
// (selector, active, event); the caller decides what selecting means.
function buildModelRow(m, current, onSelect) {
    // The catalog's `current` flag is per (provider, id) — it marks only
    // the copy the pane actually runs on. When the flag is absent (older
    // payloads, the subagent picker strips it), compare the row's own
    // selector against the passed current value.
    const active = m.current === true || (m.current == null && selectorFor(m) === current);
    const row = document.createElement('button');
    row.className = 'tb-model-row' + (active ? ' active' : '');
    row.type = 'button';
    // Hover tooltip: models.dev description of this model.
    if (m.description) row.title = m.description;
    const label = document.createElement('span');
    label.textContent = m.id;
    row.appendChild(label);
    if (m.contextLimit) {
        const limit = document.createElement('span');
        limit.className = 'tb-model-limit';
        limit.textContent = formatTokenCount(m.contextLimit);
        row.appendChild(limit);
    }
    if (active) {
        const check = document.createElement('span');
        check.className = 'tb-check';
        check.innerHTML = icon('check');
        row.appendChild(check);
    }
    row.addEventListener('click', (e) => {
        e.stopPropagation();
        onSelect(selectorFor(m), active, e);
    });
    return row;
}

// Shared model-list renderer: grouped by provider, search-filtered,
// with the current-model check. Used by the toolbar model popover
// (renderToolbarModelList in app.js) AND the settings subagent / board
// pickers (via createModelThinkingPicker) so all stay identical.
// extraRows (if any) are appended before the catalog rows (the picker's
// "Inherit" row).
export function renderModelList(listEl, query, filterLabel, models, current, onSelect, extraRows) {
    if (!listEl) return;
    listEl.innerHTML = '';
    for (const el of extraRows || []) listEl.appendChild(el);
    if (!models || models.length === 0) {
        appendEmptyModelRow(listEl, 'No models loaded');
        return;
    }
    const q = query;
    const filtered = q ? models.filter((m) => m.id.toLowerCase().includes(q)) : models;
    if (filtered.length === 0) {
        appendEmptyModelRow(listEl, `No models match "${filterLabel}"`);
        return;
    }
    // Group by provider: the server pushes models in profile order
    // (default first), so consecutive grouping matches the catalog.
    let lastProvider = null;
    for (const m of filtered) {
        const provider = m.provider || 'default';
        if (provider !== lastProvider) {
            appendModelGroupHeader(listEl, provider);
            lastProvider = provider;
        }
        listEl.appendChild(buildModelRow(m, current, onSelect));
    }
}

// ── Toolbar-style reasoning-effort chips ──
// The chips render the Off chip (the "no reasoning_effort sent" state,
// active when the level is off/empty) plus the model's accepted values.
// The element wiring and label rules live here; onSelect(value) is the
// caller's message channel (the toolbar sends set_thinking_level on the
// chat socket).
//
// cfg:
//   gridEl   — the chip container
//   sectionEl — optional wrapping section hidden when the model has no
//               reasoning-effort control at all
//   onSelect — (value) => send the selection ('off' = parameter omitted)
//
// Returns render(level, efforts, unsupported).
export function createThinkingChips({ gridEl, sectionEl, onSelect }) {
    return function render(level, efforts, unsupported) {
        if (!gridEl) return;
        gridEl.innerHTML = '';
        if (unsupported) {
            // The model definitively has no reasoning-effort control (a
            // known toggle-only models.dev entry, or a llama.cpp /props
            // probe that reported no support): hide the chips entirely —
            // there is nothing to select and nothing would be sent.
            if (sectionEl) sectionEl.hidden = true;
            return;
        }
        if (sectionEl) sectionEl.hidden = false;
        const off = !level || level === 'off';
        // Explicit Off chip: the "no reasoning_effort sent" state.
        // Active when the level is off/empty. A stored level the model
        // does not accept (policy B) renders as NO active chip — the
        // parameter is omitted, so the chips read the truth: nothing
        // selected = nothing sent.
        const offChip = document.createElement('button');
        offChip.className = 'tb-thinking-chip' + (off ? ' active' : '');
        offChip.type = 'button';
        offChip.textContent = thinkingLabel('off');
        offChip.title = off ? 'No reasoning_effort sent' : 'Click to disable reasoning (no reasoning_effort sent)';
        offChip.addEventListener('click', (e) => {
            e.stopPropagation();
            if (off) return; // already off
            onSelect('off');
        });
        gridEl.appendChild(offChip);
        const values = (Array.isArray(efforts) && efforts.length > 0) ? efforts : DEFAULT_THINKING_EFFORTS;
        for (const value of values) {
            if (value === 'off') continue; // the Off chip above owns that state
            const chip = document.createElement('button');
            const label = thinkingLabel(value);
            chip.className = 'tb-thinking-chip' + (value === level ? ' active' : '');
            chip.type = 'button';
            chip.textContent = label;
            chip.title = value;
            chip.addEventListener('click', (e) => {
                e.stopPropagation();
                // Toggle: clicking the active chip deselects (sends
                // 'off' → parameter omitted); clicking another chip
                // switches to that value.
                const next = value === level ? 'off' : value;
                onSelect(next);
            });
            gridEl.appendChild(chip);
        }
    };
}

// ── Shared model + reasoning-effort picker ──
// The Agent settings subagent picker and the Board "Start agent"
// popover offer the same two controls: a model list (the shared
// renderModelList, preceded by a leading "inherit"-style row for the
// empty value) and MODEL-AWARE reasoning-effort chips (the chosen
// model's catalog reasoningEfforts when a specific model is selected,
// the active pane's model values for the empty value — the session
// then runs that model — the default set when the catalog has no
// data). This factory unifies them so the chip building, the
// effort-option rules, and the "stored effort the new model no longer
// accepts → reset to inherit" guard live in exactly one place.
//
// cfg:
//   listEl, chipsEl — the model-list container and chip container
//   filterEl        — optional model filter input (read at render)
//   getState()      — current state object with `model` and
//                     `thinkingLevel` properties ('' = inherit /
//                     workspace default), or null while none is
//                     active; mutated in place
//   getModels()     — the model catalog array (may be empty while the
//                     list_models reply is in flight)
//   getPane()       — () => the active pane (its model's catalog values
//                     serve the empty-value row), or null
//   defaultRow      — { label, title } for the leading empty-value row
//   inheritChipTitle — tooltip of the leading "Inherit" effort chip
//   stripPaneCurrent — strip the catalog's `current` flag before
//                     rendering (it marks the PANE's model, right
//                     for the toolbar popover but wrong for a
//                     picker whose active state comes from the
//                     stored value alone)
//   paneCurrentRow  — while the model is the empty value, also
//                     highlight the pane's current model row (the
//                     board popover shows what its default row
//                     resolves to)
//   onModelChange(id)    — called after state.model is set
//   onThinkingChange(v)  — called after state.thinkingLevel is
//                     set, including '' from the reset guard
export function createModelThinkingPicker(cfg) {
    // The reasoning-effort values selectable for the state's current
    // model choice (see the section comment above).
    const effortOptions = (s) => {
        if (s.model) {
            const m = (cfg.getModels() || []).find((m) => m.id === s.model);
            if (m && Array.isArray(m.reasoningEfforts) && m.reasoningEfforts.length > 0) {
                return m.reasoningEfforts;
            }
        } else {
            const pane = cfg.getPane ? cfg.getPane() : null;
            if (pane && Array.isArray(pane.reasoningEfforts) && pane.reasoningEfforts.length > 0) {
                return pane.reasoningEfforts;
            }
        }
        return DEFAULT_THINKING_EFFORTS;
    };
    // A stored effort the (new) model does not accept would be
    // rejected or silently dropped at spawn/start — reset to
    // "Inherit" so the stored level always matches the options
    // shown (the server still validates against the final model
    // at spawn/start time as the backstop).
    const resetStaleThinking = (s) => {
        if (s.thinkingLevel && s.thinkingLevel !== 'off'
            && !effortOptions(s).includes(s.thinkingLevel)) {
            s.thinkingLevel = '';
            if (cfg.onThinkingChange) cfg.onThinkingChange('');
        }
    };
    const selectModel = (id) => {
        const s = cfg.getState();
        if (!s) return;
        s.model = id || '';
        resetStaleThinking(s);
        if (cfg.onModelChange) cfg.onModelChange(s.model);
        render();
    };
    const selectThinking = (value) => {
        const s = cfg.getState();
        if (!s) return;
        s.thinkingLevel = value || '';
        if (cfg.onThinkingChange) cfg.onThinkingChange(s.thinkingLevel);
        render();
    };
    const render = () => {
        const s = cfg.getState();
        if (!s) return;
        const models = cfg.getModels() || [];
        if (cfg.listEl) {
            const current = s.model || (cfg.paneCurrentRow
                ? (models.find((m) => m.current)?.id || '')
                : '');
            // The leading empty-value row, highlighted while the
            // stored model is ''.
            const defaultRow = document.createElement('button');
            defaultRow.type = 'button';
            defaultRow.className = 'tb-model-row' + (s.model === '' ? ' active' : '');
            defaultRow.textContent = cfg.defaultRow.label;
            if (cfg.defaultRow.title) defaultRow.title = cfg.defaultRow.title;
            if (s.model === '') {
                const check = document.createElement('span');
                check.className = 'tb-check';
                check.innerHTML = icon('check');
                defaultRow.appendChild(check);
            }
            defaultRow.addEventListener('click', (e) => {
                e.stopPropagation();
                if (s.model !== '') selectModel('');
            });
            // Strip the catalog's `current` flag when requested:
            // it marks the PANE's model (right for the toolbar
            // popover) but would pin the parent's model row as
            // active here — double-checking it next to the
            // default row and swallowing clicks on it (the
            // onSelect guard ignores active rows), so the
            // selection could never leave the default.
            const listModels = cfg.stripPaneCurrent
                ? models.map(({ current: _paneCurrent, ...rest }) => rest)
                : models;
            const query = cfg.filterEl ? cfg.filterEl.value.trim().toLowerCase() : '';
            renderModelList(cfg.listEl, query,
                cfg.filterEl ? cfg.filterEl.value : query,
                listModels, current, (id) => {
                    if (s.model !== id) selectModel(id);
                }, [defaultRow]);
        }
        if (cfg.chipsEl) {
            cfg.chipsEl.innerHTML = '';
            const chip = (value, label, title) => {
                const el = document.createElement('button');
                el.type = 'button';
                el.className = 'tb-thinking-chip' + (value === (s.thinkingLevel || '') ? ' active' : '');
                el.textContent = label;
                el.title = title;
                el.addEventListener('click', (e) => {
                    e.stopPropagation();
                    if (value !== (s.thinkingLevel || '')) selectThinking(value);
                });
                return el;
            };
            cfg.chipsEl.appendChild(chip('', 'Inherit', cfg.inheritChipTitle));
            cfg.chipsEl.appendChild(chip('off', 'Off', 'No reasoning_effort sent'));
            // Full labels (the toolbar chips' L/M/H short forms
            // are too cryptic for these settings contexts).
            for (const value of effortOptions(s)) {
                if (value === 'off') continue; // the Off chip owns that state
                cfg.chipsEl.appendChild(chip(value, thinkingFullLabel(value), value));
            }
        }
    };
    return {
        render,
        selectModel,
        selectThinking,
        // Re-validate a stored effort against a freshly arrived
        // catalog and re-render: a late list_models reply can
        // make a previously unknown model's accepted values
        // knowable, at which point a stale effort must reset.
        refresh: () => {
            const s = cfg.getState();
            if (!s) return;
            resetStaleThinking(s);
            render();
        },
    };
}
